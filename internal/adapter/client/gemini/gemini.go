package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	apiKeyEnv          = "GEMINI_API_KEY"
	baseURLEnv         = "GOOGLE_GEMINI_BASE_URL"
	authMechanismEnv   = "GEMINI_API_KEY_AUTH_MECHANISM"
	modelEnv           = "GEMINI_MODEL"
	cliHomeEnv         = "GEMINI_CLI_HOME"
	trustWorkspaceEnv  = "GEMINI_CLI_TRUST_WORKSPACE"
	telemetryEnv       = "GEMINI_TELEMETRY_ENABLED"
	settingsFileName   = "settings.json"
	persistentSlotsMsg = "Persistent model slots are supported only for the Claude client."
)

// settingsJSON is the exact, PMux-owned Gemini CLI settings document written
// into the isolated home directory before every launch.
const settingsJSON = `{"security":{"auth":{"selectedType":"gemini-api-key"}},"privacy":{"usageStatisticsEnabled":false},"telemetry":{"enabled":false}}`

var versionPattern = regexp.MustCompile(`(?m)(?:^|\s)v?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]+)?(?:\s|$)`)

// ModelPreflight verifies that an exact, case-sensitive model ID is currently
// available. The adapter never substitutes or normalizes the supplied ID.
type ModelPreflight func(context.Context, string) error

// Process is the complete, shell-free Gemini CLI process invocation.
type Process struct {
	Path   string
	Args   []string
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Runner is injectable so detection and launch can be tested without spawning
// a real Gemini CLI process.
type Runner interface {
	Output(context.Context, string, ...string) ([]byte, error)
	Run(context.Context, Process) error
}

type execRunner struct{}

func (execRunner) Output(ctx context.Context, path string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, path, args...).CombinedOutput()
}

func (execRunner) Run(ctx context.Context, process Process) error {
	cmd := exec.CommandContext(ctx, process.Path, process.Args...)
	cmd.Env = process.Env
	cmd.Dir = process.Dir
	cmd.Stdin = process.Stdin
	cmd.Stdout = process.Stdout
	cmd.Stderr = process.Stderr
	return cmd.Run()
}

// Options provides process and persistence boundaries. Zero values select the
// real process environment and standard streams. HomeDir is required: it is
// the PMux-owned isolated Gemini CLI settings directory.
type Options struct {
	Executable       string
	LookPath         func(string) (string, error)
	Runner           Runner
	Environment      func() []string
	WorkingDirectory func() (string, error)
	ModelPreflight   ModelPreflight
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	JSONMode         bool
	HomeDir          string
	Now              func() time.Time
}

type Launcher struct {
	executable       string
	lookPath         func(string) (string, error)
	runner           Runner
	environment      func() []string
	workingDirectory func() (string, error)
	preflight        ModelPreflight
	stdin            io.Reader
	stdout           io.Writer
	stderr           io.Writer
	jsonMode         bool
	homeDir          string
	now              func() time.Time
}

func New(options Options) *Launcher {
	lookPath := options.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	runner := options.Runner
	if runner == nil {
		runner = execRunner{}
	}
	environment := options.Environment
	if environment == nil {
		environment = os.Environ
	}
	workingDirectory := options.WorkingDirectory
	if workingDirectory == nil {
		workingDirectory = os.Getwd
	}
	stdin := options.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Launcher{
		executable: options.Executable, lookPath: lookPath, runner: runner,
		environment: environment, workingDirectory: workingDirectory,
		preflight: options.ModelPreflight, stdin: stdin, stdout: stdout,
		stderr: stderr, jsonMode: options.JSONMode, homeDir: options.HomeDir,
		now: now,
	}
}

var _ domainclient.ClientLauncher = (*Launcher)(nil)

func (l *Launcher) Client() domainclient.ClientID { return domainclient.Gemini }

func (l *Launcher) resolveExecutable() (string, error) {
	name := l.executable
	if name == "" {
		name = "gemini"
	}
	path, err := l.lookPath(name)
	if err != nil {
		return "", &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: "Gemini CLI was not found on PATH. Install Gemini CLI, then run \"pmux doctor\".",
			Cause:   err,
		}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.ClientBinaryMissing, pmuxerr.Environment, "Could not resolve the Gemini CLI executable path.")
	}
	return path, nil
}

func (l *Launcher) Detect(ctx context.Context) (domainclient.ClientInstall, error) {
	path, err := l.resolveExecutable()
	if err != nil {
		return domainclient.ClientInstall{}, err
	}
	output, err := l.runner.Output(ctx, path, "--version")
	if err != nil {
		return domainclient.ClientInstall{}, pmuxerr.Wrap(err, pmuxerr.ClientBinaryMissing, pmuxerr.Environment, "Could not determine the Gemini CLI version; PMux requires a verified semantic version.")
	}
	version, ok := parseVersion(string(output))
	if !ok {
		return domainclient.ClientInstall{Path: path, Version: "unknown", Supported: false}, &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: "Could not determine the Gemini CLI version; PMux requires a verified semantic version.",
		}
	}
	return domainclient.ClientInstall{Path: path, Version: version, Supported: true}, nil
}

func parseVersion(output string) (version string, ok bool) {
	match := versionPattern.FindStringSubmatch(output)
	if match == nil {
		return "", false
	}
	if _, err := strconv.Atoi(match[1]); err != nil {
		return "", false
	}
	return match[1] + "." + match[2] + "." + match[3], true
}

var conflictingGeminiEnv = map[string]struct{}{
	apiKeyEnv: {}, baseURLEnv: {}, "GOOGLE_VERTEX_BASE_URL": {},
	"GOOGLE_GENAI_USE_VERTEXAI": {}, "GOOGLE_GENAI_USE_GCA": {},
	modelEnv: {}, cliHomeEnv: {}, authMechanismEnv: {},
	trustWorkspaceEnv: {}, telemetryEnv: {}, "GEMINI_CLI_CUSTOM_HEADERS": {},
}

func (l *Launcher) home() (string, error) {
	if l.homeDir == "" {
		return "", pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "Gemini launcher requires a PMux-owned home directory.")
	}
	home, err := filepath.Abs(l.homeDir)
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not resolve the Gemini CLI home directory.")
	}
	return home, nil
}

func (l *Launcher) Env(spec domainclient.LaunchSpec) ([]string, error) {
	if spec.BaseURL == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Gemini launch requires a proxy base URL.")
	}
	if spec.Token == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Gemini launch requires a proxy authentication token.")
	}
	home, err := l.home()
	if err != nil {
		return nil, err
	}
	parent := l.environment()
	env := make([]string, 0, len(parent)+7)
	for _, value := range parent {
		name := value
		if index := strings.IndexByte(value, '='); index >= 0 {
			name = value[:index]
		}
		if _, remove := conflictingGeminiEnv[name]; !remove {
			env = append(env, value)
		}
	}
	env = append(env,
		apiKeyEnv+"="+spec.Token,
		baseURLEnv+"="+spec.BaseURL,
		authMechanismEnv+"=bearer",
		modelEnv+"="+spec.Model,
		cliHomeEnv+"="+home,
		trustWorkspaceEnv+"=true",
		telemetryEnv+"=false",
	)
	return env, nil
}

// ensureSettings guarantees the isolated Gemini CLI home contains exactly the
// PMux-owned settings document. Identical content is left untouched.
func (l *Launcher) ensureSettings(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not create the private Gemini CLI settings directory.")
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "Could not secure the Gemini CLI settings directory.")
	}
	path := filepath.Join(home, settingsFileName)
	current, err := os.ReadFile(path)
	if err == nil && string(current) == settingsJSON {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not read the Gemini CLI settings.")
	}
	if err := writeAtomic(path, []byte(settingsJSON), 0o600); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not write the Gemini CLI settings.")
	}
	return nil
}

func (l *Launcher) Launch(ctx context.Context, spec domainclient.LaunchSpec) (domainclient.LaunchResult, error) {
	if spec.Client != "" && spec.Client != domainclient.Gemini {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Gemini launcher received a different client ID.")
	}
	if spec.Model == "" {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Gemini launch requires an exact model ID.")
	}
	for _, arg := range spec.Args {
		if arg == "-m" || arg == "--model" || strings.HasPrefix(arg, "--model=") {
			return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Client arguments must not contain '-m' or '--model'; use 'pmux launch --model <id>'.")
		}
	}
	install, err := l.Detect(ctx)
	if err != nil {
		return domainclient.LaunchResult{}, err
	}
	if !install.Supported {
		return domainclient.LaunchResult{}, &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: fmt.Sprintf("Gemini CLI %s is unsupported; PMux requires a verified semantic version.", install.Version),
		}
	}
	if l.preflight == nil {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "Gemini model preflight is not configured.")
	}
	if err := l.preflight(ctx, spec.Model); err != nil {
		return domainclient.LaunchResult{}, err
	}
	home, err := l.home()
	if err != nil {
		return domainclient.LaunchResult{}, err
	}
	if err := l.ensureSettings(home); err != nil {
		return domainclient.LaunchResult{}, err
	}
	env, err := l.Env(spec)
	if err != nil {
		return domainclient.LaunchResult{}, err
	}
	dir := spec.WorkingDir
	if dir == "" {
		dir, err = l.workingDirectory()
		if err != nil {
			return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Could not determine the caller working directory.")
		}
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Could not resolve the caller working directory.")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Working directory is not accessible: "+dir+".")
	}
	stdout := l.stdout
	if l.jsonMode {
		stdout = l.stderr
	}
	args := make([]string, 0, 3+len(spec.Args))
	args = append(args, "--skip-trust", "-m", spec.Model)
	args = append(args, spec.Args...)
	err = l.runner.Run(ctx, Process{
		Path: install.Path, Args: args, Env: env, Dir: dir,
		Stdin: l.stdin, Stdout: stdout, Stderr: l.stderr,
	})
	if err == nil {
		return domainclient.LaunchResult{}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 1 && code <= 125 {
			return domainclient.LaunchResult{ExitCode: code}, nil
		}
		if result, ok := signalLaunchResult(exitErr.ProcessState); ok {
			return result, nil
		}
		return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ClientLaunchFailed, pmuxerr.Upstream, "Gemini CLI returned an unsupported exit status after launch.")
	}
	return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ClientLaunchFailed, pmuxerr.Environment, "Gemini CLI could not be started after successful preflight.")
}

// PlanPersist always fails: persistent model slots are a Claude-only feature.
func (l *Launcher) PlanPersist(context.Context, domainclient.PersistSpec) (domainclient.PersistPlan, error) {
	return domainclient.PersistPlan{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, persistentSlotsMsg)
}

// Upsert always fails: persistent model slots are a Claude-only feature.
func (l *Launcher) Upsert(context.Context, domainclient.PersistPlan) error {
	return pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, persistentSlotsMsg)
}

// Unpersist always fails: persistent model slots are a Claude-only feature.
func (l *Launcher) Unpersist(context.Context) error {
	return pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, persistentSlotsMsg)
}

func writeAtomic(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".pmux-gemini-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	if os.PathSeparator == '\\' {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

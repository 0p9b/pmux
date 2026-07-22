package codex

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
	apiKeyEnv = "OPENAI_API_KEY"

	persistUnsupportedMessage = "Persistent model slots are supported only for the Claude client."
)

var versionPattern = regexp.MustCompile(`codex-cli\s+v?(\d+)\.(\d+)\.(\d+)`)

// ModelPreflight verifies that an exact, case-sensitive model ID is currently
// available. The adapter never substitutes or normalizes the supplied ID. A
// nil preflight disables the check; Codex model availability is enforced by
// the proxied provider at request time.
type ModelPreflight func(context.Context, string) error

// Process is the complete, shell-free Codex process invocation.
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
// a real Codex process.
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

// Options provides process boundaries. Zero values select the real process
// environment and standard streams.
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
		stderr: stderr, jsonMode: options.JSONMode, now: now,
	}
}

var _ domainclient.ClientLauncher = (*Launcher)(nil)

func (l *Launcher) Client() domainclient.ClientID { return domainclient.Codex }

func (l *Launcher) resolveExecutable() (string, error) {
	name := l.executable
	if name == "" {
		name = "codex"
	}
	path, err := l.lookPath(name)
	if err != nil {
		return "", &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: "Codex CLI was not found on PATH. Install Codex CLI (for example \"npm install -g @openai/codex\"), then run \"pmux doctor\".",
			Cause:   err,
		}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.ClientBinaryMissing, pmuxerr.Environment, "Could not resolve the Codex CLI executable path.")
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
		return domainclient.ClientInstall{}, pmuxerr.Wrap(err, pmuxerr.ClientBinaryMissing, pmuxerr.Environment, "Could not determine the Codex CLI version; PMux requires a verified codex-cli version.")
	}
	version, ok := parseVersion(string(output))
	if !ok {
		return domainclient.ClientInstall{Path: path, Version: "unknown", Supported: false}, &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: "Could not determine the Codex CLI version; PMux requires a verified codex-cli version.",
		}
	}
	return domainclient.ClientInstall{Path: path, Version: version, Supported: true}, nil
}

func parseVersion(output string) (version string, ok bool) {
	match := versionPattern.FindStringSubmatch(output)
	if match == nil {
		return "", false
	}
	for _, part := range match[1:] {
		if _, err := strconv.Atoi(part); err != nil {
			return "", false
		}
	}
	return match[1] + "." + match[2] + "." + match[3], true
}

var conflictingCodexEnv = map[string]struct{}{
	apiKeyEnv: {}, "CODEX_API_KEY": {}, "OPENAI_BASE_URL": {},
	"CODEX_HOME": {}, "CODEX_MODEL": {},
}

func (l *Launcher) Env(spec domainclient.LaunchSpec) ([]string, error) {
	if spec.BaseURL == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Codex launch requires a proxy base URL.")
	}
	if spec.Token == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Codex launch requires a proxy authentication token.")
	}
	parent := l.environment()
	env := make([]string, 0, len(parent)+1)
	for _, value := range parent {
		name := value
		if index := strings.IndexByte(value, '='); index >= 0 {
			name = value[:index]
		}
		if _, remove := conflictingCodexEnv[name]; !remove {
			env = append(env, value)
		}
	}
	env = append(env, apiKeyEnv+"="+spec.Token)
	return env, nil
}

func (l *Launcher) Launch(ctx context.Context, spec domainclient.LaunchSpec) (domainclient.LaunchResult, error) {
	if spec.Client != "" && spec.Client != domainclient.Codex {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Codex launcher received a different client ID.")
	}
	if spec.Model == "" {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Codex launch requires an exact model ID.")
	}
	for _, arg := range spec.Args {
		if arg == "-m" || arg == "--model" || strings.HasPrefix(arg, "--model=") {
			return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Client arguments must not contain '-m' or '--model'; use 'pmux launch --model <id>'.")
		}
		if arg == "-c" || arg == "--config" || strings.HasPrefix(arg, "--config=") {
			return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Client arguments must not contain '-c' or '--config'; model and provider config come from pmux flags.")
		}
	}
	install, err := l.Detect(ctx)
	if err != nil {
		return domainclient.LaunchResult{}, err
	}
	if !install.Supported {
		return domainclient.LaunchResult{}, &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: fmt.Sprintf("Codex CLI %s is unsupported; PMux requires a verified codex-cli version.", install.Version),
		}
	}
	if l.preflight != nil {
		if err := l.preflight(ctx, spec.Model); err != nil {
			return domainclient.LaunchResult{}, err
		}
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
	providerConfig := fmt.Sprintf(`model_providers.pmux={ name = "pmux", base_url = "%s/v1", env_key = "OPENAI_API_KEY", wire_api = "responses" }`, spec.BaseURL)
	args := make([]string, 0, 6+len(spec.Args))
	args = append(args, "-m", spec.Model, "-c", providerConfig, "-c", `model_provider="pmux"`)
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
		return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ClientLaunchFailed, pmuxerr.Upstream, "Codex CLI returned an unsupported exit status after launch.")
	}
	return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ClientLaunchFailed, pmuxerr.Environment, "Codex CLI could not be started after successful preflight.")
}

// PlanPersist fails closed: Codex has no independently managed persistent
// model slots, so PMux never mutates Codex configuration.
func (l *Launcher) PlanPersist(context.Context, domainclient.PersistSpec) (domainclient.PersistPlan, error) {
	return domainclient.PersistPlan{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, persistUnsupportedMessage)
}

// Upsert fails closed for the same reason as PlanPersist.
func (l *Launcher) Upsert(context.Context, domainclient.PersistPlan) error {
	return pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, persistUnsupportedMessage)
}

// Unpersist fails closed for the same reason as PlanPersist.
func (l *Launcher) Unpersist(context.Context) error {
	return pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, persistUnsupportedMessage)
}

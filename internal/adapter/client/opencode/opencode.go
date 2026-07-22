package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	configContentEnv        = "OPENCODE_CONFIG_CONTENT"
	disableProjectConfigEnv = "OPENCODE_DISABLE_PROJECT_CONFIG"
	providerID              = "pmux"
)

var versionPattern = regexp.MustCompile(`(?m)^\s*v?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]+)?\s*$`)

// Process is the complete, shell-free OpenCode process invocation.
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
// a real OpenCode process.
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

// Options provides process and environment boundaries. Zero values select the
// real process environment and standard streams.
type Options struct {
	Executable       string
	LookPath         func(string) (string, error)
	Runner           Runner
	Environment      func() []string
	WorkingDirectory func() (string, error)
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
		stdin: stdin, stdout: stdout, stderr: stderr,
		jsonMode: options.JSONMode, now: now,
	}
}

var _ domainclient.ClientLauncher = (*Launcher)(nil)

func (l *Launcher) Client() domainclient.ClientID { return domainclient.OpenCode }

func (l *Launcher) resolveExecutable() (string, error) {
	name := l.executable
	if name == "" {
		name = "opencode"
	}
	path, err := l.lookPath(name)
	if err != nil {
		return "", &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: "OpenCode was not found on PATH. Install OpenCode, then run \"pmux doctor\".",
			Cause:   err,
		}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.ClientBinaryMissing, pmuxerr.Environment, "Could not resolve the OpenCode executable path.")
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
		return domainclient.ClientInstall{}, pmuxerr.Wrap(err, pmuxerr.ClientBinaryMissing, pmuxerr.Environment, "Could not determine the OpenCode version.")
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return domainclient.ClientInstall{Path: path, Version: "unknown", Supported: false}, &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: "Could not determine the OpenCode version; \"opencode --version\" printed nothing.",
		}
	}
	// Dev builds print "local" instead of a semantic version; any non-empty
	// identifier marks a supported installation.
	if version, ok := parseVersion(trimmed); ok {
		return domainclient.ClientInstall{Path: path, Version: version, Supported: true}, nil
	}
	return domainclient.ClientInstall{Path: path, Version: trimmed, Supported: true}, nil
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

var conflictingOpenCodeEnv = map[string]struct{}{
	"OPENCODE_CONFIG": {}, configContentEnv: {}, "OPENCODE_CONFIG_DIR": {},
	disableProjectConfigEnv: {}, "OPENCODE_PURE": {},
}

// proxyConfig is the deterministic, compactly marshaled OpenCode
// configuration handed to the process through OPENCODE_CONFIG_CONTENT. The
// apiKey stays process-scoped in the environment: it never appears in argv,
// logs, or JSON output.
type proxyConfig struct {
	Schema   string              `json:"$schema"`
	Model    string              `json:"model"`
	Provider map[string]provider `json:"provider"`
}

type provider struct {
	NPM     string                   `json:"npm"`
	Name    string                   `json:"name"`
	Options providerOptions          `json:"options"`
	Models  map[string]providerModel `json:"models"`
}

type providerOptions struct {
	BaseURL string `json:"baseURL"`
	APIKey  string `json:"apiKey"`
}

type providerModel struct {
	Name string `json:"name"`
}

func marshalConfig(spec domainclient.LaunchSpec) ([]byte, error) {
	config := proxyConfig{
		Schema: "https://opencode.ai/config.json",
		Model:  providerID + "/" + spec.Model,
		Provider: map[string]provider{
			providerID: {
				NPM:  "@ai-sdk/openai-compatible",
				Name: "PMux (CLIProxyAPI)",
				Options: providerOptions{
					BaseURL: spec.BaseURL + "/v1",
					APIKey:  spec.Token,
				},
				Models: map[string]providerModel{
					spec.Model: {Name: spec.Model},
				},
			},
		},
	}
	body, err := json.Marshal(config)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not encode the OpenCode provider configuration.")
	}
	return body, nil
}

func (l *Launcher) Env(spec domainclient.LaunchSpec) ([]string, error) {
	if spec.BaseURL == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "OpenCode launch requires a proxy base URL.")
	}
	if spec.Token == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "OpenCode launch requires a proxy authentication token.")
	}
	config, err := marshalConfig(spec)
	if err != nil {
		return nil, err
	}
	parent := l.environment()
	env := make([]string, 0, len(parent)+2)
	for _, value := range parent {
		name := value
		if index := strings.IndexByte(value, '='); index >= 0 {
			name = value[:index]
		}
		if _, remove := conflictingOpenCodeEnv[name]; !remove {
			env = append(env, value)
		}
	}
	env = append(env, configContentEnv+"="+string(config), disableProjectConfigEnv+"=1")
	return env, nil
}

func (l *Launcher) Launch(ctx context.Context, spec domainclient.LaunchSpec) (domainclient.LaunchResult, error) {
	if spec.Client != "" && spec.Client != domainclient.OpenCode {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "OpenCode launcher received a different client ID.")
	}
	if spec.Model == "" {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "OpenCode launch requires an exact model ID.")
	}
	for _, arg := range spec.Args {
		if arg == "-m" || arg == "--model" || strings.HasPrefix(arg, "--model=") {
			return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Client arguments must not contain a model flag; use 'pmux launch --model <id>'.")
		}
	}
	install, err := l.Detect(ctx)
	if err != nil {
		return domainclient.LaunchResult{}, err
	}
	if !install.Supported {
		return domainclient.LaunchResult{}, &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: fmt.Sprintf("OpenCode %s is unsupported; PMux requires a working OpenCode installation.", install.Version),
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
	// The model travels in OPENCODE_CONFIG_CONTENT, never in argv.
	args := append([]string(nil), spec.Args...)
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
		return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ClientLaunchFailed, pmuxerr.Upstream, "OpenCode returned an unsupported exit status after launch.")
	}
	return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ClientLaunchFailed, pmuxerr.Environment, "OpenCode could not be started after successful preflight.")
}

// signalLaunchResult converts a signal-terminated process state into the
// portable 128+signal launch result. On platforms without signal-aware wait
// status (Windows) the assertion simply fails and no result is produced.
func signalLaunchResult(state *os.ProcessState) (domainclient.LaunchResult, bool) {
	if state == nil {
		return domainclient.LaunchResult{}, false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return domainclient.LaunchResult{}, false
	}
	signal := status.Signal()
	return domainclient.LaunchResult{ExitCode: 128 + int(signal), Signal: signal.String()}, true
}

// PlanPersist always fails: OpenCode receives its proxy configuration per
// launch through OPENCODE_CONFIG_CONTENT, so there is nothing to persist.
func (l *Launcher) PlanPersist(context.Context, domainclient.PersistSpec) (domainclient.PersistPlan, error) {
	return domainclient.PersistPlan{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Persistent model slots are supported only for the Claude client.")
}

func (l *Launcher) Upsert(context.Context, domainclient.PersistPlan) error {
	return pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Persistent model slots are supported only for the Claude client.")
}

func (l *Launcher) Unpersist(context.Context) error {
	return pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Persistent model slots are supported only for the Claude client.")
}

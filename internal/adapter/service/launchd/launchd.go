package launchd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/0p9b/pmux/internal/adapter/service/foreground"
	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	ownerEnvironmentKey    = "PMUX_SERVICE_OWNER"
	ownerEnvironmentVal    = "pmux"
	instanceEnvironmentKey = "PMUX_SERVICE_INSTANCE"
)

var (
	instancePattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	terminalEscapePattern  = regexp.MustCompile(`(?:\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]|\x1b[@-_]|\x{009b}[0-?]*[ -/]*[@-~])`)
)

// Runner is the narrow process boundary used for launchctl and log following.
// Implementations must execute name and args directly, without a shell.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error)
}

type Config struct {
	InstanceID string
	PlistDir   string
	UID        int
	Runner     Runner
	Health     health.Checker
}

type Manager struct {
	instanceID string
	label      string
	plistPath  string
	uid        int
	runner     Runner
	health     health.Checker
}

var _ service.ServiceManager = (*Manager)(nil)

func New(cfg Config) (*Manager, error) {
	if !instancePattern.MatchString(cfg.InstanceID) {
		return nil, invalid("instance ID must contain only letters, digits, '.', '_', or '-' and must start with a letter or digit")
	}
	if cfg.UID < 0 {
		return nil, invalid("launchd UID must not be negative")
	}
	if cfg.PlistDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not resolve the LaunchAgents directory")
		}
		cfg.PlistDir = filepath.Join(home, "Library", "LaunchAgents")
	}
	if !filepath.IsAbs(cfg.PlistDir) {
		return nil, invalid("LaunchAgents directory must be absolute")
	}
	if cfg.Runner == nil {
		cfg.Runner = execRunner{}
	}
	if cfg.Health == nil {
		return nil, invalid("launchd service requires a health verifier")
	}
	label := service.Identity(service.BackendLaunchd, cfg.InstanceID)
	return &Manager{
		instanceID: cfg.InstanceID,
		label:      label,
		plistPath:  filepath.Join(filepath.Clean(cfg.PlistDir), label+".plist"),
		uid:        cfg.UID,
		runner:     cfg.Runner,
		health:     cfg.Health,
	}, nil
}

func (m *Manager) Backend() service.ServiceBackend { return service.BackendLaunchd }

func (m *Manager) Detect(ctx context.Context) (service.ServiceStatus, error) {
	return m.Status(ctx)
}

func (m *Manager) Install(_ context.Context, spec service.ServiceSpec) error {
	if err := m.validateSpec(spec); err != nil {
		return err
	}
	for _, path := range []string{spec.RuntimeDir, spec.LogDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create a private LaunchAgent directory")
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not secure a LaunchAgent directory")
		}
	}
	body, err := renderPlist(spec, m.label)
	if err != nil {
		return wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.User, "could not render the LaunchAgent definition")
	}
	if existing, readErr := os.ReadFile(m.plistPath); readErr == nil {
		owned, ownErr := isOwnedPlist(existing, m.label, m.instanceID)
		if ownErr != nil || !owned {
			return ownership("refusing to overwrite a foreign LaunchAgent at " + m.plistPath)
		}
		if bytes.Equal(existing, body) {
			return nil
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return wrap(readErr, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not inspect the LaunchAgent definition")
	}
	if err := atomicWrite(m.plistPath, body, 0o600); err != nil {
		return wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not install the LaunchAgent definition")
	}
	return nil
}

func (m *Manager) Uninstall(ctx context.Context) error {
	body, err := os.ReadFile(m.plistPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not inspect the LaunchAgent definition")
	}
	owned, parseErr := isOwnedPlist(body, m.label, m.instanceID)
	if parseErr != nil || !owned {
		return ownership("refusing to remove a foreign LaunchAgent at " + m.plistPath)
	}
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if status.State == service.ServiceRunning {
		if _, err := m.runner.Run(ctx, "/bin/launchctl", "bootout", m.target()); err != nil {
			return launchctlError(err, "could not stop the LaunchAgent before uninstalling it")
		}
	}
	if err := os.Remove(m.plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not remove the LaunchAgent definition")
	}
	if err := syncDir(filepath.Dir(m.plistPath)); err != nil {
		return wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not durably remove the LaunchAgent definition")
	}
	return nil
}

func (m *Manager) Start(ctx context.Context) error {
	if _, err := m.ownedDefinition(); err != nil {
		return err
	}
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if status.State != service.ServiceRunning {
		if _, err := m.runner.Run(ctx, "/bin/launchctl", "bootstrap", m.domain(), m.plistPath); err != nil {
			return launchctlError(err, "could not bootstrap the LaunchAgent")
		}
	}
	if _, err := m.health.WaitReady(ctx); err != nil {
		return wrap(err, pmuxerr.ServiceHealthDeadline, pmuxerr.Environment, "CLIProxyAPI did not become healthy after starting the LaunchAgent")
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		return invalid("stop timeout must be greater than zero")
	}
	if _, err := m.ownedDefinition(); err != nil {
		return err
	}
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if status.State == service.ServiceStopped || status.State == service.ServiceNotInstalled {
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := m.runner.Run(stopCtx, "/bin/launchctl", "bootout", m.target()); err != nil {
		return launchctlError(err, "could not stop the LaunchAgent")
	}
	return nil
}

func (m *Manager) Restart(ctx context.Context) (service.ServiceStatus, error) {
	if _, err := m.ownedDefinition(); err != nil {
		return service.ServiceStatus{}, err
	}
	status, err := m.Status(ctx)
	if err != nil {
		return service.ServiceStatus{}, err
	}
	if status.State == service.ServiceRunning {
		if _, err := m.runner.Run(ctx, "/bin/launchctl", "kickstart", "-k", m.target()); err != nil {
			return service.ServiceStatus{}, launchctlError(err, "could not restart the LaunchAgent")
		}
	} else if _, err := m.runner.Run(ctx, "/bin/launchctl", "bootstrap", m.domain(), m.plistPath); err != nil {
		return service.ServiceStatus{}, launchctlError(err, "could not bootstrap the LaunchAgent")
	}
	healthResult, err := m.health.WaitReady(ctx)
	if err != nil {
		return service.ServiceStatus{}, wrap(err, pmuxerr.ServiceHealthDeadline, pmuxerr.Environment, "CLIProxyAPI did not become healthy after restarting the LaunchAgent")
	}
	if healthResult.Version == "" {
		healthResult.Version = health.UnknownVersion
		if healthResult.Warning == "" {
			healthResult.Warning = health.UnknownVersionWarning
		}
	}
	return service.ServiceStatus{
		Backend: service.BackendLaunchd, State: service.ServiceRunning,
		Healthy: true, CoreVersion: healthResult.Version, Warning: healthResult.Warning,
	}, nil
}

func (m *Manager) Status(ctx context.Context) (service.ServiceStatus, error) {
	if _, err := os.Stat(m.plistPath); errors.Is(err, os.ErrNotExist) {
		return service.ServiceStatus{Backend: service.BackendLaunchd, State: service.ServiceNotInstalled, CoreVersion: health.UnknownVersion}, nil
	} else if err != nil {
		return service.ServiceStatus{}, wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not inspect the LaunchAgent definition")
	}
	if _, err := m.ownedDefinition(); err != nil {
		return service.ServiceStatus{}, err
	}
	out, err := m.runner.Run(ctx, "/bin/launchctl", "print", m.target())
	if err != nil {
		return service.ServiceStatus{Backend: service.BackendLaunchd, State: service.ServiceStopped, CoreVersion: health.UnknownVersion}, nil
	}
	return service.ServiceStatus{
		Backend: service.BackendLaunchd, State: service.ServiceRunning,
		PID: parsePID(out), Detail: "loaded", CoreVersion: health.UnknownVersion,
	}, nil
}

func (m *Manager) Logs(ctx context.Context, tail int, follow bool) (io.ReadCloser, error) {
	if tail < 0 {
		return nil, invalid("log tail must not be negative")
	}
	body, err := m.ownedDefinition()
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := plistLogPaths(body)
	if err != nil {
		return nil, wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "could not read LaunchAgent log paths")
	}
	args := []string{"-n", strconv.Itoa(tail)}
	if follow {
		args = append(args, "-F")
	}
	args = append(args, stdout, stderr)
	reader, err := m.runner.Stream(ctx, "/usr/bin/tail", args...)
	if err != nil {
		return nil, wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not read LaunchAgent logs")
	}
	return newRedactingReader(reader), nil
}

const maxLogLineBytes = 1024 * 1024

// newRedactingReader keeps launchd's tail stream bounded and applies the same
// secret boundary as foreground and systemd before any bytes reach callers.
func newRedactingReader(source io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	result := &redactingReader{reader: reader, source: source}
	go func() {
		defer func() { _ = result.closeSource() }()
		scanner := bufio.NewScanner(source)
		scanner.Buffer(make([]byte, 4096), maxLogLineBytes)
		for scanner.Scan() {
			line := sanitizeLogControls(scanner.Text())
			if _, err := io.WriteString(writer, foreground.RedactLogText(line)+"\n"); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		}
		_ = writer.CloseWithError(scanner.Err())
	}()
	return result
}

func sanitizeLogControls(text string) string {
	text = terminalEscapePattern.ReplaceAllString(text, "")
	var sanitized strings.Builder
	sanitized.Grow(len(text))
	for _, character := range text {
		if unicode.IsPrint(character) {
			sanitized.WriteRune(character)
		}
	}
	return sanitized.String()
}

type redactingReader struct {
	reader    *io.PipeReader
	source    io.Closer
	closeOnce sync.Once
	closeErr  error
}

func (r *redactingReader) Read(payload []byte) (int, error) {
	return r.reader.Read(payload)
}

func (r *redactingReader) Close() error {
	readerErr := r.reader.Close()
	sourceErr := r.closeSource()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

func (r *redactingReader) closeSource() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.source.Close()
	})
	return r.closeErr
}

func (m *Manager) validateSpec(spec service.ServiceSpec) error {
	if spec.InstanceID != m.instanceID || spec.Identity != m.label {
		return invalid("service identity must be " + m.label)
	}
	for name, value := range map[string]string{
		"PMux executable": spec.PMuxPath, "CLIProxyAPI executable": spec.BinaryPath,
		"config": spec.ConfigPath, "runtime directory": spec.RuntimeDir, "log directory": spec.LogDir,
	} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return invalid(name + " path must be absolute and clean")
		}
	}
	if filepath.Base(spec.RuntimeDir) == ".env" {
		return invalid("runtime directory must not be an .env path")
	}
	if _, err := os.Stat(filepath.Join(spec.RuntimeDir, ".env")); err == nil {
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "runtime directory contains .env; refusing to install the LaunchAgent")
	} else if !errors.Is(err, os.ErrNotExist) {
		return wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not verify the runtime directory")
	}
	if _, err := environmentMap(spec.Environment); err != nil {
		return err
	}
	return nil
}

func (m *Manager) target() string { return m.domain() + "/" + m.label }
func (m *Manager) domain() string { return "gui/" + strconv.Itoa(m.uid) }

func (m *Manager) ownedDefinition() ([]byte, error) {
	body, err := os.ReadFile(m.plistPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "LaunchAgent is not installed")
	}
	if err != nil {
		return nil, wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not read the LaunchAgent definition")
	}
	owned, parseErr := isOwnedPlist(body, m.label, m.instanceID)
	if parseErr != nil || !owned {
		return nil, ownership("LaunchAgent is not owned by PMux: " + m.plistPath)
	}
	return body, nil
}

func renderPlist(spec service.ServiceSpec, label string) ([]byte, error) {
	env, err := environmentMap(spec.Environment)
	if err != nil {
		return nil, err
	}
	env[ownerEnvironmentKey] = ownerEnvironmentVal
	env[instanceEnvironmentKey] = spec.InstanceID
	values := []plistEntry{
		{Key: "Label", String: label},
		{Key: "ProgramArguments", Array: []string{filepath.ToSlash(spec.PMuxPath), "--binary", filepath.ToSlash(spec.BinaryPath), "--config", filepath.ToSlash(spec.ConfigPath)}},
		{Key: "WorkingDirectory", String: filepath.ToSlash(spec.RuntimeDir)},
		{Key: "EnvironmentVariables", Dict: env},
		{Key: "RunAtLoad", Bool: new(true)},
		{Key: "KeepAlive", BoolDict: map[string]bool{"SuccessfulExit": false}},
		{Key: "StandardOutPath", String: filepath.ToSlash(filepath.Join(spec.LogDir, spec.InstanceID+".out.log"))},
		{Key: "StandardErrorPath", String: filepath.ToSlash(filepath.Join(spec.LogDir, spec.InstanceID+".err.log"))},
	}
	var out bytes.Buffer
	out.WriteString(xml.Header)
	out.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	enc := xml.NewEncoder(&out)
	enc.Indent("", "  ")
	if err := enc.Encode(plistDocument{Version: "1.0", Entries: values}); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func environmentMap(items []string) (map[string]string, error) {
	out := make(map[string]string, len(items)+2)
	for _, item := range items {
		name, value, ok := strings.Cut(item, "=")
		if !ok || !environmentNamePattern.MatchString(name) || !validXMLText(value) {
			return nil, invalid("service environment entries must use NAME=VALUE with a valid environment name and XML-safe value")
		}
		if name == ownerEnvironmentKey || name == instanceEnvironmentKey {
			return nil, invalid("service environment must not override PMux ownership markers")
		}
		if name == "MANAGEMENT_PASSWORD" || strings.HasPrefix(name, "PGSTORE_") ||
			strings.HasPrefix(name, "OBJECTSTORE_") || strings.HasPrefix(name, "GITSTORE_") {
			return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "service environment contains a forbidden configuration override: "+name)
		}
		if _, exists := out[name]; exists {
			return nil, invalid("service environment contains duplicate variable " + name)
		}
		out[name] = value
	}
	return out, nil
}
func validXMLText(value string) bool {
	for _, r := range value {
		if r == 0 || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') {
			return false
		}
	}
	return true
}

func parsePID(out []byte) int {
	re := regexp.MustCompile(`(?m)^\s*pid\s*=\s*([0-9]+)\s*$`)
	match := re.FindSubmatch(out)
	if len(match) != 2 {
		return 0
	}
	pid, _ := strconv.Atoi(string(match[1]))
	return pid
}

func launchctlError(err error, message string) error {
	return wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, message)
}

func invalid(message string) *pmuxerr.Error {
	return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, message)
}

func ownership(message string) *pmuxerr.Error {
	return pmuxerr.New(pmuxerr.ServiceForeignOwner, pmuxerr.Environment, message)
}

func wrap(err error, code string, class pmuxerr.Class, message string) *pmuxerr.Error {
	return pmuxerr.Wrap(err, code, class, message)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func (execRunner) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &commandReader{ReadCloser: stdout, cmd: cmd}, nil
}

type commandReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (r *commandReader) Close() error {
	closeErr := r.ReadCloser.Close()
	killErr := r.cmd.Process.Kill()
	_ = r.cmd.Wait()
	if closeErr != nil {
		return closeErr
	}
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return killErr
	}
	return nil
}

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".pmux-launchd-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(tmp)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	keep = true
	return syncDir(dir)
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (m *Manager) String() string {
	return fmt.Sprintf("launchd %s (%s)", m.label, m.plistPath)
}

package systemd

import (
	"bufio"
	"bytes"
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
	"sync"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/foreground"
	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const OwnershipMarker = "# Managed by PMux"

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type Runner interface {
	Run(ctx context.Context, executable string, args ...string) ([]byte, error)
	Stream(ctx context.Context, executable string, args ...string) (io.ReadCloser, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, executable, args...).CombinedOutput()
}

func (OSRunner) Stream(ctx context.Context, executable string, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
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
	cmd  *exec.Cmd
	once sync.Once
}

func (r *commandReader) Close() error {
	var closeErr error
	r.once.Do(func() {
		closeErr = r.ReadCloser.Close()
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		_ = r.cmd.Wait()
	})
	return closeErr
}

type Manager struct {
	mu         sync.Mutex
	instanceID string
	unitDir    string
	runner     Runner
	health     health.Checker
	lastHealth health.Result
}

var _ service.ServiceManager = (*Manager)(nil)

func New(instanceID, unitDir string, runner Runner, checker health.Checker) *Manager {
	if runner == nil {
		runner = OSRunner{}
	}
	return &Manager{instanceID: instanceID, unitDir: unitDir, runner: runner, health: checker}
}

func (m *Manager) Backend() service.ServiceBackend { return service.BackendSystemdUser }

func (m *Manager) identity() string {
	return service.Identity(service.BackendSystemdUser, m.instanceID)
}
func (m *Manager) unitPath() string { return filepath.Join(m.unitDir, m.identity()) }

func (m *Manager) Detect(ctx context.Context) (service.ServiceStatus, error) {
	if _, err := os.Stat(m.unitPath()); errors.Is(err, os.ErrNotExist) {
		return service.ServiceStatus{Backend: service.BackendSystemdUser, State: service.ServiceNotInstalled, CoreVersion: health.UnknownVersion}, nil
	} else if err != nil {
		return service.ServiceStatus{}, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not inspect systemd user service definition")
	}
	return m.Status(ctx)
}

func (m *Manager) Install(ctx context.Context, spec service.ServiceSpec) error {
	if spec.InstanceID != m.instanceID {
		return foreignError("the requested instance does not match this systemd manager")
	}
	if spec.Identity == "" {
		spec.Identity = m.identity()
	}
	if spec.Identity != m.identity() {
		return foreignError(fmt.Sprintf("service identity %q is not canonical %q", spec.Identity, m.identity()))
	}
	if err := validateSpec(spec); err != nil {
		return err
	}
	unit, err := RenderUnit(spec)
	if err != nil {
		return err
	}

	path := m.unitPath()
	var previous []byte
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !Owned(existing, m.instanceID) {
			return foreignError("the canonical unit path exists without PMux's matching ownership marker")
		}
		previous = existing
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return pmuxerr.Wrap(readErr, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not read systemd user service definition")
	}
	if err := os.MkdirAll(m.unitDir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not create systemd user unit directory")
	}
	if err := atomicWrite(path, unit, 0o600); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not install systemd user service definition")
	}
	if _, err := m.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		if previous == nil {
			_ = os.Remove(path)
		} else {
			_ = atomicWrite(path, previous, 0o600)
		}
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "systemd user manager rejected the PMux service definition")
	}
	if _, err := m.runner.Run(ctx, "systemctl", "--user", "enable", m.identity()); err != nil {
		if previous == nil {
			_ = os.Remove(path)
		} else {
			_ = atomicWrite(path, previous, 0o600)
		}
		_, _ = m.runner.Run(ctx, "systemctl", "--user", "daemon-reload")
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "systemd could not enable the PMux user service")
	}
	return nil
}

func (m *Manager) Uninstall(ctx context.Context) error {
	path := m.unitPath()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not read systemd user service definition")
	}
	if !Owned(body, m.instanceID) {
		return foreignError("the canonical unit path is not owned by this PMux instance")
	}
	if err := m.Stop(ctx, 5*time.Second); err != nil {
		return err
	}
	if _, err := m.runner.Run(ctx, "systemctl", "--user", "disable", m.identity()); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "systemd could not disable the PMux user service")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not remove systemd user service definition")
	}
	if _, err := m.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not reload systemd user manager after uninstall")
	}
	return nil
}

func (m *Manager) Start(ctx context.Context) error {
	if err := m.requireOwned(); err != nil {
		return err
	}
	if m.health == nil {
		return pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Internal, "systemd health checker is not configured")
	}
	if _, err := m.runner.Run(ctx, "systemctl", "--user", "start", m.identity()); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "systemd could not start CLIProxyAPI")
	}
	result, err := m.health.WaitReady(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.lastHealth = result
	m.mu.Unlock()
	return nil
}

func (m *Manager) Stop(ctx context.Context, timeout time.Duration) error {
	if _, err := os.Stat(m.unitPath()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := m.requireOwned(); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := m.runner.Run(stopCtx, "systemctl", "--user", "stop", m.identity()); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "systemd could not stop CLIProxyAPI")
	}
	m.mu.Lock()
	m.lastHealth = health.Result{}
	m.mu.Unlock()
	return nil
}

func (m *Manager) Restart(ctx context.Context) (service.ServiceStatus, error) {
	if err := m.requireOwned(); err != nil {
		return service.ServiceStatus{}, err
	}
	if m.health == nil {
		return service.ServiceStatus{}, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Internal, "systemd health checker is not configured")
	}
	if _, err := m.runner.Run(ctx, "systemctl", "--user", "restart", m.identity()); err != nil {
		return service.ServiceStatus{}, pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "systemd could not restart CLIProxyAPI")
	}
	result, err := m.health.WaitReady(ctx)
	if err != nil {
		return service.ServiceStatus{}, err
	}
	m.mu.Lock()
	m.lastHealth = result
	m.mu.Unlock()
	return m.Status(ctx)
}

func (m *Manager) Status(ctx context.Context) (service.ServiceStatus, error) {
	if _, err := os.Stat(m.unitPath()); errors.Is(err, os.ErrNotExist) {
		return service.ServiceStatus{Backend: service.BackendSystemdUser, State: service.ServiceNotInstalled, CoreVersion: health.UnknownVersion}, nil
	} else if err != nil {
		return service.ServiceStatus{}, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not inspect systemd user service definition")
	}
	output, err := m.runner.Run(ctx, "systemctl", "--user", "show", m.identity(), "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=MainPID", "--property=ExecMainStartTimestamp", "--property=ExecMainStatus")
	if err != nil {
		return service.ServiceStatus{}, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not read systemd user service status")
	}
	properties := parseProperties(output)
	status := service.ServiceStatus{
		Backend:     service.BackendSystemdUser,
		State:       mapState(properties["LoadState"], properties["ActiveState"]),
		Detail:      strings.TrimSpace(properties["SubState"]),
		CoreVersion: health.UnknownVersion,
	}
	status.PID, _ = strconv.Atoi(properties["MainPID"])
	if timestamp := properties["ExecMainStartTimestamp"]; timestamp != "" {
		status.Since, _ = time.Parse("Mon 2006-01-02 15:04:05 MST", timestamp)
	}
	m.mu.Lock()
	last := m.lastHealth
	m.mu.Unlock()
	if status.State == service.ServiceRunning && last.Version != "" {
		status.Healthy = true
		status.CoreVersion = last.Version
		status.Warning = last.Warning
	}
	return status, nil
}

func (m *Manager) Logs(ctx context.Context, tail int, follow bool) (io.ReadCloser, error) {
	if err := m.requireOwned(); err != nil {
		return nil, err
	}
	if tail < 0 {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "log tail count cannot be negative")
	}
	args := []string{"--user", "-u", m.identity(), "-n", strconv.Itoa(tail), "--no-pager"}
	if follow {
		args = append(args, "-f")
	}
	stream, err := m.runner.Stream(ctx, "journalctl", args...)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not read systemd user service logs")
	}
	return newRedactingReader(stream), nil
}

func (m *Manager) requireOwned() error {
	body, err := os.ReadFile(m.unitPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "systemd user service is not installed")
		}
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not read systemd user service definition")
	}
	if !Owned(body, m.instanceID) {
		return foreignError("the canonical unit path is not owned by this PMux instance")
	}
	return nil
}

func RenderUnit(spec service.ServiceSpec) ([]byte, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}
	identity := service.Identity(service.BackendSystemdUser, spec.InstanceID)
	if spec.Identity != "" && spec.Identity != identity {
		return nil, foreignError("service identity is not canonical")
	}
	environment := foreground.AllowlistedEnvironment(spec.Environment)
	var out strings.Builder
	out.WriteString(OwnershipMarker + "\n")
	out.WriteString("# PMux-Instance: " + spec.InstanceID + "\n")
	out.WriteString("[Unit]\n")
	out.WriteString("Description=CLIProxyAPI instance " + spec.InstanceID + " (managed by PMux)\n")
	out.WriteString("After=network-online.target\n\n")
	out.WriteString("[Service]\nType=simple\n")
	out.WriteString("WorkingDirectory=" + systemdPath(spec.RuntimeDir) + "\n")
	out.WriteString("ExecStart=" + quote(filepath.ToSlash(spec.PMuxPath)) + " --binary " + quote(filepath.ToSlash(spec.BinaryPath)) + " --config " + quote(filepath.ToSlash(spec.ConfigPath)) + " --runtime-dir " + quote(filepath.ToSlash(spec.RuntimeDir)) + "\n")
	for _, entry := range environment {
		out.WriteString("Environment=" + quote(entry) + "\n")
	}
	out.WriteString("Restart=on-failure\nRestartSec=2\n\n")
	out.WriteString("[Install]\nWantedBy=default.target\n")
	return []byte(out.String()), nil
}

func Owned(body []byte, instanceID string) bool {
	return bytes.HasPrefix(body, []byte(OwnershipMarker+"\n# PMux-Instance: "+instanceID+"\n"))
}

func validateSpec(spec service.ServiceSpec) error {
	if !instanceIDPattern.MatchString(spec.InstanceID) {
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "service instance ID is invalid")
	}
	for label, path := range map[string]string{"PMux executable": spec.PMuxPath, "CLIProxyAPI executable": spec.BinaryPath, "config": spec.ConfigPath, "runtime directory": spec.RuntimeDir, "log directory": spec.LogDir} {
		if !filepath.IsAbs(path) {
			return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, label+" path must be absolute")
		}
		if strings.ContainsAny(path, "\n\r\x00") {
			return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, label+" path contains unsupported characters")
		}
	}
	info, err := os.Stat(spec.RuntimeDir)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "could not access PMux runtime directory")
	}
	if !info.IsDir() {
		return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "PMux runtime path is not a directory")
	}
	if _, err := os.Stat(filepath.Join(spec.RuntimeDir, ".env")); err == nil {
		return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "runtime directory contains .env; refusing to start CLIProxyAPI because CWD environment could override the recorded config")
	} else if !errors.Is(err, os.ErrNotExist) {
		return pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "could not verify that PMux runtime directory contains no .env")
	}
	return nil
}

func systemdPath(value string) string {
	value = filepath.ToSlash(value)
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, " ", `\x20`)
	return value
}
func quote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "%", "%%")
	return `"` + value + `"`
}

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".pmux-unit-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func foreignError(explanation string) error {
	return &pmuxerr.Error{Code: pmuxerr.ServiceForeignOwner, Class: pmuxerr.Environment, Message: "PMux will not replace a foreign service definition", Explanation: explanation, Repair: []string{"Complete the explicit adoption hardening transaction before modifying it."}}
}

func parseProperties(output []byte) map[string]string {
	properties := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			properties[name] = value
		}
	}
	return properties
}

func mapState(load, active string) service.ServiceState {
	if load == "not-found" {
		return service.ServiceNotInstalled
	}
	switch active {
	case "active":
		return service.ServiceRunning
	case "activating", "reloading":
		return service.ServiceStarting
	case "deactivating":
		return service.ServiceStopping
	case "inactive":
		return service.ServiceStopped
	case "failed":
		return service.ServiceFailed
	default:
		return service.ServiceUnknown
	}
}

func newRedactingReader(source io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer func() { _ = writer.Close() }()
		defer func() { _ = source.Close() }()
		scanner := bufio.NewScanner(source)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			if _, err := io.WriteString(writer, foreground.RedactLogText(scanner.Text())+"\n"); err != nil {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = writer.CloseWithError(err)
		}
	}()
	return struct {
		io.Reader
		io.Closer
	}{Reader: reader, Closer: closeBoth{reader: reader, source: source}}
}

type closeBoth struct {
	reader *io.PipeReader
	source io.Closer
}

func (c closeBoth) Close() error {
	first := c.reader.Close()
	second := c.source.Close()
	if first != nil {
		return first
	}
	return second
}

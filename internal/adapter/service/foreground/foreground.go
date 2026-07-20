package foreground

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var (
	authorizationPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)([^\s,;]+)`)
	managementKeyPattern = regexp.MustCompile(`(?i)(x-management-key\s*:\s*)([^\s,;]+)`)
	proxyKeyPattern      = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
)
var inspectProcessOwned = processOwned
var stopRecordedProcess = stopExternal

var allowedEnvironment = map[string]struct{}{
	"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "TMPDIR": {},
	"LANG": {}, "LC_ALL": {}, "TERM": {},
	"SYSTEMROOT": {}, "SystemRoot": {}, "SystemDrive": {}, "TEMP": {}, "TMP": {},
	"COMSPEC": {}, "ComSpec": {}, "PATHEXT": {},
}

// Command is the complete, shell-free child process contract.
type Command struct {
	Path   string
	Args   []string
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type Process interface {
	PID() int
	Signal(os.Signal) error
	Kill() error
	Wait() error
}

type Runner interface {
	Start(ctx context.Context, command Command) (Process, error)
}

type OSRunner struct{}

func (OSRunner) Start(_ context.Context, command Command) (Process, error) {
	cmd := exec.Command(command.Path, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = append([]string(nil), command.Env...)
	cmd.Stdin = command.Stdin
	cmd.Stdout = command.Stdout
	cmd.Stderr = command.Stderr
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return osProcess{cmd: cmd}, nil
}

type osProcess struct{ cmd *exec.Cmd }

func (p osProcess) PID() int                      { return p.cmd.Process.Pid }
func (p osProcess) Signal(signal os.Signal) error { return signalProcess(p.cmd.Process, signal) }
func (p osProcess) Kill() error                   { return killProcess(p.cmd.Process) }
func (p osProcess) Wait() error                   { return p.cmd.Wait() }

// Manager implements service.ServiceManager for an attached foreground process.
type Manager struct {
	mu            sync.Mutex
	runner        Runner
	health        health.Checker
	spec          service.ServiceSpec
	owned         bool
	process       Process
	done          chan error
	since         time.Time
	state         service.ServiceState
	result        health.Result
	logs          *logBuffer
	pidPath       string
	externalPID   int
	externalSince time.Time
	attached      *Streams
}

var _ service.ServiceManager = (*Manager)(nil)

func New(runner Runner, checker health.Checker) *Manager {
	if runner == nil {
		runner = OSRunner{}
	}
	return &Manager{runner: runner, health: checker, state: service.ServiceNotInstalled, logs: newLogBuffer(1000)}
}

// NewPersistent records and validates foreground process ownership across PMux
// invocations. A stale or mismatched record is removed rather than trusted.
func NewPersistent(runner Runner, checker health.Checker, pidPath string) *Manager {
	manager := New(runner, checker)
	manager.pidPath = pidPath
	return manager
}

// Streams are attached directly to CLIProxyAPI for `service start --foreground`.
type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// NewAttachedPersistent configures a persistent manager whose attached start
// returns only after the child is healthy and provides a separate lifetime wait.
func NewAttachedPersistent(runner Runner, checker health.Checker, pidPath string, streams Streams) *Manager {
	manager := NewPersistent(runner, checker, pidPath)
	manager.attached = &streams
	return manager
}

func (m *Manager) Backend() service.ServiceBackend { return service.BackendForeground }

func (m *Manager) Detect(ctx context.Context) (service.ServiceStatus, error) { return m.Status(ctx) }

func (m *Manager) Install(_ context.Context, spec service.ServiceSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	canonical := service.Identity(service.BackendForeground, spec.InstanceID)
	if spec.Identity == "" {
		spec.Identity = canonical
	}
	if spec.Identity != canonical {
		return ownershipError(fmt.Sprintf("foreground identity %q is not canonical %q", spec.Identity, canonical))
	}
	spec.Environment = AllowlistedEnvironment(spec.Environment)

	externalPID, externalSince := m.recoverPID(spec)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owned && !sameSpec(m.spec, spec) {
		return ownershipError("a different foreground installation is already registered")
	}
	m.spec = spec
	m.owned = true
	m.externalPID = externalPID
	m.externalSince = externalSince
	if m.process == nil {
		if externalPID > 0 {
			m.state = service.ServiceRunning
		} else {
			m.state = service.ServiceStopped
		}
	}
	return nil
}

func (m *Manager) Uninstall(ctx context.Context) error {
	m.mu.Lock()
	owned := m.owned
	running := m.process != nil || m.externalPID > 0
	m.mu.Unlock()
	if !owned {
		return nil
	}
	if running {
		if err := m.Stop(ctx, 5*time.Second); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.spec = service.ServiceSpec{}
	m.owned = false
	m.state = service.ServiceNotInstalled
	m.externalPID = 0
	m.externalSince = time.Time{}
	_ = m.removePIDRecord()
	m.mu.Unlock()
	return nil
}

func (m *Manager) Start(ctx context.Context) error {
	_, err := m.start(ctx, nil)
	return err
}

// StartAttached commits startup through the normal health gate, then returns a
// waiter for the process lifetime. The mutation lock may be released before the
// caller invokes the waiter.
func (m *Manager) StartAttached(ctx context.Context) (func(context.Context) error, error) {
	if m.attached == nil {
		return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Internal, "foreground terminal streams are not configured")
	}
	done, err := m.start(ctx, m.attached)
	if err != nil {
		return nil, err
	}
	if done == nil {
		return nil, pmuxerr.New(pmuxerr.ServiceForeignOwner, pmuxerr.Environment, "foreground CLIProxyAPI is already running and cannot be attached to this terminal")
	}
	return func(waitCtx context.Context) error {
		select {
		case err := <-done:
			if err == nil {
				return nil
			}
			return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Upstream, "foreground CLIProxyAPI exited")
		case <-waitCtx.Done():
			stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			if err := m.Stop(stopCtx, 5*time.Second); err != nil {
				return err
			}
			return pmuxerr.Wrap(waitCtx.Err(), pmuxerr.CodeInterrupted, pmuxerr.User, "foreground CLIProxyAPI was interrupted")
		}
	}, nil
}

func (m *Manager) start(ctx context.Context, streams *Streams) (<-chan error, error) {
	m.mu.Lock()
	if !m.owned {
		m.mu.Unlock()
		return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "foreground service is not installed")
	}
	if m.process != nil || m.externalPID > 0 {
		m.mu.Unlock()
		if streams != nil {
			return nil, pmuxerr.New(pmuxerr.ServiceForeignOwner, pmuxerr.Environment, "foreground CLIProxyAPI is already running and cannot be attached to this terminal")
		}
		return nil, nil
	}
	spec := m.spec
	var logs *logBuffer
	command := Command{
		Path: spec.BinaryPath,
		Args: []string{"-config", spec.ConfigPath},
		Dir:  spec.RuntimeDir,
		Env:  append([]string(nil), spec.Environment...),
	}
	if streams == nil {
		logs = newLogBuffer(1000)
		m.logs = logs
		command.Stdout, command.Stderr = logs, logs
	} else {
		command.Stdin, command.Stdout, command.Stderr = streams.Stdin, streams.Stdout, streams.Stderr
	}
	m.mu.Unlock()

	if err := ensureCleanRuntime(spec.RuntimeDir); err != nil {
		return nil, err
	}
	if m.health == nil {
		return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Internal, "foreground health checker is not configured")
	}
	process, err := m.runner.Start(ctx, command)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not start CLIProxyAPI in the foreground")
	}
	if err := m.writePIDRecord(process.PID(), spec); err != nil {
		_ = process.Kill()
		return nil, err
	}
	m.mu.Lock()
	m.process = process
	m.done = make(chan error, 1)
	m.since = time.Now()
	m.state = service.ServiceStarting
	done := m.done
	m.mu.Unlock()
	go m.wait(process, done, logs)

	result, err := m.health.WaitReady(ctx)
	if err != nil {
		m.mu.Lock()
		if m.process == process {
			m.state = service.ServiceFailed
		}
		m.mu.Unlock()
		if streams != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			_ = m.Stop(stopCtx, 5*time.Second)
			cancel()
		}
		return nil, err
	}
	m.mu.Lock()
	if m.process == process {
		m.state = service.ServiceRunning
		m.result = result
	}
	m.mu.Unlock()
	return done, nil
}

func (m *Manager) wait(process Process, done chan error, logs *logBuffer) {
	err := process.Wait()
	m.mu.Lock()
	if m.process == process {
		if stoppedByLifecycle(err) {
			err = nil
		}
		m.process = nil
		if m.state == service.ServiceStopping || err == nil {
			m.state = service.ServiceStopped
		} else {
			m.state = service.ServiceFailed
		}
	}
	_ = m.removePIDRecord()
	m.mu.Unlock()
	done <- err
	close(done)
	if logs != nil {
		logs.finish()
	}
}

func (m *Manager) Stop(ctx context.Context, timeout time.Duration) error {
	m.mu.Lock()
	if !m.owned {
		m.mu.Unlock()
		return nil
	}
	process := m.process
	done := m.done
	externalPID := m.externalPID
	if process == nil && externalPID == 0 {
		m.state = service.ServiceStopped
		m.mu.Unlock()
		_ = m.removePIDRecord()
		return nil
	}
	m.state = service.ServiceStopping
	m.mu.Unlock()
	if process == nil {
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		if err := stopRecordedProcess(ctx, externalPID, timeout); err != nil {
			return err
		}
		m.mu.Lock()
		m.externalPID = 0
		m.externalSince = time.Time{}
		m.state = service.ServiceStopped
		m.mu.Unlock()
		return m.removePIDRecord()
	}

	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if killErr := process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return pmuxerr.Wrap(killErr, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not stop foreground CLIProxyAPI after graceful shutdown was unavailable")
		}
		select {
		case <-ctx.Done():
			return pmuxerr.Wrap(ctx.Err(), pmuxerr.ServiceStartFailed, pmuxerr.Environment, "foreground CLIProxyAPI shutdown was interrupted")
		case <-done:
			return nil
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return pmuxerr.Wrap(ctx.Err(), pmuxerr.ServiceStartFailed, pmuxerr.Environment, "foreground CLIProxyAPI shutdown was interrupted")
	case <-done:
		return nil
	case <-timer.C:
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not force foreground CLIProxyAPI shutdown")
	}
	select {
	case <-ctx.Done():
		return pmuxerr.Wrap(ctx.Err(), pmuxerr.ServiceStartFailed, pmuxerr.Environment, "foreground CLIProxyAPI shutdown was interrupted")
	case <-done:
		return nil
	}
}

func (m *Manager) Restart(ctx context.Context) (service.ServiceStatus, error) {
	if err := m.Stop(ctx, 5*time.Second); err != nil {
		return service.ServiceStatus{}, err
	}
	if err := m.Start(ctx); err != nil {
		return service.ServiceStatus{}, err
	}
	return m.Status(ctx)
}

func (m *Manager) Status(_ context.Context) (service.ServiceStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := service.ServiceStatus{Backend: service.BackendForeground, State: m.state, CoreVersion: health.UnknownVersion}
	if !m.owned {
		status.State = service.ServiceNotInstalled
		return status, nil
	}
	if m.process != nil {
		status.PID = m.process.PID()
		status.Since = m.since
	} else if m.externalPID > 0 {
		status.PID = m.externalPID
		status.Since = m.externalSince
	}
	if m.state == service.ServiceRunning && m.process != nil {
		status.Healthy = true
		status.CoreVersion = m.result.Version
		status.Warning = m.result.Warning
	}
	return status, nil
}

type pidRecord struct {
	PID        int       `json:"pid"`
	Started    time.Time `json:"started"`
	BinaryPath string    `json:"binary_path"`
	ConfigPath string    `json:"config_path"`
	RuntimeDir string    `json:"runtime_dir"`
	InstanceID string    `json:"instance_id"`
	Argv       []string  `json:"argv"`
}

func (m *Manager) writePIDRecord(pid int, spec service.ServiceSpec) error {
	if m.pidPath == "" {
		return nil
	}
	record := pidRecord{PID: pid, Started: time.Now().UTC(), BinaryPath: spec.BinaryPath, ConfigPath: spec.ConfigPath, RuntimeDir: spec.RuntimeDir, InstanceID: spec.InstanceID, Argv: []string{"-config", spec.ConfigPath}}
	body, err := json.Marshal(record)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Internal, "could not encode foreground CLIProxyAPI ownership metadata")
	}
	if err := os.MkdirAll(filepath.Dir(m.pidPath), 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not create the foreground ownership directory")
	}
	temp, err := os.CreateTemp(filepath.Dir(m.pidPath), "."+filepath.Base(m.pidPath)+".tmp-*")
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not create foreground ownership metadata")
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(body, '\n')); err != nil {
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
	if err := os.Rename(name, m.pidPath); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not commit foreground ownership metadata")
	}
	if err := syncDirectory(filepath.Dir(m.pidPath)); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not make foreground ownership metadata durable")
	}
	return nil
}

func (m *Manager) recoverPID(spec service.ServiceSpec) (int, time.Time) {
	if m.pidPath == "" {
		return 0, time.Time{}
	}
	body, err := os.ReadFile(m.pidPath)
	if err != nil {
		return 0, time.Time{}
	}
	var record pidRecord
	if json.Unmarshal(body, &record) != nil ||
		record.InstanceID != spec.InstanceID ||
		record.BinaryPath != spec.BinaryPath ||
		record.ConfigPath != spec.ConfigPath ||
		record.RuntimeDir != spec.RuntimeDir ||
		!inspectProcessOwned(record.PID, spec, record.Started) {
		_ = os.Remove(m.pidPath)
		return 0, time.Time{}
	}
	if len(record.Argv) != 2 || record.Argv[0] != "-config" || record.Argv[1] != spec.ConfigPath {
		_ = os.Remove(m.pidPath)
		return 0, time.Time{}
	}
	return record.PID, record.Started
}

func (m *Manager) removePIDRecord() error {
	if m.pidPath == "" {
		return nil
	}
	if err := os.Remove(m.pidPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not remove foreground ownership metadata")
	}
	if err := syncDirectory(filepath.Dir(m.pidPath)); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not make foreground ownership removal durable")
	}
	return nil
}

func (m *Manager) Logs(ctx context.Context, tail int, follow bool) (io.ReadCloser, error) {
	return m.logs.reader(ctx, tail, follow), nil
}

// AllowlistedEnvironment returns a deterministic, duplicate-free complete child
// environment. Unknown and secret-bearing variables are omitted.
func AllowlistedEnvironment(input []string) []string {
	values := make(map[string]string)
	for _, entry := range input {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			continue
		}
		if _, ok := allowedEnvironment[name]; !ok || redact.IsSensitiveKey(name) {
			continue
		}
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+values[name])
	}
	return out
}

func validateSpec(spec service.ServiceSpec) error {
	if !instanceIDPattern.MatchString(spec.InstanceID) {
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "service instance ID is invalid")
	}
	identity := service.Identity(service.BackendForeground, spec.InstanceID)
	if spec.Identity != "" && spec.Identity != identity {
		return ownershipError("foreground service identity is not canonical")
	}
	for label, path := range map[string]string{
		"PMux executable":        spec.PMuxPath,
		"CLIProxyAPI executable": spec.BinaryPath,
		"config":                 spec.ConfigPath,
		"runtime directory":      spec.RuntimeDir,
		"log directory":          spec.LogDir,
	} {
		if !filepath.IsAbs(path) {
			return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, label+" path must be absolute")
		}
	}
	return ensureCleanRuntime(spec.RuntimeDir)
}

func ensureCleanRuntime(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "could not access PMux runtime directory")
	}
	if !info.IsDir() {
		return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "PMux runtime path is not a directory")
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		return pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "runtime directory contains .env; refusing to start CLIProxyAPI because CWD environment could override the recorded config")
	} else if !errors.Is(err, os.ErrNotExist) {
		return pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "could not verify that PMux runtime directory contains no .env")
	}
	return nil
}

func sameSpec(left, right service.ServiceSpec) bool {
	return left.InstanceID == right.InstanceID && left.Identity == right.Identity && left.PMuxPath == right.PMuxPath &&
		left.BinaryPath == right.BinaryPath && left.ConfigPath == right.ConfigPath && left.RuntimeDir == right.RuntimeDir &&
		left.LogDir == right.LogDir && strings.Join(left.Environment, "\x00") == strings.Join(right.Environment, "\x00")
}

func ownershipError(explanation string) error {
	return &pmuxerr.Error{
		Code:        pmuxerr.ServiceForeignOwner,
		Class:       pmuxerr.Environment,
		Message:     "PMux will not replace a foreign service definition",
		Explanation: explanation,
		Repair:      []string{"Complete the explicit adoption hardening transaction before modifying it."},
	}
}

// RedactLogText sanitizes common structured secret forms using the shared
// redaction policy. It deliberately favors over-redaction for service logs.
func RedactLogText(text string) string {
	text = authorizationPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := authorizationPattern.FindStringSubmatch(match)
		return parts[1] + redact.Mask(parts[2])
	})
	text = managementKeyPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := managementKeyPattern.FindStringSubmatch(match)
		return parts[1] + redact.Mask(parts[2])
	})
	text = proxyKeyPattern.ReplaceAllStringFunc(text, redact.Mask)

	fields := strings.Fields(text)
	for index, field := range fields {
		name, value, ok := strings.Cut(field, "=")
		if ok && redact.IsSensitiveKey(strings.Trim(name, `"'`)) {
			fields[index] = name + "=" + redact.Mask(strings.Trim(value, `"'`))
		}
	}
	if len(fields) == 0 {
		return text
	}
	redacted := strings.Join(fields, " ")
	if strings.HasSuffix(text, "\n") {
		redacted += "\n"
	}
	return redacted
}

type logBuffer struct {
	mu          sync.Mutex
	maxLines    int
	lines       []string
	subscribers map[chan string]struct{}
	finished    bool
}

func newLogBuffer(maxLines int) *logBuffer {
	return &logBuffer{maxLines: maxLines, subscribers: make(map[chan string]struct{})}
}

func (b *logBuffer) Write(payload []byte) (int, error) {
	text := RedactLogText(string(payload))
	parts := strings.SplitAfter(text, "\n")
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, part := range parts {
		if part == "" {
			continue
		}
		b.lines = append(b.lines, part)
		if len(b.lines) > b.maxLines {
			b.lines = append([]string(nil), b.lines[len(b.lines)-b.maxLines:]...)
		}
		for subscriber := range b.subscribers {
			select {
			case subscriber <- part:
			default:
			}
		}
	}
	return len(payload), nil
}

func (b *logBuffer) finish() {
	b.mu.Lock()
	if !b.finished {
		b.finished = true
		for subscriber := range b.subscribers {
			close(subscriber)
			delete(b.subscribers, subscriber)
		}
	}
	b.mu.Unlock()
}

func (b *logBuffer) reader(ctx context.Context, tail int, follow bool) io.ReadCloser {
	b.mu.Lock()
	start := 0
	if tail > 0 && len(b.lines) > tail {
		start = len(b.lines) - tail
	}
	snapshot := strings.Join(b.lines[start:], "")
	if !follow || b.finished {
		b.mu.Unlock()
		return io.NopCloser(bytes.NewBufferString(snapshot))
	}
	updates := make(chan string, 64)
	b.subscribers[updates] = struct{}{}
	b.mu.Unlock()

	reader, writer := io.Pipe()
	go func() {
		defer func() { _ = writer.Close() }()
		if _, err := io.WriteString(writer, snapshot); err != nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updates:
				if !ok {
					return
				}
				if _, err := io.WriteString(writer, update); err != nil {
					return
				}
			}
		}
	}()
	return reader
}

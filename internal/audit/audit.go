package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

const recordVersion = 1

type Result string

const (
	ResultOK         Result = "ok"
	ResultFailed     Result = "failed"
	ResultRolledBack Result = "rolled_back"
)

type Entry struct {
	Version   int               `json:"version"`
	Timestamp time.Time         `json:"ts"`
	Operation string            `json:"op_id"`
	Actor     string            `json:"actor"`
	Command   string            `json:"command"`
	Target    string            `json:"target"`
	Params    map[string]string `json:"params,omitempty"`
	Result    Result            `json:"result"`
	ErrorCode string            `json:"error_code,omitempty"`
}

type Option func(*Log)

func WithKnownSecrets(values ...string) Option {
	copyOfValues := append([]string(nil), values...)
	return func(log *Log) { log.secrets = append(log.secrets, copyOfValues...) }
}

type Log struct {
	path    string
	now     func() time.Time
	secrets []string
	mu      sync.Mutex
}

func New(path string, options ...Option) (*Log, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "audit path must be absolute")
	}
	log := &Log{path: path, now: time.Now}
	for _, option := range options {
		option(log)
	}
	return log, nil
}

// Prepare verifies that the durable audit destination can be created,
// protected, flushed, and reopened before a governed mutation is dispatched.
// It writes no audit record.
func (l *Log) Prepare() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create audit directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect audit directory")
	}
	_, statErr := os.Stat(l.path)
	created := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not prepare audit log")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect audit log")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush audit log")
	}
	if err := file.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not close audit log")
	}
	if created {
		if err := syncAuditDirectory(dir); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush audit directory")
		}
	}
	return nil
}

// RegisterKnownSecrets adds transient exact-match redactions for secrets
// learned after construction. Values are never written to the audit log.
func (l *Log) RegisterKnownSecrets(values ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.secrets = append(l.secrets, values...)
}

// Append redacts sensitive parameter fields and exact known secret values,
// appends exactly one JSON record, and fsyncs it before returning.
func (l *Log) Append(entry Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry.Timestamp.IsZero() {
		entry.Timestamp = l.now().UTC()
	}
	entry.Version = recordVersion
	entry.Operation = redact.Known(entry.Operation, l.secrets...)
	entry.Actor = redact.Known(entry.Actor, l.secrets...)
	entry.Command = redact.Known(entry.Command, l.secrets...)
	entry.Target = redact.Known(entry.Target, l.secrets...)
	entry.ErrorCode = redact.Known(entry.ErrorCode, l.secrets...)
	entry.Params = redact.Map(entry.Params)
	for key, value := range entry.Params {
		entry.Params[key] = redact.Known(value, l.secrets...)
	}
	if entry.Operation == "" || entry.Command == "" || entry.Target == "" {
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "audit entry is missing required fields")
	}
	switch entry.Result {
	case ResultOK, ResultFailed, ResultRolledBack:
	default:
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "audit entry has an invalid result")
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "could not encode audit record")
	}
	payload = append(payload, '\n')
	return l.append(payload)
}

func (l *Log) Entries() ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	file, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not open audit log")
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var entries []Entry
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && (!errors.Is(readErr, io.EOF) || line[len(line)-1] == '\n') {
			var entry Entry
			if err := json.Unmarshal(line, &entry); err != nil || entry.Version != recordVersion {
				if err == nil {
					err = errors.New("unsupported audit record version")
				}
				return nil, pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Internal, "audit log contains an invalid record")
			}
			entries = append(entries, entry)
		}
		if errors.Is(readErr, io.EOF) {
			return entries, nil
		}
		if readErr != nil {
			return nil, pmuxerr.Wrap(readErr, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not read audit log")
		}
	}
}

func (l *Log) append(payload []byte) error {
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not create audit directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect audit directory")
	}
	if err := trimTornTail(l.path); err != nil {
		return err
	}
	_, statErr := os.Stat(l.path)
	created := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not open audit log for append")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect audit log")
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not append audit log")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush audit log")
	}
	if err := file.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not close audit log")
	}
	if created {
		if err := syncAuditDirectory(dir); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not flush audit directory")
		}
	}
	return nil
}

func trimTornTail(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not open audit log for recovery")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not inspect audit log for recovery")
	}
	if info.Size() == 0 {
		return nil
	}
	var value [1]byte
	if _, err := file.ReadAt(value[:], info.Size()-1); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not inspect audit log tail")
	}
	if value[0] == '\n' {
		return nil
	}
	truncateAt := int64(0)
	var block [4096]byte
	for end := info.Size(); end > 0; {
		start := end - int64(len(block))
		if start < 0 {
			start = 0
		}
		length := int(end - start)
		if _, err := file.ReadAt(block[:length], start); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not scan audit log tail")
		}
		found := false
		for index := length - 1; index >= 0; index-- {
			if block[index] == '\n' {
				truncateAt = start + int64(index) + 1
				found = true
				break
			}
		}
		if found {
			break
		}
		end = start
	}
	if err := file.Truncate(truncateAt); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not remove torn audit record")
	}
	if err := file.Sync(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not flush audit recovery")
	}
	return nil
}

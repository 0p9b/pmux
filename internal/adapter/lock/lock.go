package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	adapterfs "github.com/0p9b/pmux/internal/adapter/fs"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type Metadata struct {
	PID       int       `json:"pid"`
	Hostname  string    `json:"hostname,omitempty"`
	Operation string    `json:"operation"`
	StartedAt time.Time `json:"started_at"`
}

type Manager struct {
	path string
	now  func() time.Time
}

func New(path string) (*Manager, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "mutation lock path must be absolute")
	}
	return &Manager{path: path, now: time.Now}, nil
}

type Handle struct {
	file *os.File
	once sync.Once
	err  error
}

// TryAcquire takes the advisory lock without waiting. The metadata is diagnostic
// only; the OS lock is the sole ownership authority.
func (m *Manager) TryAcquire(operation string) (*Handle, error) {
	return m.tryAcquire(operation)
}

// Acquire waits interruptibly for the OS advisory lock. Read-only operations
// should not call Acquire; they require no lock at all.
func (m *Manager) Acquire(ctx context.Context, operation string) (*Handle, error) {
	for {
		handle, err := m.tryAcquire(operation)
		if err == nil {
			return handle, nil
		}
		var busy *BusyError
		if !errors.As(err, &busy) {
			return nil, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, pmuxerr.Wrap(ctx.Err(), pmuxerr.CodeCanceled, pmuxerr.User, "waiting for the PMux mutation lock was canceled")
		case <-timer.C:
		}
	}
}

func (m *Manager) WithMutation(ctx context.Context, operation string, mutate func() error) error {
	handle, err := m.Acquire(ctx, operation)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Release() }()
	return mutate()
}

func (m *Manager) tryAcquire(operation string) (*Handle, error) {
	if operation == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "mutation operation cannot be empty")
	}
	if err := adapterfs.EnsurePrivateDir(filepath.Dir(m.path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(m.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "could not open PMux mutation lock")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect PMux mutation lock")
	}
	locked, err := platformTryLock(file)
	if err != nil {
		_ = file.Close()
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "could not acquire PMux mutation lock")
	}
	if !locked {
		metadata := readMetadata(file)
		_ = file.Close()
		return nil, &BusyError{Metadata: metadata}
	}

	handle := &Handle{file: file}
	hostname, _ := os.Hostname()
	metadata := Metadata{PID: os.Getpid(), Hostname: hostname, Operation: operation, StartedAt: m.now().UTC()}
	payload, err := json.Marshal(metadata)
	if err != nil {
		_ = handle.Release()
		return nil, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "could not encode mutation lock metadata")
	}
	payload = append(payload, '\n')
	if err := file.Truncate(0); err != nil {
		_ = handle.Release()
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "could not reset mutation lock metadata")
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = handle.Release()
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "could not seek mutation lock metadata")
	}
	if _, err := file.Write(payload); err != nil {
		_ = handle.Release()
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "could not write mutation lock metadata")
	}
	if err := file.Sync(); err != nil {
		_ = handle.Release()
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "could not flush mutation lock metadata")
	}
	return handle, nil
}

func (h *Handle) Release() error {
	if h == nil {
		return nil
	}
	h.once.Do(func() {
		unlockErr := platformUnlock(h.file)
		closeErr := h.file.Close()
		h.err = errors.Join(unlockErr, closeErr)
	})
	return h.err
}

// BusyError retains safe holder metadata while mapping to the canonical
// ownership-conflict condition.
type BusyError struct {
	Metadata Metadata
}

func (e *BusyError) Error() string {
	if e.Metadata.PID > 0 && e.Metadata.Operation != "" {
		return fmt.Sprintf("another PMux mutation is in progress (pid %d, operation %s); retry when it completes", e.Metadata.PID, e.Metadata.Operation)
	}
	return "another PMux mutation is in progress"
}

func (e *BusyError) Unwrap() error {
	return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, e.Error())
}

func readMetadata(file *os.File) Metadata {
	if _, err := file.Seek(0, 0); err != nil {
		return Metadata{}
	}
	var metadata Metadata
	_ = json.NewDecoder(file).Decode(&metadata)
	return metadata
}

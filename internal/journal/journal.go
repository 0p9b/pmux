package journal

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	domain "github.com/0p9b/pmux/internal/domain/journal"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

const eventVersion = 1

const (
	StateInProgress  = "in_progress"
	StateCompleted   = "completed"
	StateFailed      = "failed"
	StateInterrupted = "interrupted"
	StateRolledBack  = "rolled_back"
)

type Option func(*Journal)

// WithKnownSecrets ensures exact known values are removed before journal data
// is serialized. The values themselves are held only in memory.
func WithKnownSecrets(values ...string) Option {
	copyOfValues := append([]string(nil), values...)
	return func(j *Journal) { j.secrets = append(j.secrets, copyOfValues...) }
}

type Journal struct {
	path    string
	now     func() time.Time
	secrets []string
	mu      sync.Mutex
}

var _ domain.Journal = (*Journal)(nil)

func New(path string, options ...Option) (*Journal, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "journal path must be absolute")
	}
	j := &Journal{path: path, now: time.Now}
	for _, option := range options {
		option(j)
	}
	return j, nil
}

// RegisterKnownSecrets adds transient exact-match redactions for secrets
// learned after construction. Values are never written to the journal.
func (j *Journal) RegisterKnownSecrets(values ...string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.secrets = append(j.secrets, values...)
}

type event struct {
	Version   int               `json:"version"`
	Sequence  uint64            `json:"sequence"`
	Timestamp time.Time         `json:"timestamp"`
	Type      string            `json:"type"`
	TxID      domain.TxID       `json:"tx_id"`
	Operation string            `json:"operation,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Step      *domain.Step      `json:"step,omitempty"`
	State     string            `json:"state,omitempty"`
	Reason    string            `json:"reason,omitempty"`
}

func (j *Journal) Begin(operation string, metadata map[string]string) (domain.TxID, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if operation == "" {
		return "", pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "journal operation cannot be empty")
	}
	id, err := newTxID(j.now())
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "could not create operation ID")
	}
	events, err := j.readEvents()
	if err != nil {
		return "", err
	}
	e := event{
		Version: eventVersion, Sequence: nextSequence(events), Timestamp: j.now().UTC(),
		Type: "begin", TxID: id, Operation: redact.Known(operation, j.secrets...),
		Metadata: sanitizeMap(metadata, j.secrets), State: StateInProgress,
	}
	if err := j.append(e); err != nil {
		return "", err
	}
	return id, nil
}

func (j *Journal) Record(id domain.TxID, step domain.Step) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	events, err := j.readEvents()
	if err != nil {
		return err
	}
	if err := requirePending(events, id); err != nil {
		return err
	}
	step = j.sanitizeStep(step)
	if step.At.IsZero() {
		step.At = j.now().UTC()
	}
	return j.append(event{
		Version: eventVersion, Sequence: nextSequence(events), Timestamp: j.now().UTC(),
		Type: "step", TxID: id, Step: &step,
	})
}

func (j *Journal) Commit(id domain.TxID) error {
	return j.setState(id, StateCompleted, "")
}

func (j *Journal) Rollback(id domain.TxID) error {
	return j.setState(id, StateRolledBack, "")
}

func (j *Journal) Interrupt(id domain.TxID, reason string) error {
	return j.setState(id, StateInterrupted, reason)
}

func (j *Journal) Fail(id domain.TxID, reason string) error {
	return j.setState(id, StateFailed, reason)
}

func (j *Journal) setState(id domain.TxID, state, reason string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	events, err := j.readEvents()
	if err != nil {
		return err
	}
	if err := requirePending(events, id); err != nil {
		return err
	}
	return j.append(event{
		Version: eventVersion, Sequence: nextSequence(events), Timestamp: j.now().UTC(),
		Type: "state", TxID: id, State: state, Reason: redact.Known(reason, j.secrets...),
	})
}

func (j *Journal) Pending() ([]domain.Tx, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	events, err := j.readEvents()
	if err != nil {
		return nil, err
	}
	transactions, err := fold(events)
	if err != nil {
		return nil, err
	}
	pending := make([]domain.Tx, 0)
	for _, tx := range transactions {
		if tx.State == StateInProgress || tx.State == StateInterrupted || tx.State == StateFailed {
			pending = append(pending, tx)
		}
	}
	sort.Slice(pending, func(i, k int) bool {
		if pending[i].BeganAt.Equal(pending[k].BeganAt) {
			return pending[i].ID < pending[k].ID
		}
		return pending[i].BeganAt.Before(pending[k].BeganAt)
	})
	return pending, nil
}

func (j *Journal) sanitizeStep(step domain.Step) domain.Step {
	step.Name = redact.Known(step.Name, j.secrets...)
	step.Action = redact.Known(step.Action, j.secrets...)
	step.Target = redact.Known(step.Target, j.secrets...)
	step.Undo = sanitizeMap(step.Undo, j.secrets)
	return step
}

func sanitizeMap(input map[string]string, secrets []string) map[string]string {
	if input == nil {
		return nil
	}
	masked := redact.Map(input)
	for key, value := range masked {
		masked[key] = redact.Known(value, secrets...)
	}
	return masked
}

func newTxID(now time.Time) (domain.TxID, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return domain.TxID(fmt.Sprintf("op_%s_%s", now.UTC().Format("20060102T150405"), hex.EncodeToString(suffix[:]))), nil
}

func nextSequence(events []event) uint64 {
	if len(events) == 0 {
		return 1
	}
	return events[len(events)-1].Sequence + 1
}

func requirePending(events []event, id domain.TxID) error {
	transactions, err := fold(events)
	if err != nil {
		return err
	}
	tx, exists := transactions[id]
	if !exists {
		return pmuxerr.New(pmuxerr.JournalCorrupt, pmuxerr.Internal, "journal transaction does not exist")
	}
	if tx.State == StateCompleted || tx.State == StateRolledBack {
		return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "journal transaction is already closed")
	}
	return nil
}

func fold(events []event) (map[domain.TxID]domain.Tx, error) {
	transactions := make(map[domain.TxID]domain.Tx)
	for _, e := range events {
		switch e.Type {
		case "begin":
			if _, exists := transactions[e.TxID]; exists {
				return nil, corrupt("duplicate journal transaction")
			}
			transactions[e.TxID] = domain.Tx{ID: e.TxID, Operation: e.Operation, Metadata: e.Metadata, State: StateInProgress, BeganAt: e.Timestamp}
		case "step":
			tx, exists := transactions[e.TxID]
			if !exists || e.Step == nil {
				return nil, corrupt("journal step has no transaction")
			}
			if tx.State == StateCompleted || tx.State == StateRolledBack {
				return nil, corrupt("journal step follows a closed transaction")
			}
			tx.Steps = append(tx.Steps, *e.Step)
			transactions[e.TxID] = tx
		case "state":
			tx, exists := transactions[e.TxID]
			if !exists {
				return nil, corrupt("journal state has no transaction")
			}
			if tx.State == StateCompleted || tx.State == StateRolledBack {
				return nil, corrupt("journal state follows a closed transaction")
			}
			switch e.State {
			case StateCompleted, StateFailed, StateInterrupted, StateRolledBack:
			default:
				return nil, corrupt("journal contains an invalid transaction state")
			}
			tx.State = e.State
			transactions[e.TxID] = tx
		default:
			return nil, corrupt("journal contains an unknown event")
		}
	}
	return transactions, nil
}

func (j *Journal) readEvents() ([]event, error) {
	file, err := os.Open(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not open operation journal")
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var events []event
	var lastSequence uint64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !errors.Is(readErr, io.EOF) || line[len(line)-1] == '\n' {
				var e event
				if err := json.Unmarshal(line, &e); err != nil {
					return nil, corrupt("operation journal contains invalid JSON")
				}
				if e.Version != eventVersion || e.Sequence != lastSequence+1 || e.TxID == "" {
					return nil, corrupt("operation journal sequence or version is invalid")
				}
				lastSequence = e.Sequence
				events = append(events, e)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, pmuxerr.Wrap(readErr, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not read operation journal")
		}
	}
	return events, nil
}

func (j *Journal) append(e event) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "could not encode operation journal record")
	}
	payload = append(payload, '\n')
	dir := filepath.Dir(j.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not create operation journal directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect operation journal directory")
	}
	if err := trimTornTail(j.path); err != nil {
		return err
	}
	_, statErr := os.Stat(j.path)
	created := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not open operation journal for append")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "could not protect operation journal")
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not append operation journal")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not flush operation journal")
	}
	if err := file.Close(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not close operation journal")
	}
	if created {
		if err := syncDirectory(dir); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not flush operation journal directory")
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
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not open operation journal for recovery")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not inspect operation journal for recovery")
	}
	if info.Size() == 0 {
		return nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], info.Size()-1); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not inspect operation journal tail")
	}
	if last[0] == '\n' {
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
			return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not scan operation journal tail")
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
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not remove torn journal record")
	}
	if err := file.Sync(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.JournalCorrupt, pmuxerr.Environment, "could not flush journal recovery")
	}
	return nil
}

func syncDirectory(path string) error {
	return syncJournalDirectory(path)
}

func corrupt(message string) error {
	return pmuxerr.New(pmuxerr.JournalCorrupt, pmuxerr.Internal, message)
}

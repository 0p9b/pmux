package tui

import (
	"context"
	"io"
	"time"

	"github.com/0p9b/pmux/internal/redact"
)

// Facade is the sole boundary through which the TUI can request application
// behavior. A Shell never calls it during construction or Init; the first
// render is always made from the supplied Snapshot.
type Facade interface {
	Execute(context.Context, ActionRequest) (Snapshot, error)
}

// ActionRequest is transport-neutral. Arguments are already separated; they
// must never be interpreted by a shell.
type ActionRequest struct {
	ID        ActionID
	Arguments []string
	// Secret is an ephemeral protected input. Facades must consume and clear it;
	// it is never copied into snapshots, events, state, logs, or output.
	Secret []byte
	// Events receives redacted operation progress while Execute is running.
	// The Shell owns and drains the channel; facades must honor cancellation
	// rather than blocking forever on delivery.
	Events chan<- ActionEvent
	// ProtectedInput is a one-shot, non-echo response stream for prompts that
	// arise after an operation starts, such as an OAuth callback URL.
	ProtectedInput io.Reader
}

// ActionEvent is the presentation-safe streaming projection shared by OAuth,
// log follow, and other long-running operations.
type ActionEvent struct {
	Type      string
	Timestamp time.Time
	Message   string
	Fields    map[string]string
}

// Snapshot is an immutable presentation projection. It intentionally contains
// no infrastructure clients and only redacted, display-safe credential data.
type Snapshot struct {
	Version   string
	Context   string
	UpdatedAt time.Time
	Dashboard DashboardSnapshot
	Providers []ProviderRow
	Models    []ModelRow
	Launch    LaunchSnapshot
	Doctor    []DoctorRow
	Service   ServiceSnapshot
	Config    []ConfigRow
	Settings  []SettingRow
	Logs      []LogRow
}

type DashboardSnapshot struct {
	Installation string
	CoreVersion  string
	Service      Status
	ConfigPath   string
	AuthDir      string
	Bind         string
	Health       Status
	Claude       Status
	Providers    int
	Accounts     int
	Models       int
	RecentErrors []string
	Warnings     []string
	Recommended  ActionID
}

type ProviderRow struct {
	ID       string
	Name     string
	Kind     string
	Flows    []string
	Enabled  bool
	Status   Status
	Accounts string
	Models   int
}

type ModelRow struct {
	ID        string
	Owner     string
	Provider  string
	Available bool
	Favorite  bool
	Stale     bool
	Latency   time.Duration
}

type LaunchSnapshot struct {
	ClientPath    string
	ClientVersion string
	ModelID       string
	Provider      string
	BaseURL       string
	Token         SecretMask
	WorkingDir    string
	Arguments     []string
	Ready         bool
	Reason        string
}

type DoctorRow struct {
	ID       string
	Status   Status
	Severity string
	Summary  string
	Evidence []string
	Fixable  bool
}

type ServiceSnapshot struct {
	Backend     string
	Identity    string
	Status      Status
	PID         int
	BinaryPath  string
	ConfigPath  string
	RuntimeDir  string
	CoreVersion string
	Warning     string
}

type ConfigRow struct {
	Key        string
	Value      string
	Sensitive  bool
	Activation string
}

type SettingRow struct {
	Key   string
	Value string
}

type LogRow struct {
	Timestamp time.Time
	Source    string
	Level     string
	Message   string
	RequestID string
}

func MaskSecret(value string) SecretMask {
	return SecretMask(redact.Mask(value))
}

// SecretMask is the only secret-shaped value accepted by a presentation DTO.
// It stores no complete secret.
type SecretMask string

func (s SecretMask) String() string {
	if s == "" {
		return "********"
	}
	return string(s)
}

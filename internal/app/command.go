package app

import (
	"context"
	"fmt"
	"time"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

// Invocation is the transport-neutral description of one public PMux command.
// Presentation layers must set Interactive false whenever prompting is forbidden
// (notably for JSON and non-TTY execution).
type Operation string

const (
	OpDashboardStatus     Operation = "dashboard.status"
	OpTUIDashboard        Operation = "tui.dashboard"
	OpSetup               Operation = "setup"
	OpProvidersList       Operation = "providers.list"
	OpProvidersLogin      Operation = "providers.login"
	OpProvidersVerify     Operation = "providers.verify"
	OpProvidersEnable     Operation = "providers.enable"
	OpProvidersDisable    Operation = "providers.disable"
	OpProvidersRemove     Operation = "providers.remove"
	OpProvidersResetQuota Operation = "providers.reset_quota"
	OpTUIProviders        Operation = "tui.providers"
	OpModelsList          Operation = "models.list"
	OpModelsTest          Operation = "models.test"
	OpModelsFavorite      Operation = "models.favorite"
	OpModelsUnfavorite    Operation = "models.unfavorite"
	OpModelsAliases       Operation = "models.aliases"
	OpModelsExclusions    Operation = "models.exclusions"
	OpTUIModels           Operation = "tui.models"
	OpLaunch              Operation = "launch"
	OpLaunchPreflight     Operation = "launch.preflight"
	OpDoctor              Operation = "doctor"
	OpServiceStatus       Operation = "service.status"
	OpServiceStart        Operation = "service.start"
	OpServiceStop         Operation = "service.stop"
	OpServiceRestart      Operation = "service.restart"
	OpServiceInstall      Operation = "service.install"
	OpServiceUninstall    Operation = "service.uninstall"
	OpServiceLogs         Operation = "service.logs"
	OpTUIService          Operation = "tui.service"
	OpConfigShow          Operation = "config.show"
	OpConfigGet           Operation = "config.get"
	OpConfigSet           Operation = "config.set"
	OpConfigEdit          Operation = "config.edit"
	OpConfigBackup        Operation = "config.backup"
	OpConfigRestore       Operation = "config.restore"
	OpTUIConfig           Operation = "tui.config"
	OpUpdateCheck         Operation = "update.check"
	OpUpdateSelf          Operation = "update.self"
	OpUpdateProxy         Operation = "update.proxy"
	OpKeysList            Operation = "keys.list"
	OpKeysAdd             Operation = "keys.add"
	OpKeysRemove          Operation = "keys.remove"
	OpPluginsList         Operation = "plugins.list"
	OpPluginStore         Operation = "plugins.store"
	OpPluginInstall       Operation = "plugins.install"
	OpPluginSetEnabled    Operation = "plugins.set_enabled"
	OpPluginConfigShow    Operation = "plugins.config_show"
	OpPluginConfigSet     Operation = "plugins.config_set"
	OpPluginRemove        Operation = "plugins.remove"
	OpPanel               Operation = "panel"
	OpProfilesList        Operation = "profiles.list"
	OpProfilesShow        Operation = "profiles.show"
	OpProfilesSet         Operation = "profiles.set"
	OpProfilesRemove      Operation = "profiles.remove"
)

type Invocation struct {
	Operation   Operation
	Arguments   []string
	Options     map[string]any
	ConfigDir   string
	JSON        bool
	Verbose     bool
	Yes         bool
	Interactive bool
}

// Result is the finite result shared by human, JSON, and TUI presentations.
// Human is optional presentation text. Data is always the JSON representation.
type Result struct {
	Data     any
	Human    []string
	ExitCode int
	// Streamed is true when EventSink already received exactly one terminal
	// event and no additional finite envelope may be written.
	Streamed bool
	// Attachment waits after the finite startup transaction has committed and
	// its mutation lock has been released. It is never serialized.
	Attachment func(context.Context) error
}

// Event represents one item from a streaming operation such as OAuth or log
// following. Terminal events are still followed by the finite Result returned
// from Execute.
type Event struct {
	Type       string    `json:"type"`
	InstanceID string    `json:"instance_id,omitempty"`
	Timestamp  time.Time `json:"timestamp,omitempty"`
	Data       any       `json:"data,omitempty"`
	Human      string    `json:"-"`
}

// EventSink receives streaming events in call order.
type EventSink func(Event) error

// UseCases is the sole command-to-application composition seam. Concrete
// infrastructure is injected at the composition root; Cobra handlers never
// import adapters.
type UseCases interface {
	Execute(context.Context, Invocation, EventSink) (Result, error)
}

// UseCaseFunc adapts a function for focused composition and command tests.
type UseCaseFunc func(context.Context, Invocation, EventSink) (Result, error)

func (f UseCaseFunc) Execute(ctx context.Context, invocation Invocation, sink EventSink) (Result, error) {
	return f(ctx, invocation, sink)
}

// UnavailableUseCases fails closed when the composition root cannot provide
// the infrastructure required by an operation. It deliberately never reports
// success or behaves as a no-op.
type UnavailableUseCases struct{}

func (UnavailableUseCases) Execute(_ context.Context, invocation Invocation, _ EventSink) (Result, error) {
	op := invocation.Operation
	if op == "" {
		op = "requested operation"
	}
	err := pmuxerr.New(pmuxerr.CodeDependencyMissing, pmuxerr.Environment,
		fmt.Sprintf("PMux cannot run %q because its application services are unavailable.", op))
	err.Explanation = "The command was constructed without the required application use cases."
	err.Repair = []string{"Install or run a complete PMux build, then retry."}
	return Result{}, err
}

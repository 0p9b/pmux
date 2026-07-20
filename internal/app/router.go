package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
	domainconfig "github.com/0p9b/pmux/internal/domain/config"
	"github.com/0p9b/pmux/internal/domain/management"
	domainmodel "github.com/0p9b/pmux/internal/domain/model"
	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	"github.com/0p9b/pmux/internal/domain/provider"
	domainservice "github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
	"github.com/0p9b/pmux/internal/state"
)

// StateStore is the versioned local state boundary used by the command router.
// The concrete state.Store satisfies it without exposing paths or secret values.
type StateStore interface {
	LoadConfig() (state.Config, error)
	SaveConfig(state.Config) error
	LoadState() (state.State, error)
	SaveState(state.State) error
	LoadSecretReferences() (state.SecretReferences, error)
}

type SetupRequest struct {
	Mode, ProxyPath, ConfigPath string
	Harden, Yes, Interactive    bool
}

type SetupOutcome struct {
	Installation state.Installation `json:"installation"`
	CoreComplete bool               `json:"core_complete"`
	Hardened     bool               `json:"hardened"`
	NextActions  []string           `json:"next_actions,omitempty"`
}

type SetupService interface {
	Setup(context.Context, SetupRequest) (SetupOutcome, error)
}
type ManagementFactory func(context.Context, state.Installation) (management.ManagementClient, error)
type ModelFactory func(context.Context, state.Installation) (domainmodel.ModelCatalog, error)
type ServiceFactory func(context.Context, state.Installation, bool) (domainservice.ServiceManager, error)
type ConfigFactory func(context.Context, state.Installation) (domainconfig.ConfigFile, error)
type AuthFactory func(context.Context, state.Installation, management.ProviderID) (provider.ProviderAuthenticator, error)
type LauncherFactory func(context.Context, state.Installation, Invocation) (domainclient.ClientLauncher, error)
type SecretLoader func(context.Context, state.Installation) ([]byte, error)

type ModelTester interface {
	Test(context.Context, state.Installation, string, time.Duration) (any, error)
}
type ConfigEditRequest struct {
	Scope   string
	Target  string
	Editor  string
	Confirm func(diff string) (bool, error)
}

type ConfigEditResult struct {
	Path            string `json:"path"`
	Diff            string `json:"diff"`
	BackupPath      string `json:"backup_path,omitempty"`
	RestartRequired bool   `json:"restart_required"`
}

type ConfigMaintenance interface {
	Backup(context.Context, string) (string, error)
	Restore(context.Context, string, string) (any, error)
	Edit(context.Context, ConfigEditRequest) (ConfigEditResult, error)
}
type PMuxConfigRestorePlan struct {
	ID          string
	Fingerprint [32]byte
	Config      state.Config
	Diff        string
}

type PMuxConfigMaintenance interface {
	BackupPMux(context.Context) (string, error)
	PlanRestorePMux(context.Context, string, state.Config) (PMuxConfigRestorePlan, error)
	RestorePMux(context.Context, PMuxConfigRestorePlan) error
}
type DoctorService interface {
	Run(context.Context, state.Installation, []string, []string, bool, bool, bool) (any, bool, error)
}
type BundleService interface {
	Bundle(context.Context, state.Installation, string) (any, error)
}
type UpdateService interface {
	Check(context.Context, string, state.Installation) (any, error)
	Self(context.Context, string) (any, error)
	Proxy(context.Context, state.Installation, string) (any, error)
}

// Dependencies are concrete infrastructure ports. Factories are installation
// scoped so no adapter may accidentally act on a different instance.
type Dependencies struct {
	Roots             domainplatform.Roots
	Store             StateStore
	Setup             SetupService
	Management        ManagementFactory
	Models            ModelFactory
	Services          ServiceFactory
	Configs           ConfigFactory
	Auth              AuthFactory
	Launcher          LauncherFactory
	Secrets           SecretLoader
	KnownSecrets      func(context.Context, state.Installation) ([][]byte, error)
	ModelTester       ModelTester
	ConfigFiles       ConfigMaintenance
	PMuxConfig        PMuxConfigMaintenance
	Doctor            DoctorService
	Bundle            BundleService
	Updates           UpdateService
	Input             io.Reader
	Output            io.Writer
	VerifyPrivateFile func(string) error
	ReadPassword      func(context.Context, string) ([]byte, error)
	WorkingDir        func() (string, error)
	Now               func() time.Time
}

// Router is the concrete command-to-use-case implementation used by both CLI
// and TUI presentation. It performs no work until Execute is called.
type Router struct{ deps Dependencies }

func NewRouter(deps Dependencies) (*Router, error) {
	if deps.Store == nil {
		return nil, dependencyError("PMux state storage is unavailable.", "Run PMux with a writable canonical state directory.")
	}
	for name, value := range map[string]string{"config": deps.Roots.Config, "state": deps.Roots.State, "cache": deps.Roots.Cache, "data": deps.Roots.Data} {
		if strings.TrimSpace(value) == "" {
			return nil, typedConfig(fmt.Sprintf("The canonical %s root is unavailable.", name), "Check the platform home and XDG environment, then retry.")
		}
	}
	if deps.Input == nil {
		deps.Input = os.Stdin
	}
	if deps.Output == nil {
		deps.Output = io.Discard
	}
	if deps.VerifyPrivateFile == nil {
		deps.VerifyPrivateFile = func(path string) error {
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("protected input is not a regular file")
			}
			if info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("protected input permissions are not private")
			}
			return nil
		}
	}
	if deps.WorkingDir == nil {
		deps.WorkingDir = func() (string, error) { return ".", nil }
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Router{deps: deps}, nil
}

// New is a concise alias retained for composition and package-local fakes.
func New(deps Dependencies) (*Router, error) { return NewRouter(deps) }

func (r *Router) Execute(ctx context.Context, in Invocation, sink EventSink) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, canceled(err)
	}
	cfg, err := r.deps.Store.LoadConfig()
	if err != nil {
		return Result{}, normalize(err, pmuxerr.ConfigUnreadable, "PMux could not load its versioned configuration.")
	}
	st, err := r.deps.Store.LoadState()
	if err != nil {
		return Result{}, normalize(err, pmuxerr.ConfigUnreadable, "PMux could not load its versioned state.")
	}
	installation, configured := selectInstallation(st, cfg.DefaultInstallation)

	switch in.Operation {
	case OpDashboardStatus, OpTUIDashboard:
		return r.dashboard(ctx, installation, configured)
	case OpSetup:
		return r.setup(ctx, in)
	case OpProvidersList, OpTUIProviders:
		return r.providersList(ctx, installation, configured)
	case OpProvidersLogin:
		return r.providerLogin(ctx, in, sink, installation, configured)
	case OpProvidersVerify:
		return r.providersVerify(ctx, in, installation, configured)
	case OpProvidersEnable, OpProvidersDisable, OpProvidersRemove:
		return r.providerMutation(ctx, in, installation, configured)
	case OpModelsList, OpTUIModels:
		return r.modelsList(ctx, in, installation, configured)
	case OpModelsFavorite, OpModelsUnfavorite:
		return r.modelFavorite(in, st)
	case OpModelsTest:
		return r.modelTest(ctx, in, installation, configured)
	case OpServiceStatus, OpTUIService:
		return r.serviceStatus(ctx, installation, configured)
	case OpServiceStart, OpServiceStop, OpServiceRestart, OpServiceInstall, OpServiceUninstall, OpServiceLogs:
		return r.serviceMutation(ctx, in, sink, installation, configured)
	case OpConfigShow, OpTUIConfig:
		return r.configShow(ctx, in, cfg, installation, configured)
	case OpConfigGet:
		return r.configGet(ctx, in, cfg, installation, configured)
	case OpConfigSet:
		return r.configSet(ctx, in, sink, cfg, installation, configured)
	case OpConfigBackup, OpConfigRestore:
		return r.configMaintenance(ctx, in, sink, installation, configured)
	case OpConfigEdit:
		return r.configEdit(ctx, in, installation, configured)
	case OpLaunch:
		return r.launch(ctx, in, installation, configured)
	case OpLaunchPreflight:
		return r.launchPreflight(ctx, in, installation, configured)
	case OpDoctor:
		return r.doctor(ctx, in, installation, configured)
	case OpUpdateCheck, OpUpdateSelf, OpUpdateProxy:
		return r.update(ctx, in, installation, configured)
	default:
		return Result{}, typedUsage(fmt.Sprintf("Unsupported application operation %q.", in.Operation))
	}
}

func (r *Router) dashboard(ctx context.Context, installation state.Installation, configured bool) (Result, error) {
	data := map[string]any{"configured": configured, "roots": r.deps.Roots, "network_activity": false}
	human := []string{"PMux is not configured. Run `pmux setup --mode managed` or adopt an existing installation."}
	if configured {
		data["installation"] = safeInstallation(installation)
		status, err := r.readServiceStatus(ctx, installation)
		if err != nil {
			data["service_error"] = safeError(err)
		} else {
			data["service"] = status
		}
		human = []string{fmt.Sprintf("CLIProxyAPI %s (%s)", valueOr(installation.CoreVersionSeen, "unknown"), installation.Kind)}
	}
	return Result{Data: data, Human: human}, nil
}

func (r *Router) setup(ctx context.Context, in Invocation) (Result, error) {
	if r.deps.Setup == nil {
		return Result{}, dependencyError("Setup infrastructure is unavailable.", "Install a complete PMux build and retry `pmux setup`.")
	}
	req := SetupRequest{Mode: optionString(in, "mode"), ProxyPath: optionString(in, "proxy_path"), ConfigPath: optionString(in, "config_path"), Harden: optionBool(in, "harden"), Yes: in.Yes, Interactive: in.Interactive}
	if req.Mode == "" {
		return Result{}, typedUsage("Setup requires an explicit managed or adopt mode in this presentation.")
	}
	if req.Mode != "managed" && req.Mode != "adopt" {
		return Result{}, typedUsage("Setup mode must be `managed` or `adopt`.")
	}
	if req.Mode == "adopt" && req.ProxyPath == "" {
		return Result{}, typedUsage("Adoption requires `--proxy-path`; no files were changed.")
	}
	if req.Harden && !req.Interactive && !req.Yes {
		return Result{}, typedUsage("Noninteractive hardening requires `--harden --yes`; no changes were made.")
	}
	out, err := r.deps.Setup.Setup(ctx, req)
	if err != nil {
		return Result{}, ensureTyped(err, "Setup failed before it could be verified.")
	}
	return Result{Data: out, Human: []string{fmt.Sprintf("CLIProxyAPI %s setup is complete.", out.Installation.ID)}}, nil
}

type providerStatus struct {
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Flows    []provider.AuthFlow `json:"flows"`
	Accounts int                 `json:"accounts"`
	Usable   int                 `json:"usable"`
	Status   string              `json:"status"`
}

func (r *Router) providersList(ctx context.Context, installation state.Installation, configured bool) (Result, error) {
	definitions := provider.Registry()
	rows := make([]providerStatus, 0, len(definitions))
	files := []management.AuthFile(nil)
	if configured {
		client, err := r.management(ctx, installation)
		if err != nil {
			return Result{}, err
		}
		files, err = client.AuthFiles(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not read provider status from CLIProxyAPI.")
		}
	}
	for _, definition := range definitions {
		row := providerStatus{ID: string(definition.ID), Name: definition.Name, Flows: append([]provider.AuthFlow(nil), definition.Flows...), Status: "not-configured"}
		for _, file := range files {
			if file.Provider == definition.ID {
				row.Accounts++
				if !file.Disabled && usableStatus(file.Status) {
					row.Usable++
				}
			}
		}
		if row.Accounts > 0 {
			row.Status = "unavailable"
			if row.Usable > 0 {
				row.Status = "authenticated"
			}
		}
		rows = append(rows, row)
	}
	return Result{Data: map[string]any{"providers": rows, "configured": configured}, Human: []string{fmt.Sprintf("%d providers (%d configured accounts)", len(rows), len(files))}}, nil
}

func (r *Router) providersVerify(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("verify providers", "Run `pmux setup --mode managed` first.")
	}
	client, err := r.management(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	files, err := client.AuthFiles(ctx)
	if err != nil {
		return Result{}, ensureTyped(err, "Provider verification failed.")
	}
	providerID := ""
	if len(in.Arguments) > 0 && !strings.EqualFold(in.Arguments[0], "all") {
		providerID = in.Arguments[0]
	}
	account := optionString(in, "account")
	selected := make([]management.AuthFile, 0)
	usable := 0
	for _, file := range files {
		if providerID != "" && string(file.Provider) != providerID {
			continue
		}
		if account != "" && file.Name != account {
			continue
		}
		selected = append(selected, file)
		if !file.Disabled && usableStatus(file.Status) {
			usable++
		}
	}
	data := map[string]any{
		"accounts": selected,
		"usable":   usable,
		"total":    len(selected),
	}
	if optionBool(in, "refresh_models") {
		catalog, catalogErr := r.models(ctx, installation)
		if catalogErr != nil {
			data["model_refresh"] = map[string]any{"count": 0, "status": "failed", "error": safeErrorMessage(catalogErr)}
			result := Result{Data: data, Human: []string{fmt.Sprintf("Verified %d provider credential record(s); %d usable. Live model refresh failed.", len(selected), usable)}}
			return result, unhealthy("Provider credential status was read, but live model refresh failed.", "Retry `pmux providers verify --refresh-models`.")
		}
		entries, refreshErr := catalog.Refresh(ctx)
		if refreshErr != nil {
			data["model_refresh"] = map[string]any{"count": 0, "status": "failed", "error": safeErrorMessage(refreshErr)}
			result := Result{Data: data, Human: []string{fmt.Sprintf("Verified %d provider credential record(s); %d usable. Live model refresh failed.", len(selected), usable)}}
			return result, unhealthy("Provider credential status was read, but live model refresh failed.", "Retry `pmux providers verify --refresh-models`.")
		}
		data["model_refresh"] = map[string]any{"count": len(entries), "status": "complete"}
	}
	result := Result{
		Data:  data,
		Human: []string{fmt.Sprintf("Verified %d provider credential record(s); %d usable.", len(selected), usable)},
	}
	if providerID != "" && usable == 0 {
		return result, authError(fmt.Sprintf("Provider %q has no usable configured credential.", providerID), fmt.Sprintf("Run `pmux providers login %s`.", providerID))
	}
	if providerID == "" && len(selected) > usable {
		return result, unhealthy("Provider verification found one or more unavailable credential records.", "Run `pmux providers verify <provider>` for a targeted result, then reauthenticate or enable unavailable accounts.")
	}
	return result, nil
}

func (r *Router) providerLogin(ctx context.Context, in Invocation, sink EventSink, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("authenticate a provider", "Run `pmux setup --mode managed` first.")
	}
	if len(in.Arguments) != 1 {
		return Result{}, typedUsage("Provider login requires one provider ID.")
	}
	if r.deps.Auth == nil {
		return Result{}, dependencyError("Provider authentication is unavailable.", "Run `pmux doctor` to inspect the management capability.")
	}

	method := valueOr(optionString(in, "method"), "auto")
	if method != "auto" && method != "browser" && method != "device" {
		return Result{}, typedUsage("Provider authentication method must be auto, browser, or device.")
	}
	apiKeyFile := optionString(in, "api_key_file")
	apiKeyStdin := optionBool(in, "api_key_stdin")
	callbackStdin := optionBool(in, "callback_url_stdin")
	serviceAccount := optionString(in, "service_account")
	vertexPrefix := optionString(in, "vertex_prefix")
	noBrowser := optionBool(in, "no_browser")
	if apiKeyFile != "" && apiKeyStdin {
		return Result{}, typedUsage("Use exactly one protected API-key input: `--api-key-file` or `--api-key-stdin`.")
	}
	if vertexPrefix != "" && serviceAccount == "" {
		return Result{}, typedUsage("`--vertex-prefix` requires `--service-account`.")
	}

	apiKeyRoute := apiKeyFile != "" || apiKeyStdin
	vertexRoute := serviceAccount != ""
	selectedRoutes := 0
	for _, selected := range []bool{apiKeyRoute, vertexRoute, callbackStdin} {
		if selected {
			selectedRoutes++
		}
	}
	if selectedRoutes > 1 {
		return Result{}, typedUsage("API-key, Vertex import, and pasted-callback inputs are mutually exclusive.")
	}
	if (apiKeyRoute || vertexRoute) && (method != "auto" || noBrowser) {
		return Result{}, typedUsage("`--method` and `--no-browser` apply only to OAuth provider login.")
	}
	if callbackStdin && method == "device" {
		return Result{}, typedUsage("`--callback-url-stdin` requires browser callback authentication, not device authorization.")
	}

	providerID := management.ProviderID(in.Arguments[0])
	authenticator, err := r.deps.Auth(ctx, installation, providerID)
	if err != nil {
		return Result{}, ensureTyped(err, "Provider authentication could not be prepared.")
	}
	flows := authenticator.Flows()
	autoAPIKey := !apiKeyRoute && !vertexRoute && !callbackStdin && method == "auto" &&
		slices.Contains(flows, provider.FlowAPIKey) &&
		!slices.Contains(flows, provider.FlowBrowser) &&
		!slices.Contains(flows, provider.FlowDeviceCode)
	if autoAPIKey {
		if !in.Interactive {
			return Result{}, typedUsage("Noninteractive API-key configuration requires `--api-key-file` or `--api-key-stdin`; no changes were made.")
		}
		apiKeyRoute = true
	}

	if apiKeyRoute {
		if !slices.Contains(flows, provider.FlowAPIKey) {
			return Result{}, typedConfig(fmt.Sprintf("Provider %q does not support protected API-key authentication.", providerID), "Run `pmux providers list` to inspect supported methods.")
		}
		if !in.Interactive && !in.Yes {
			return Result{}, typedUsage("Noninteractive API-key configuration requires `--yes`; no changes were made.")
		}
		var input provider.ProtectedInput
		if apiKeyFile != "" {
			input = &fileProtectedInput{path: apiKeyFile, verify: r.deps.VerifyPrivateFile}
		} else if protected, ok := in.Options["protected_input"].(io.Reader); ok && protected != nil {
			input = &readerProtectedInput{reader: protected}
		} else if autoAPIKey {
			if r.deps.ReadPassword == nil {
				return Result{}, dependencyError("Protected terminal input is unavailable.", "Retry with `--api-key-file` or `--api-key-stdin`.")
			}
			input = &promptProtectedInput{read: r.deps.ReadPassword, prompt: fmt.Sprintf("API key for %s: ", providerID)}
		} else {
			input = &readerProtectedInput{reader: r.deps.Input}
		}
		key, applyErr := authenticator.ApplyAPIKey(ctx, providerKeyApplication(in, input))
		if applyErr != nil {
			return Result{}, ensureTyped(applyErr, "The protected provider API key could not be applied.")
		}
		data := map[string]any{"provider": providerID, "status": "configured", "id": key.ID}
		if key.Label != "" {
			data["label"] = key.Label
		}
		if key.Mask != "" {
			data["mask"] = key.Mask
		}
		return Result{Data: data, Human: []string{fmt.Sprintf("Configured a protected API key for %s.", providerID)}}, nil
	}

	if vertexRoute {
		if !slices.Contains(flows, provider.FlowVertexImport) {
			return Result{}, typedConfig(fmt.Sprintf("Provider %q does not support Vertex service-account import.", providerID), "Run `pmux providers list` to inspect supported methods.")
		}
		if !in.Interactive && !in.Yes {
			return Result{}, typedUsage("Noninteractive Vertex import requires `--yes`; no changes were made.")
		}
		imported, importErr := authenticator.ImportVertex(ctx, provider.VertexImport{Path: serviceAccount, Prefix: vertexPrefix})
		if importErr != nil {
			return Result{}, ensureTyped(importErr, "The Vertex service account could not be imported.")
		}
		return Result{
			Data:  map[string]any{"provider": providerID, "status": "configured", "credential": imported.Name},
			Human: []string{"Imported the Vertex service account and verified its credential."},
		}, nil
	}

	flow := selectFlow(flows, method)
	if callbackStdin {
		flow = provider.FlowPasteCallback
		if !slices.Contains(flows, provider.FlowBrowser) {
			return Result{}, typedConfig(fmt.Sprintf("Provider %q does not support pasted callback authentication.", providerID), "Run `pmux providers list` to inspect supported methods.")
		}
	}
	if !callbackStdin && noBrowser && flow == provider.FlowBrowser {
		// Paste-callback is the contract's headless callback variant. It keeps
		// Management API polling unchanged and selects -no-browser only if the
		// authenticator must use its closed subprocess fallback.
		flow = provider.FlowPasteCallback
	}
	if flow == "" {
		return Result{}, typedConfig(fmt.Sprintf("Provider %q does not support authentication method %q.", providerID, method), "Run `pmux providers list` to inspect supported methods.")
	}

	session, err := authenticator.Begin(ctx, flow)
	if err != nil {
		return Result{}, ensureTyped(err, "Provider authentication could not start.")
	}
	if err := emit(sink, Event{Type: "auth_started", Timestamp: r.deps.Now(), Data: map[string]any{"provider": providerID, "flow": flow}}); err != nil {
		_ = authenticator.Cancel(context.Background(), session)
		return Result{}, sinkError(err)
	}
	challenge := session.Challenge
	if challenge.URL != "" || challenge.VerificationURI != "" || challenge.UserCode != "" {
		if err := emit(sink, Event{Type: "verification_required", Timestamp: r.deps.Now(), Data: safeChallenge(challenge)}); err != nil {
			_ = authenticator.Cancel(context.Background(), session)
			return Result{}, sinkError(err)
		}
	}
	var status management.OAuthStatus
	interactiveCallback := !callbackStdin && in.Interactive && (flow == provider.FlowBrowser || flow == provider.FlowPasteCallback)
	if callbackStdin || interactiveCallback {
		var callbackBytes []byte
		var readErr error
		if callbackStdin {
			reader := r.deps.Input
			if protected, ok := in.Options["protected_input"].(io.Reader); ok && protected != nil {
				reader = protected
			}
			callbackBytes, readErr = readProtectedBytes(ctx, reader)
		} else {
			if emitErr := emit(sink, Event{
				Type:      "protected_input_required",
				Timestamp: r.deps.Now(),
				Data:      map[string]any{"provider": providerID, "kind": "callback_url"},
				Human:     "Paste the full callback URL from your browser's address bar:",
			}); emitErr != nil {
				_ = authenticator.Cancel(context.Background(), session)
				return Result{}, sinkError(emitErr)
			}
			if protected, ok := in.Options["interactive_protected_input"].(io.Reader); ok && protected != nil {
				callbackBytes, readErr = readProtectedBytes(ctx, protected)
			} else if r.deps.ReadPassword == nil {
				readErr = dependencyError("Protected terminal input is unavailable.", "Retry with `--callback-url-stdin`.")
			} else {
				callbackBytes, readErr = r.deps.ReadPassword(ctx, "Paste the full callback URL from your browser's address bar: ")
			}
		}
		if readErr != nil {
			clearBytes(callbackBytes)
			_ = authenticator.Cancel(context.Background(), session)
			return Result{}, normalize(readErr, pmuxerr.ConfigUnreadable, "Could not read the protected callback URL.")
		}
		callbackURL := strings.TrimSpace(string(callbackBytes))
		clearBytes(callbackBytes)
		if callbackURL == "" {
			_ = authenticator.Cancel(context.Background(), session)
			return Result{}, typedUsage("The protected callback URL is empty.")
		}
		status, err = authenticator.CompletePaste(ctx, session, callbackURL)
	} else {
		status, err = authenticator.Poll(ctx, session)
	}

	for {
		if err != nil {
			_ = authenticator.Cancel(context.Background(), session)
			return Result{}, ensureTyped(err, "Provider authentication did not complete.")
		}
		switch strings.ToLower(status.Status) {
		case "complete", "success", "authenticated":
			_ = emit(sink, Event{Type: "complete", Timestamp: r.deps.Now(), Data: map[string]any{"provider": providerID, "status": status.Status}})
			return Result{Data: map[string]any{"provider": providerID, "status": status.Status}, Streamed: sink != nil}, nil
		case "failed", "error", "denied", "expired", "canceled":
			_ = authenticator.Cancel(context.Background(), session)
			return Result{}, authError("Authentication did not complete: "+valueOr(status.Message, status.Status)+". No usable credential was recorded.", fmt.Sprintf("Retry with `pmux providers login %s`.", providerID))
		default:
			if emitErr := emit(sink, Event{Type: "waiting", Timestamp: r.deps.Now(), Data: map[string]any{"provider": providerID, "status": status.Status}}); emitErr != nil {
				_ = authenticator.Cancel(context.Background(), session)
				return Result{}, sinkError(emitErr)
			}
		}
		status, err = authenticator.Poll(ctx, session)
	}
}

func (r *Router) providerMutation(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("change provider state", "Run `pmux setup --mode managed` first.")
	}
	if len(in.Arguments) == 0 {
		return Result{}, typedUsage("A provider ID is required.")
	}
	if !in.Interactive && !in.Yes {
		return Result{}, typedUsage("Noninteractive provider mutation requires `--yes`; no changes were made.")
	}
	client, err := r.management(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	files, err := client.AuthFiles(ctx)
	if err != nil {
		return Result{}, ensureTyped(err, "PMux could not inspect provider credentials before mutation.")
	}
	providerID := management.ProviderID(in.Arguments[0])
	account := ""
	if len(in.Arguments) > 1 {
		account = in.Arguments[1]
	}
	selected := make([]management.AuthFile, 0)
	for _, file := range files {
		if file.Provider == providerID && (account == "" || file.Name == account) {
			selected = append(selected, file)
		}
	}
	if len(selected) == 0 {
		return Result{}, authError(fmt.Sprintf("Provider %q has no matching credential.", providerID), "Run `pmux providers list` and choose an existing account.")
	}
	if in.Interactive && !in.Yes {
		phrase := ""
		switch in.Operation {
		case OpProvidersDisable:
			phrase = "disable"
		case OpProvidersRemove:
			if account == "" {
				phrase = string(providerID)
			} else {
				phrase = "remove"
			}
		}
		if phrase != "" {
			ok, confirmErr := r.confirmPhrase(phrase)
			if confirmErr != nil {
				return Result{}, confirmErr
			}
			if !ok {
				return Result{}, canceled(errors.New("provider mutation was not confirmed"))
			}
		}
	}
	switch in.Operation {
	case OpProvidersRemove:
		if optionBool(in, "keep_credentials") {
			return Result{}, typedConfig("`--keep-credentials` leaves no supported provider-level mutation for this configured state.", "Disable the provider instead, or remove the selected credential without `--keep-credentials`.")
		}
		names := make([]string, len(selected))
		for i := range selected {
			names[i] = selected[i].Name
		}
		if err := client.DeleteAuthFiles(ctx, names, false); err != nil {
			return Result{}, ensureTyped(err, "Provider credential removal failed.")
		}
	default:
		disabled := in.Operation == OpProvidersDisable
		for _, file := range selected {
			body, _ := json.Marshal(map[string]any{"name": file.Name, "disabled": disabled})
			if err := client.PatchAuthFileStatus(ctx, management.AuthFileStatusPatch(body)); err != nil {
				return Result{}, ensureTyped(err, "Provider status mutation failed.")
			}
		}
	}
	return Result{Data: map[string]any{"provider": providerID, "accounts": len(selected), "operation": in.Operation}, Human: []string{fmt.Sprintf("%s %d %s account(s).", in.Operation, len(selected), providerID)}}, nil
}

func (r *Router) modelsList(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{Data: map[string]any{"models": []domainmodel.CatalogEntry{}, "source": "none", "stale": false}, Human: []string{"No models are cached because PMux is not configured."}}, nil
	}
	catalog, err := r.models(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	var entries []domainmodel.CatalogEntry
	if optionBool(in, "refresh") {
		entries, err = catalog.Refresh(ctx)
	} else {
		entries, err = catalog.List(ctx)
	}
	if err != nil {
		return Result{}, ensureTyped(err, "PMux could not load the dynamic model catalog.")
	}
	entries = filterModels(entries, optionString(in, "provider"), optionString(in, "search"), optionBool(in, "available"), optionBool(in, "favorite"))
	source, stale := "cache", false
	if len(entries) > 0 {
		source, stale = entries[0].Source, entries[0].Stale
	}
	return Result{Data: map[string]any{"models": entries, "source": source, "stale": stale}, Human: []string{fmt.Sprintf("%d dynamically discovered model(s).", len(entries))}}, nil
}

func (r *Router) modelFavorite(in Invocation, st state.State) (Result, error) {
	if len(in.Arguments) != 1 {
		return Result{}, typedUsage("An exact model ID is required.")
	}
	id := in.Arguments[0]
	index := slices.Index(st.Favorites, id)
	if in.Operation == OpModelsFavorite && index < 0 {
		st.Favorites = append(st.Favorites, id)
		slices.Sort(st.Favorites)
	}
	if in.Operation == OpModelsUnfavorite && index >= 0 {
		st.Favorites = append(st.Favorites[:index], st.Favorites[index+1:]...)
	}
	if err := r.deps.Store.SaveState(st); err != nil {
		return Result{}, normalize(err, pmuxerr.ConfigMutationConflict, "PMux could not update model favorites.")
	}
	return Result{Data: map[string]any{"id": id, "favorite": in.Operation == OpModelsFavorite}, Human: []string{fmt.Sprintf("Model %s favorite=%t.", id, in.Operation == OpModelsFavorite)}}, nil
}

func (r *Router) modelTest(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("test a model", "Run `pmux setup --mode managed` first.")
	}
	if r.deps.ModelTester == nil {
		return Result{}, dependencyError("Model testing is unavailable.", "Run `pmux models list --refresh` to verify discovery instead.")
	}
	if len(in.Arguments) != 1 {
		return Result{}, typedUsage("An exact model ID is required.")
	}
	timeout := 30 * time.Second
	if raw := optionString(in, "timeout"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return Result{}, typedUsage("--timeout must be a positive duration.")
		}
		timeout = parsed
	}
	catalog, err := r.models(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	entries, err := catalog.Refresh(ctx)
	if err != nil {
		return Result{}, ensureTyped(err, "PMux could not refresh the live model catalog before testing.")
	}
	modelID := in.Arguments[0]
	entryIndex := slices.IndexFunc(entries, func(entry domainmodel.CatalogEntry) bool { return entry.ID == modelID })
	if entryIndex < 0 {
		return Result{}, typedUsage(fmt.Sprintf("Model %q is not in the current live catalog; run `pmux models list --refresh`.", modelID))
	}
	entry := entries[entryIndex]
	if entry.Stale {
		staleErr := pmuxerr.New(pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Live model availability could not be verified; the matching catalog entry is stale.")
		staleErr.Repair = []string{"Restore CLIProxyAPI connectivity, then run `pmux models list --refresh` before testing a model."}
		return Result{}, staleErr
	}
	if !entry.Available {
		return Result{}, authError(fmt.Sprintf("Model %q is not currently available.", modelID), "Run `pmux providers verify --refresh-models`, then refresh the model catalog.")
	}
	if provider := management.ProviderID(optionString(in, "provider")); provider != "" {
		if !slices.Contains(entry.Providers, provider) {
			if len(entry.Providers) == 0 || slices.Contains(entry.Providers, management.ProviderID("Unknown")) {
				return Result{}, authError(fmt.Sprintf("Provider attribution for model %q is unavailable; provider %q cannot be verified.", modelID, provider), "Run `pmux models list --refresh`; use `--provider` only when management attribution is available.")
			}
			return Result{}, authError(fmt.Sprintf("Model %q is not attributed to provider %q in the current live catalog.", modelID, provider), fmt.Sprintf("Run `pmux models list --provider %s --refresh` and choose an attributed model.", provider))
		}
	}
	value, err := r.deps.ModelTester.Test(ctx, installation, in.Arguments[0], timeout)
	if err != nil {
		return Result{}, ensureTyped(err, "Model test failed.")
	}
	return Result{Data: value, Human: []string{"Model test passed."}}, nil
}

func (r *Router) serviceStatus(ctx context.Context, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		status := domainservice.ServiceStatus{Backend: domainservice.BackendForeground, State: domainservice.ServiceNotInstalled, CoreVersion: "unknown"}
		return Result{Data: status, Human: []string{"CLIProxyAPI service is not installed. Run `pmux setup --mode managed`."}}, nil
	}
	status, err := r.readServiceStatus(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: status, Human: []string{fmt.Sprintf("Service %s (%s).", status.State, status.Backend)}}, nil
}

type attachedServiceStarter interface {
	StartAttached(context.Context) (func(context.Context) error, error)
}

func (r *Router) serviceMutation(ctx context.Context, in Invocation, sink EventSink, installation state.Installation, configured bool) (Result, error) {
	if !configured {
		return Result{}, notConfigured("manage the service", "Run `pmux setup --mode managed` or adopt an installation first.")
	}
	if in.Operation == OpServiceLogs {
		return r.serviceLogs(ctx, in, sink, installation)
	}
	foreground := optionBool(in, "foreground")
	manager, err := r.service(ctx, installation, foreground)
	if err != nil {
		return Result{}, err
	}
	timeout := 15 * time.Second
	if raw := optionString(in, "timeout"); raw != "" {
		parsed, parseErr := time.ParseDuration(raw)
		if parseErr != nil || parsed <= 0 {
			return Result{}, typedUsage("--timeout must be a positive duration.")
		}
		timeout = parsed
	}
	if (in.Operation == OpServiceStop || in.Operation == OpServiceRestart || in.Operation == OpServiceUninstall) && !in.Interactive && !in.Yes {
		return Result{}, typedUsage("Noninteractive service mutation requires `--yes`; no changes were made.")
	}
	if in.Operation == OpServiceUninstall && installation.Kind != "managed" {
		return Result{}, ownershipError("PMux will not uninstall an adopted service definition.", "Use the service's owning tool or complete adoption hardening first.")
	}
	current, err := manager.Status(ctx)
	if err != nil {
		return Result{}, ensureTyped(err, "PMux could not inspect service state before the requested mutation.")
	}
	if in.Interactive && !in.Yes {
		phrase := ""
		switch in.Operation {
		case OpServiceStop:
			if current.State != domainservice.ServiceStopped && current.State != domainservice.ServiceNotInstalled {
				phrase = "stop"
			}
		case OpServiceRestart:
			phrase = "restart"
		case OpServiceUninstall:
			if current.State != domainservice.ServiceNotInstalled {
				phrase = "uninstall"
			}
		}
		if phrase != "" {
			ok, confirmErr := r.confirmPhrase(phrase)
			if confirmErr != nil {
				return Result{}, confirmErr
			}
			if !ok {
				return Result{}, canceled(errors.New("service mutation was not confirmed"))
			}
		}
	}
	var attachment func(context.Context) error
	switch in.Operation {
	case OpServiceStart:
		if foreground {
			if starter, ok := manager.(attachedServiceStarter); ok {
				attachment, err = starter.StartAttached(ctx)
			} else {
				err = manager.Start(ctx)
			}
		} else {
			err = manager.Start(ctx)
		}
		if err != nil {
			return Result{}, ensureTyped(err, "CLIProxyAPI service could not be started.")
		}
	case OpServiceStop:
		if err := manager.Stop(ctx, timeout); err != nil {
			return Result{}, ensureTyped(err, "CLIProxyAPI service could not be stopped.")
		}
	case OpServiceRestart:
		status, err := manager.Restart(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "CLIProxyAPI service could not be restarted and verified.")
		}
		return Result{Data: status, Human: []string{"CLIProxyAPI restarted and passed its health gate."}}, nil
	case OpServiceInstall:
		if installation.Kind != "managed" {
			return Result{}, ownershipError("PMux will not install over an adopted service definition.", "Run the separately previewed adoption hardening transaction.")
		}
		spec := serviceSpec(r.deps.Roots, installation, manager.Backend())
		if err := manager.Install(ctx, spec); err != nil {
			return Result{}, ensureTyped(err, "CLIProxyAPI service definition could not be installed.")
		}
		if optionBool(in, "start") {
			if err := manager.Start(ctx); err != nil {
				return Result{}, ensureTyped(err, "The service was installed but did not start successfully.")
			}
		}
	case OpServiceUninstall:
		if err := manager.Uninstall(ctx); err != nil {
			return Result{}, ensureTyped(err, "CLIProxyAPI service definition could not be uninstalled.")
		}
	default:
		return Result{}, typedUsage("Unsupported service mutation.")
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return Result{}, ensureTyped(err, "Service mutation completed, but status verification failed.")
	}
	return Result{Data: status, Human: []string{fmt.Sprintf("Service is %s.", status.State)}, Attachment: attachment}, nil
}

type serviceLogEntry struct {
	Source    string    `json:"source"`
	Level     string    `json:"level,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

var (
	logCredentialPattern = regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_-]{8,}\b`)
	logBearerPattern     = regexp.MustCompile(`(?i)(Bearer\s+)(\S+)`)
	logSecretField       = regexp.MustCompile(`(?i)((?:api[-_]?key|secret[-_]?key|private[-_]?key|access_token|refresh_token|password|ANTHROPIC_AUTH_TOKEN)\s*[=:]\s*)(\S+)`)
	logPrivateKeyMarker  = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----|-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	logPrivateKeyBody    = regexp.MustCompile(`\b[A-Za-z0-9+/]{40,}={0,2}\b`)
	logLevelPattern      = regexp.MustCompile(`(?i)\b(debug|info|warn(?:ing)?|error|critical)\b`)
)

func (r *Router) serviceLogs(ctx context.Context, in Invocation, sink EventSink, installation state.Installation) (Result, error) {
	source := strings.ToLower(strings.TrimSpace(optionString(in, "source")))
	if source == "" {
		source = "all"
	}
	if !slices.Contains([]string{"pmux", "proxy", "service", "request-error", "all"}, source) {
		return Result{}, typedUsage("--source must be pmux, proxy, service, request-error, or all.")
	}
	level := strings.ToLower(strings.TrimSpace(optionString(in, "level")))
	if level != "" && !slices.Contains([]string{"debug", "info", "warn", "warning", "error", "critical"}, level) {
		return Result{}, typedUsage("--level must be debug, info, warn, warning, error, or critical.")
	}
	if level == "warning" {
		level = "warn"
	}
	lines := optionInt(in, "lines", 100)
	if lines < 0 {
		return Result{}, typedUsage("--lines must not be negative.")
	}
	if lines > 10000 {
		return Result{}, typedUsage("--lines must not exceed 10000.")
	}
	since, err := parseLogSince(optionString(in, "since"), r.deps.Now())
	if err != nil {
		return Result{}, err
	}
	clearSource := strings.ToLower(strings.TrimSpace(optionString(in, "clear")))
	if clearSource != "" {
		if source != "all" || level != "" || lines != 100 || !since.IsZero() || optionBool(in, "follow") || strings.TrimSpace(optionString(in, "output")) != "" {
			return Result{}, typedUsage("--clear cannot be combined with log read, filter, follow, or output flags.")
		}
		return r.clearServiceLogs(ctx, in, sink, installation, clearSource)
	}
	follow := optionBool(in, "follow")
	output := strings.TrimSpace(optionString(in, "output"))
	knownSecrets := make([]string, 0, 2)
	if r.deps.KnownSecrets != nil {
		loaded, loadErr := r.deps.KnownSecrets(ctx, installation)
		if loadErr != nil {
			return Result{}, ensureTyped(loadErr, "PMux could not load the local redaction set; no logs were emitted.")
		}
		for _, secret := range loaded {
			if len(secret) != 0 {
				knownSecrets = append(knownSecrets, string(secret))
			}
			clear(secret)
		}
	} else if r.deps.Secrets != nil {
		secret, loadErr := r.deps.Secrets(ctx, installation)
		if loadErr != nil {
			return Result{}, ensureTyped(loadErr, "PMux could not load the local redaction set; no logs were emitted.")
		}
		if len(secret) != 0 {
			knownSecrets = append(knownSecrets, string(secret))
		}
		clear(secret)
	}
	defer func() {
		for index := range knownSecrets {
			knownSecrets[index] = ""
		}
	}()
	var outputFile *os.File
	if output != "" {
		outputFile, err = createPrivateLogOutput(output)
		if err != nil {
			return Result{}, err
		}
		defer outputFile.Close()
	}
	acceptedCount := 0
	emittedCount := 0
	retained := make(map[string][]serviceLogEntry)
	appendEntry := func(entry serviceLogEntry) error {
		entry.Message = redactLogMessage(entry.Message, knownSecrets)
		entry.Level = normalizeLogLevel(entry.Level)
		if level != "" && entry.Level != level {
			return nil
		}
		if !since.IsZero() && (entry.Timestamp.IsZero() || entry.Timestamp.Before(since)) {
			return nil
		}
		if entry.Timestamp.IsZero() {
			entry.Timestamp = r.deps.Now().UTC()
		}
		retainLimit := lines
		if follow && entry.Source == "service" && retainLimit == 0 {
			retainLimit = 1
		}
		if retainLimit == 0 {
			return nil
		}
		bucket := append(retained[entry.Source], entry)
		if len(bucket) > retainLimit {
			bucket = bucket[len(bucket)-retainLimit:]
		}
		retained[entry.Source] = bucket
		acceptedCount++
		return nil
	}

	if source == "pmux" || source == "all" {
		path := filepath.Join(r.deps.Roots.State, "logs", "pmux.log")
		if err := readTextLog(path, "pmux", appendEntry); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, ensureTyped(err, "PMux could not read its application log.")
		}
	}
	if source == "proxy" || source == "request-error" || source == "all" {
		client, clientErr := r.management(ctx, installation)
		if clientErr != nil {
			return Result{}, clientErr
		}
		if source == "proxy" || source == "all" {
			page, queryErr := client.Logs(ctx, management.LogQuery{Level: level, Since: since, Tail: lines})
			if queryErr != nil {
				return Result{}, ensureTyped(queryErr, "PMux could not read CLIProxyAPI logs.")
			}
			for _, record := range page.Records {
				if err := appendEntry(serviceLogEntry{Source: "proxy", Level: record.Level, Timestamp: record.Timestamp, Message: record.Message}); err != nil {
					return Result{}, err
				}
			}
		}
		if source == "request-error" || source == "all" {
			records, queryErr := client.RequestErrorLogs(ctx)
			if queryErr != nil {
				return Result{}, ensureTyped(queryErr, "PMux could not read CLIProxyAPI request-error logs.")
			}
			for _, record := range records {
				if err := appendEntry(serviceLogEntry{Source: "request-error", Level: "error", Message: strings.TrimSpace(record.Name + " " + record.Message)}); err != nil {
					return Result{}, err
				}
			}
		}
	}
	if source == "service" || source == "all" {
		manager, managerErr := r.service(ctx, installation, false)
		if managerErr != nil {
			return Result{}, managerErr
		}
		reader, logErr := manager.Logs(ctx, lines, follow)
		if logErr != nil {
			return Result{}, ensureTyped(logErr, "PMux could not read service-manager logs.")
		}
		defer reader.Close()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			timestamp, parsedLevel, message := parseTextLogLine(scanner.Text())
			before := acceptedCount
			if err := appendEntry(serviceLogEntry{Source: "service", Level: parsedLevel, Timestamp: timestamp, Message: message}); err != nil {
				return Result{}, err
			}
			if follow && acceptedCount > before {
				bucket := retained["service"]
				entry := bucket[len(bucket)-1]
				if err := r.emitLogEntry(sink, outputFile, installation.ID, entry); err != nil {
					return Result{}, err
				}
				emittedCount++
				retained["service"] = bucket[:len(bucket)-1]
			}
		}
		if err := scanner.Err(); err != nil {
			return Result{}, normalize(err, pmuxerr.ServiceStartFailed, "Service log streaming failed.")
		}
	}
	entries := make([]serviceLogEntry, 0, len(retained)*lines)
	for _, bucket := range retained {
		entries = append(entries, bucket...)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Timestamp.Before(entries[j].Timestamp) })
	if len(entries) > lines {
		entries = entries[len(entries)-lines:]
	}
	for _, entry := range entries {
		if err := r.emitLogEntry(sink, outputFile, installation.ID, entry); err != nil {
			return Result{}, err
		}
		emittedCount++
	}
	if outputFile != nil {
		if err := outputFile.Sync(); err != nil {
			return Result{}, normalize(err, pmuxerr.ConfigMutationConflict, "PMux could not durably write the private log output.")
		}
	}
	terminal := Event{
		Type: "complete", InstanceID: installation.ID, Timestamp: r.deps.Now().UTC(),
		Data:  map[string]any{"count": emittedCount, "follow": follow, "output": output},
		Human: fmt.Sprintf("%d redacted log record(s).", emittedCount),
	}
	if sink != nil {
		if err := emit(sink, terminal); err != nil {
			return Result{}, sinkError(err)
		}
	}
	return Result{
		Data:  map[string]any{"logs": entries, "count": emittedCount, "follow": follow, "output": output},
		Human: []string{terminal.Human}, Streamed: sink != nil,
	}, nil
}

func (r *Router) clearServiceLogs(ctx context.Context, in Invocation, sink EventSink, installation state.Installation, source string) (Result, error) {
	if source != "proxy" {
		return Result{}, typedUsage("--clear supports only the single upstream source `proxy`; PMux audit, auth, service, and request-error data cannot be cleared.")
	}
	if err := r.emitPreview(sink, installation.ID, "Preview: clear CLIProxyAPI proxy logs; audit, auth, service, and request-error data remain untouched.", map[string]any{"clear": "proxy"}); err != nil {
		return Result{}, err
	}
	if !in.Yes {
		if !in.Interactive {
			return Result{}, typedUsage("Noninteractive log clearing requires `--yes`; no logs were cleared.")
		}
		ok, err := r.confirmPhrase("clear-logs")
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, canceled(errors.New("log clearing was not confirmed"))
		}
	}
	client, err := r.management(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	if err := client.DeleteLogs(ctx); err != nil {
		return Result{}, ensureTyped(err, "CLIProxyAPI logs could not be cleared.")
	}
	return Result{Data: map[string]any{"cleared": "proxy"}, Human: []string{"CLIProxyAPI proxy logs were cleared; audit and auth data were untouched."}}, nil
}

func (r *Router) emitLogEntry(sink EventSink, output *os.File, instanceID string, entry serviceLogEntry) error {
	if output != nil {
		body, err := json.Marshal(entry)
		if err != nil {
			return normalize(err, pmuxerr.UnhandledInternal, "PMux could not encode a redacted log record.")
		}
		if _, err := output.Write(append(body, '\n')); err != nil {
			return normalize(err, pmuxerr.ConfigMutationConflict, "PMux could not write the private log output.")
		}
	}
	if sink == nil {
		return nil
	}
	return emit(sink, Event{Type: "log", InstanceID: instanceID, Timestamp: entry.Timestamp, Data: entry, Human: entry.Message})
}

func (r *Router) emitPreview(sink EventSink, instanceID, human string, data map[string]any) error {
	if sink == nil {
		return nil
	}
	return emit(sink, Event{Type: "preview", InstanceID: instanceID, Timestamp: r.deps.Now().UTC(), Data: data, Human: human})
}

func readTextLog(path, source string, appendEntry func(serviceLogEntry) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		timestamp, level, message := parseTextLogLine(scanner.Text())
		if err := appendEntry(serviceLogEntry{Source: source, Timestamp: timestamp, Level: level, Message: message}); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func parseTextLogLine(line string) (time.Time, string, string) {
	line = strings.TrimSpace(line)
	var timestamp time.Time
	fields := strings.Fields(line)
	if len(fields) != 0 {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.Trim(fields[0], "[]")); err == nil {
			timestamp = parsed
			line = strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		}
	}
	level := ""
	if match := logLevelPattern.FindStringSubmatch(line); len(match) == 2 {
		level = normalizeLogLevel(match[1])
	}
	return timestamp, level, line
}

func normalizeLogLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "warning" {
		return "warn"
	}
	return level
}

func parseLogSince(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if value, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return value, nil
	}
	if duration, err := time.ParseDuration(raw); err == nil && duration >= 0 {
		return now.Add(-duration), nil
	}
	return time.Time{}, typedUsage("--since must be an RFC3339 timestamp or nonnegative duration.")
}

func (r *Router) confirmPhrase(phrase string) (bool, error) {
	prompt := map[string]string{
		"stop":       "Stop CLIProxyAPI? Active client requests may fail. Type stop to confirm: ",
		"restart":    "Restart CLIProxyAPI? Active client requests may be interrupted. Type restart to confirm: ",
		"uninstall":  "Uninstall the PMux service definition? The binary, config, and credentials will be kept. Type uninstall to confirm: ",
		"clear-logs": "Clear proxy logs? This cannot be undone. Type clear-logs to confirm: ",
	}[phrase]
	if prompt != "" {
		if _, err := io.WriteString(r.deps.Output, prompt); err != nil {
			return false, normalize(err, pmuxerr.CodeCanceled, "PMux could not display the confirmation prompt.")
		}
	}
	reader := bufio.NewReader(r.deps.Input)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, normalize(err, pmuxerr.CodeCanceled, "PMux could not read confirmation input.")
	}
	return strings.TrimSuffix(value, "\n") == phrase, nil
}

func redactLogMessage(message string, knownSecrets []string) string {
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, message)
	message = redact.Known(message, knownSecrets...)
	message = logBearerPattern.ReplaceAllStringFunc(message, func(value string) string {
		match := logBearerPattern.FindStringSubmatch(value)
		return match[1] + redact.Mask(match[2])
	})
	message = logSecretField.ReplaceAllStringFunc(message, func(value string) string {
		match := logSecretField.FindStringSubmatch(value)
		return match[1] + redact.Mask(match[2])
	})
	message = logPrivateKeyMarker.ReplaceAllString(message, "<redacted-private-key>")
	message = logPrivateKeyBody.ReplaceAllString(message, "<redacted-private-key-material>")
	return logCredentialPattern.ReplaceAllStringFunc(message, redact.Mask)
}

func createPrivateLogOutput(path string) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, normalize(err, pmuxerr.ConfigPathMismatch, "PMux could not resolve the log output path.")
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, normalize(err, pmuxerr.ConfigUnreadable, "PMux could not create the log output directory.")
	}
	file, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, normalize(err, pmuxerr.ConfigMutationConflict, "PMux will not overwrite an existing log output path.")
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		_ = os.Remove(absolute)
		return nil, normalize(err, pmuxerr.ConfigInsecurePermissions, "PMux could not establish private log output permissions.")
	}
	return file, nil
}

func (r *Router) configShow(ctx context.Context, in Invocation, pmuxConfig state.Config, installation state.Installation, configured bool) (Result, error) {
	effective := optionBool(in, "effective")
	if optionString(in, "scope") == "pmux" {
		data := map[string]any{"scope": "pmux", "values": pmuxConfig}
		if effective {
			data["effective"] = map[string]any{
				"source":           filepath.Join(r.deps.Roots.Config, "config.json"),
				"activation":       "immediate",
				"restart_required": false,
			}
		}
		return Result{Data: data, Human: []string{"PMux application settings."}}, nil
	}
	if !configured {
		return Result{Data: map[string]any{"configured": false, "path": "", "values": map[string]any{}}, Human: []string{"No CLIProxyAPI configuration is recorded. Run `pmux setup --mode managed`."}}, nil
	}
	adapter, err := r.config(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	snapshot, err := adapter.Read(ctx, installation.ConfigPath)
	if err != nil {
		return Result{}, ensureTyped(err, "PMux could not read CLIProxyAPI configuration.")
	}
	data := safeConfig(snapshot, optionBool(in, "reveal_paths"))
	if effective {
		activation := map[string]string{
			"host": "restart_required", "port": "restart_required",
			"auth-dir": "hot_reload", "remote-management.allow-remote": "hot_reload",
		}
		for name := range managementSettingKinds {
			activation[name] = "hot_reload"
		}
		data["effective"] = map[string]any{
			"source":        filepath.Clean(installation.ConfigPath),
			"activation":    activation,
			"runtime_state": "recorded",
		}
	}
	return Result{Data: data, Human: []string{fmt.Sprintf("CLIProxyAPI configuration: %s", installation.ConfigPath)}}, nil
}

func (r *Router) configGet(ctx context.Context, in Invocation, pmuxConfig state.Config, installation state.Installation, configured bool) (Result, error) {
	if len(in.Arguments) != 1 {
		return Result{}, typedUsage("Config get requires one key.")
	}
	key := in.Arguments[0]
	if optionString(in, "scope") == "pmux" {
		value, ok := pmuxSetting(pmuxConfig, key)
		if !ok {
			return Result{}, typedConfig(fmt.Sprintf("Unknown PMux setting %q.", key), "Run `pmux config --scope pmux show` to list settings.")
		}
		return Result{Data: map[string]any{"key": key, "value": value}, Human: []string{fmt.Sprintf("%s=%v", key, value)}}, nil
	}
	if !configured {
		return Result{}, notConfigured("read proxy configuration", "Run `pmux setup --mode managed` first.")
	}
	if _, ok := managementSettingKindFor(key); ok {
		client, err := r.management(ctx, installation)
		if err != nil {
			return Result{}, err
		}
		raw, err := client.GetSetting(ctx, management.SettingName(key))
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not read the CLIProxyAPI management setting.")
		}
		value, err := managementSettingValue(raw)
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"key": key, "value": value}, Human: []string{fmt.Sprintf("%s=%v", key, value)}}, nil
	}
	adapter, err := r.config(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	snapshot, err := adapter.Read(ctx, installation.ConfigPath)
	if err != nil {
		return Result{}, ensureTyped(err, "PMux could not read CLIProxyAPI configuration.")
	}
	value, ok := proxySetting(snapshot.Config, key)
	if !ok {
		return Result{}, typedConfig(fmt.Sprintf("Unknown proxy setting %q.", key), "Run `pmux config show` to list supported settings.")
	}
	return Result{Data: map[string]any{"key": key, "value": value}, Human: []string{fmt.Sprintf("%s=%v", key, value)}}, nil
}

func (r *Router) configSet(ctx context.Context, in Invocation, sink EventSink, pmuxConfig state.Config, installation state.Installation, configured bool) (Result, error) {
	if len(in.Arguments) != 2 {
		return Result{}, typedUsage("Config set requires a key and value.")
	}
	if !in.Interactive && !in.Yes {
		return Result{}, typedUsage("Noninteractive config mutation requires `--yes`; no changes were made.")
	}
	key, raw := in.Arguments[0], in.Arguments[1]
	if optionString(in, "scope") == "pmux" {
		if slot, ok := persistentClaudeSlot(key); ok {
			return r.configSetPersistentSlot(ctx, in, installation, configured, slot, raw)
		}
		if err := setPMuxSetting(&pmuxConfig, key, raw); err != nil {
			return Result{}, err
		}
		if err := r.emitPreview(sink, installation.ID, fmt.Sprintf("Preview: set PMux setting %s to %s.", key, raw), map[string]any{"scope": "pmux", "key": key, "value": raw}); err != nil {
			return Result{}, err
		}
		if in.Interactive && !in.Yes {
			ok, err := r.confirmPhrase("write")
			if err != nil {
				return Result{}, err
			}
			if !ok {
				return Result{}, canceled(errors.New("PMux settings change was not confirmed"))
			}
		}
		if err := r.deps.Store.SaveConfig(pmuxConfig); err != nil {
			return Result{}, normalize(err, pmuxerr.ConfigMutationConflict, "PMux settings could not be committed.")
		}
		return Result{Data: map[string]any{"key": key, "value": raw, "restart_required": false, "restarted": false}, Human: []string{fmt.Sprintf("Updated PMux setting %s.", key)}}, nil
	}
	if !configured {
		return Result{}, notConfigured("change proxy configuration", "Run `pmux setup --mode managed` first.")
	}
	if strings.Contains(strings.ToLower(key), "key") || strings.Contains(strings.ToLower(key), "secret") {
		return Result{}, typedConfig("Secret fields cannot be supplied as positional config values.", "Use `pmux providers login` or the dedicated protected-key flow.")
	}
	if _, ok := managementSettingKindFor(key); ok {
		return r.configSetManagement(ctx, in, sink, key, raw, installation)
	}
	value, err := parseProxyValue(key, raw)
	if err != nil {
		return Result{}, err
	}
	adapter, err := r.config(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	snapshot, err := adapter.Read(ctx, installation.ConfigPath)
	if err != nil {
		return Result{}, ensureTyped(err, "PMux could not read CLIProxyAPI configuration.")
	}
	plan, err := adapter.Plan(ctx, snapshot, []domainconfig.PatchOp{{Path: key, Value: value}})
	if err != nil {
		return Result{}, ensureTyped(err, "CLIProxyAPI configuration change is invalid.")
	}
	if err := r.emitPreview(sink, installation.ID, fmt.Sprintf("Preview: set CLIProxyAPI setting %s to %v.", key, value), map[string]any{"scope": "proxy", "key": key, "value": value, "restart_required": plan.RestartRequired}); err != nil {
		return Result{}, err
	}
	if in.Interactive && !in.Yes {
		ok, err := r.confirmPhrase("write")
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, canceled(errors.New("proxy configuration change was not confirmed"))
		}
	}
	result, err := adapter.Apply(ctx, plan)
	if err != nil {
		return Result{}, ensureTyped(err, "CLIProxyAPI configuration change could not be committed.")
	}
	restarted, err := r.restartAfterConfig(ctx, installation, result.RestartRequired, optionBool(in, "restart"))
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{"change": result, "restart_required": result.RestartRequired, "restarted": restarted}
	human := "CLIProxyAPI configuration saved and verified."
	if result.RestartRequired && !restarted {
		human += " A service restart is required before the change is active."
	}
	return Result{Data: data, Human: []string{human}}, nil
}

func (r *Router) configSetPersistentSlot(ctx context.Context, in Invocation, installation state.Installation, configured bool, slot, raw string) (Result, error) {
	if !configured {
		return Result{}, notConfigured("configure persistent Claude model slots", "Run `pmux setup --mode managed` first.")
	}
	if r.deps.Launcher == nil || r.deps.Secrets == nil {
		return Result{}, dependencyError("Persistent Claude integration is unavailable.", "Run a complete PMux build and retry.")
	}
	launcher, err := r.deps.Launcher(ctx, installation, in)
	if err != nil {
		return Result{}, ensureTyped(err, "Claude integration could not be prepared.")
	}
	token, err := r.deps.Secrets(ctx, installation)
	if err != nil {
		return Result{}, ensureTyped(err, "The proxy key could not be loaded for persistent Claude integration.")
	}
	defer clear(token)
	update := domainclient.SlotUpdate{Action: domainclient.SlotSet, Model: raw}
	if raw == "unmanaged" {
		update = domainclient.SlotUpdate{Action: domainclient.SlotUnmanaged}
	} else if strings.TrimSpace(raw) == "" {
		return Result{}, typedUsage("Persistent Claude model slots require an exact model ID or `unmanaged`.")
	}
	if raw != "unmanaged" {
		if r.deps.Models == nil {
			return Result{}, dependencyError("Live model discovery is unavailable.", "Run `pmux models list --refresh` and retry.")
		}
		catalog, err := r.deps.Models(ctx, installation)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not prepare live model discovery.")
		}
		entries, err := catalog.Refresh(ctx)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not verify the persistent Claude model against the live catalog.")
		}
		found := false
		for _, entry := range entries {
			if entry.ID == raw && entry.Available {
				found = true
				break
			}
		}
		if !found {
			return Result{}, typedUsage(fmt.Sprintf("Model %q is not in the current live catalog; run `pmux models list --refresh`.", raw))
		}
	}
	spec := domainclient.PersistSpec{BaseURL: baseURL(installation), Token: string(token)}
	switch slot {
	case "opus":
		spec.Slots.Opus = update
	case "sonnet":
		spec.Slots.Sonnet = update
	case "haiku":
		spec.Slots.Haiku = update
	default:
		return Result{}, typedUsage("Persistent Claude slot must be opus, sonnet, or haiku.")
	}
	plan, err := launcher.PlanPersist(ctx, spec)
	if err != nil {
		return Result{}, ensureTyped(err, "Persistent Claude settings change is invalid.")
	}
	if !in.Yes && in.Interactive {
		ok, confirmErr := r.confirmPhrase("persist")
		if confirmErr != nil {
			return Result{}, confirmErr
		}
		if !ok {
			return Result{}, canceled(errors.New("persistent Claude settings were not confirmed"))
		}
	}
	if err := launcher.Upsert(ctx, plan); err != nil {
		return Result{}, ensureTyped(err, "Persistent Claude settings could not be committed.")
	}
	rollback := func(commitErr error, message string) (Result, error) {
		reverse := plan
		reverse.Before, reverse.After = plan.After, plan.Before
		reverse.Diff = ""
		if rollbackErr := launcher.Upsert(ctx, reverse); rollbackErr != nil {
			return Result{}, normalize(
				fmt.Errorf("%w; Claude settings rollback also failed: %v", commitErr, rollbackErr),
				pmuxerr.ConfigMutationConflict,
				message+" Claude settings may be partially changed.",
			)
		}
		return Result{}, normalize(commitErr, pmuxerr.ConfigMutationConflict, message+" Claude settings were restored.")
	}
	cfg, err := r.deps.Store.LoadConfig()
	if err != nil {
		return rollback(err, "PMux could not load its persistent Claude integration record.")
	}
	if cfg.PersistentClaudeModels == nil {
		cfg.PersistentClaudeModels = make(map[string]string)
	}
	if raw == "unmanaged" {
		delete(cfg.PersistentClaudeModels, slot)
	} else {
		cfg.PersistentClaudeModels[slot] = raw
	}
	if err := r.deps.Store.SaveConfig(cfg); err != nil {
		return rollback(err, "PMux could not update its persistent Claude integration record.")
	}
	value := raw
	return Result{Data: map[string]any{"slot": slot, "value": value, "path": plan.Path, "diff": plan.Diff}, Human: []string{fmt.Sprintf("Persistent Claude %s slot is now %s.", slot, value)}}, nil
}

func persistentClaudeSlot(key string) (string, bool) {
	const prefix = "integrations.claude.persistent-models."
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	slot := strings.TrimPrefix(key, prefix)
	return slot, slices.Contains([]string{"opus", "sonnet", "haiku"}, slot)
}

func (r *Router) configEdit(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	if !in.Interactive {
		return Result{}, typedUsage("This operation requires an interactive terminal; use `pmux config set` instead.")
	}
	if r.deps.ConfigFiles == nil {
		return Result{}, dependencyError("Configuration editor support is unavailable.", "Run a complete PMux build and retry.")
	}
	scope := optionString(in, "scope")
	if scope == "" {
		scope = "proxy"
	}
	target := filepath.Join(r.deps.Roots.Config, "config.json")
	if scope == "proxy" {
		if !configured {
			return Result{}, notConfigured("edit proxy configuration", "Run `pmux setup --mode managed` first.")
		}
		target = installation.ConfigPath
	} else if scope != "pmux" {
		return Result{}, typedUsage("Config edit scope must be proxy or pmux.")
	}
	result, err := r.deps.ConfigFiles.Edit(ctx, ConfigEditRequest{
		Scope: scope, Target: target, Editor: optionString(in, "editor"),
		Confirm: func(string) (bool, error) {
			if in.Yes {
				return true, nil
			}
			return r.confirmPhrase("write")
		},
	})
	if err != nil {
		return Result{}, ensureTyped(err, "Configuration edit did not commit.")
	}
	restarted := false
	if scope == "proxy" {
		restarted, err = r.restartAfterConfig(ctx, installation, result.RestartRequired, optionBool(in, "restart"))
		if err != nil {
			return Result{}, err
		}
	}
	data := map[string]any{"edit": result, "restart_required": result.RestartRequired, "restarted": restarted}
	return Result{Data: data, Human: []string{"Configuration edit was validated and committed atomically."}}, nil
}

func (r *Router) restartAfterConfig(ctx context.Context, installation state.Installation, required, requested bool) (bool, error) {
	if !required || !requested {
		return false, nil
	}
	manager, err := r.service(ctx, installation, false)
	if err != nil {
		return false, err
	}
	if _, err := manager.Restart(ctx); err != nil {
		return false, ensureTyped(err, "Configuration was saved, but the required service restart failed verification.")
	}
	return true, nil
}

func (r *Router) configMaintenance(ctx context.Context, in Invocation, sink EventSink, installation state.Installation, configured bool) (Result, error) {
	scope := optionString(in, "scope")
	if scope == "pmux" {
		if r.deps.PMuxConfig == nil {
			return Result{}, dependencyError("PMux settings backup support is unavailable.", "Run a complete PMux build and retry.")
		}
		if in.Operation == OpConfigBackup {
			id, err := r.deps.PMuxConfig.BackupPMux(ctx)
			if err != nil {
				return Result{}, ensureTyped(err, "PMux settings backup failed.")
			}
			return Result{Data: map[string]any{"scope": "pmux", "backup": id}, Human: []string{"PMux settings backup created: " + id}}, nil
		}
		if len(in.Arguments) != 1 {
			return Result{}, typedUsage("Config restore requires one backup ID.")
		}
		if !in.Interactive && !in.Yes {
			return Result{}, typedUsage("Noninteractive restore requires `--yes`; no changes were made.")
		}
		current, err := r.deps.Store.LoadConfig()
		if err != nil {
			return Result{}, ensureTyped(err, "PMux could not read current settings before restore.")
		}
		plan, err := r.deps.PMuxConfig.PlanRestorePMux(ctx, in.Arguments[0], current)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux settings backup could not be validated.")
		}
		if err := r.emitPreview(sink, installation.ID, "Preview: restore PMux settings backup "+plan.ID+".", map[string]any{"scope": "pmux", "backup": plan.ID, "diff": plan.Diff}); err != nil {
			return Result{}, err
		}
		if in.Interactive && !in.Yes {
			ok, err := r.confirmPhrase("restore")
			if err != nil {
				return Result{}, err
			}
			if !ok {
				return Result{}, canceled(errors.New("PMux settings restore was not confirmed"))
			}
		}
		if err := r.deps.PMuxConfig.RestorePMux(ctx, plan); err != nil {
			return Result{}, ensureTyped(err, "PMux settings restore failed.")
		}
		return Result{
			Data:  map[string]any{"scope": "pmux", "backup": plan.ID, "diff": plan.Diff},
			Human: []string{"PMux settings backup restored and validated."},
		}, nil
	}
	if !configured {
		return Result{}, notConfigured("manage proxy backups", "Run `pmux setup --mode managed` first.")
	}
	if r.deps.ConfigFiles == nil {
		return Result{}, dependencyError("Configuration backup support is unavailable.", "Run a complete PMux build and retry.")
	}
	if in.Operation == OpConfigBackup {
		path, err := r.deps.ConfigFiles.Backup(ctx, installation.ConfigPath)
		if err != nil {
			return Result{}, ensureTyped(err, "Configuration backup failed.")
		}
		return Result{Data: map[string]any{"backup": path}, Human: []string{"Configuration backup created: " + path}}, nil
	}
	if !in.Interactive && !in.Yes {
		return Result{}, typedUsage("Noninteractive restore requires `--yes`; no changes were made.")
	}
	if len(in.Arguments) != 1 {
		return Result{}, typedUsage("Config restore requires one backup ID.")
	}
	adapter, err := r.config(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	before, err := adapter.Read(ctx, installation.ConfigPath)
	if err != nil {
		return Result{}, ensureTyped(err, "Configuration restore could not read the current configuration.")
	}
	if err := r.emitPreview(sink, installation.ID, "Preview: restore CLIProxyAPI configuration backup "+in.Arguments[0]+".", map[string]any{"scope": "proxy", "backup": in.Arguments[0], "target": installation.ConfigPath}); err != nil {
		return Result{}, err
	}
	if in.Interactive && !in.Yes {
		ok, err := r.confirmPhrase("restore")
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, canceled(errors.New("proxy configuration restore was not confirmed"))
		}
	}
	value, err := r.deps.ConfigFiles.Restore(ctx, installation.ConfigPath, in.Arguments[0])
	if err != nil {
		return Result{}, ensureTyped(err, "Configuration restore failed.")
	}
	after, err := adapter.Read(ctx, installation.ConfigPath)
	if err != nil {
		return Result{}, ensureTyped(err, "Configuration was restored, but PMux could not classify its activation.")
	}
	required := configRestartRequired(before.Config, after.Config)
	restarted, err := r.restartAfterConfig(ctx, installation, required, optionBool(in, "restart"))
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{"restore": value, "restart_required": required, "restarted": restarted}
	human := "Configuration backup restored and verified."
	if required && !restarted {
		human += " A service restart is required before the restored listener settings are active."
	}
	return Result{Data: data, Human: []string{human}}, nil
}

func configRestartRequired(before, after domainconfig.Config) bool {
	return before.Host != after.Host || before.Port != after.Port
}

type launchReadiness struct {
	launcher domainclient.ClientLauncher
	install  domainclient.ClientInstall
	model    string
	cwd      string
}

func (r *Router) launchPreflight(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	ready, err := r.prepareLaunch(ctx, in, installation, configured)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Data: map[string]any{
			"ready":       true,
			"client":      ready.install,
			"model":       ready.model,
			"working_dir": ready.cwd,
			"base_url":    baseURL(installation),
		},
		Human: []string{fmt.Sprintf("Claude Code %s is ready with exact model %s.", ready.install.Version, ready.model)},
	}, nil
}

func (r *Router) launch(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	ready, err := r.prepareLaunch(ctx, in, installation, configured)
	if err != nil {
		return Result{}, err
	}
	if r.deps.Secrets == nil {
		return Result{}, dependencyError("Claude launch infrastructure is unavailable.", "Run `pmux doctor` to inspect the proxy-key dependency.")
	}
	token, err := r.deps.Secrets(ctx, installation)
	if err != nil {
		return Result{}, ensureTyped(err, "The private proxy key source could not be loaded.")
	}
	defer clearBytes(token)
	spec := domainclient.LaunchSpec{
		Client: domainclient.Claude, Model: ready.model, BaseURL: baseURL(installation),
		Token: string(token), Args: append([]string(nil), in.Arguments...), WorkingDir: ready.cwd,
	}
	launchResult, err := ready.launcher.Launch(ctx, spec)
	if err != nil {
		return Result{}, ensureTyped(err, "Claude Code could not be launched.")
	}
	data := map[string]any{"client": "claude", "model": ready.model, "origin": "client", "exit_code": launchResult.ExitCode}
	if launchResult.Signal != "" {
		data["signal"] = launchResult.Signal
	}
	human := []string{"Claude Code exited."}
	if launchResult.ExitCode != 0 {
		human = []string{fmt.Sprintf("Claude Code exited with status %d.", launchResult.ExitCode)}
	}
	return Result{Data: data, Human: human, ExitCode: launchResult.ExitCode}, nil
}

func (r *Router) prepareLaunch(ctx context.Context, in Invocation, installation state.Installation, configured bool) (launchReadiness, error) {
	if !configured {
		return launchReadiness{}, notConfigured("launch Claude Code", "Run `pmux setup --mode managed`, authenticate a provider, and select a live model.")
	}
	clientID := optionString(in, "client")
	if clientID == "" {
		clientID = "claude"
	}
	if clientID != "claude" {
		return launchReadiness{}, typedUsage("Claude Code is the only supported client in PMux v1.")
	}
	modelID := optionString(in, "model")
	if modelID == "" {
		return launchReadiness{}, typedUsage("Launch requires an exact dynamic model ID.")
	}
	catalog, err := r.models(ctx, installation)
	if err != nil {
		return launchReadiness{}, err
	}
	entries, err := catalog.Refresh(ctx)
	if err != nil {
		return launchReadiness{}, ensureTyped(err, "Launch requires a live model refresh.")
	}
	found := false
	for _, entry := range entries {
		if entry.ID != modelID {
			continue
		}
		if entry.Stale {
			staleErr := pmuxerr.New(pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Launch requires live model availability, but the matching catalog entry is stale.")
			staleErr.Repair = []string{"Restore CLIProxyAPI connectivity, then run `pmux models list --refresh` before launching."}
			return launchReadiness{}, staleErr
		}
		if entry.Available {
			found = true
		}
		break
	}
	if !found {
		return launchReadiness{}, unhealthy(fmt.Sprintf("Model %q is not currently served.", modelID), "Run `pmux models list --refresh` and choose an exact returned ID.")
	}
	if r.deps.Launcher == nil {
		return launchReadiness{}, dependencyError("Claude launch infrastructure is unavailable.", "Run `pmux doctor` to inspect the client dependency.")
	}
	launcher, err := r.deps.Launcher(ctx, installation, in)
	if err != nil {
		return launchReadiness{}, ensureTyped(err, "Claude launch could not be prepared.")
	}
	install, err := launcher.Detect(ctx)
	if err != nil {
		return launchReadiness{}, ensureTyped(err, "Claude Code v2.0.0 or newer was not found.")
	}
	if !install.Supported {
		return launchReadiness{}, dependencyError(
			fmt.Sprintf("Claude Code %s is unsupported; PMux requires Claude Code v2.0.0 or newer.", valueOr(install.Version, "version unknown")),
			"Install or upgrade Claude Code, then run `pmux doctor`.",
		)
	}
	cwd, err := r.deps.WorkingDir()
	if err != nil {
		return launchReadiness{}, normalize(err, pmuxerr.ConfigPathMismatch, "The launch working directory is unavailable.")
	}
	return launchReadiness{launcher: launcher, install: install, model: modelID, cwd: cwd}, nil
}

func (r *Router) doctor(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	checks := optionStrings(in, "checks")
	fixes := optionStrings(in, "fixes")
	fixing := optionBool(in, "fix")
	if fixing && !in.Interactive && !in.Yes {
		return Result{}, typedUsage("Noninteractive doctor fixes require `--yes`; no changes were made.")
	}
	if !configured {
		report := map[string]any{
			"checks": []any{map[string]any{
				"id":       "PMUX-STATE",
				"status":   "fail",
				"severity": "critical",
				"summary":  "PMux has no configured CLIProxyAPI installation",
				"evidence": []string{"state root: " + r.deps.Roots.State},
				"repair": map[string]any{
					"available":             false,
					"description":           "Run pmux setup --mode managed or adopt an installation",
					"destructive":           false,
					"confirmation_required": false,
					"verification":          "a versioned installation record exists",
				},
			}},
			"summary": map[string]int{"passed": 0, "warnings": 0, "failed": 1, "critical": 1, "exit_code": 7},
		}
		if bundle := optionString(in, "bundle"); bundle != "" {
			if r.deps.Bundle == nil {
				return Result{}, dependencyError("Diagnostic bundle support is unavailable.", "Install a complete PMux build and retry.")
			}
			value, err := r.deps.Bundle.Bundle(ctx, installation, bundle)
			if err != nil {
				return Result{}, ensureTyped(err, "Diagnostic bundle creation failed.")
			}
			report["bundle"] = value
		}
		return Result{Data: report, Human: []string{"PMux is not configured. Run `pmux setup --mode managed`."}, ExitCode: 7}, nil
	}
	if r.deps.Doctor == nil {
		return Result{}, dependencyError("Doctor infrastructure is unavailable.", "Install a complete PMux build and retry `pmux doctor`.")
	}
	report, unresolved, err := r.deps.Doctor.Run(ctx, installation, checks, fixes, fixing, in.Yes, optionBool(in, "online"))
	if err != nil {
		return Result{}, ensureTyped(err, "Doctor could not complete its requested checks.")
	}
	data := report
	if bundle := optionString(in, "bundle"); bundle != "" {
		if r.deps.Bundle == nil {
			return Result{}, dependencyError("Diagnostic bundle support is unavailable.", "Install a complete PMux build and retry.")
		}
		value, err := r.deps.Bundle.Bundle(ctx, installation, bundle)
		if err != nil {
			return Result{}, ensureTyped(err, "Diagnostic bundle creation failed.")
		}
		data, err = attachBundle(report, value)
		if err != nil {
			return Result{}, err
		}
	}
	exitCode := 0
	human := []string{"pmux doctor: all checks passed"}
	if unresolved {
		exitCode = 7
		human = []string{"pmux doctor: unresolved failures remain"}
	}
	return Result{Data: data, Human: human, ExitCode: exitCode}, nil
}

func attachBundle(report, bundle any) (any, error) {
	body, err := json.Marshal(report)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "Doctor report could not be projected for bundle output.")
	}
	projected := make(map[string]any)
	if err := json.Unmarshal(body, &projected); err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "Doctor report could not be projected for bundle output.")
	}
	projected["bundle"] = bundle
	return projected, nil
}

func (r *Router) update(ctx context.Context, in Invocation, installation state.Installation, configured bool) (Result, error) {
	if r.deps.Updates == nil {
		return Result{}, dependencyError("Update infrastructure is unavailable.", "Install a complete PMux build and retry the explicit update command.")
	}
	version := optionString(in, "version")
	switch in.Operation {
	case OpUpdateCheck:
		value, err := r.deps.Updates.Check(ctx, optionString(in, "component"), installation)
		if err != nil {
			return Result{}, ensureTyped(err, "Explicit update check failed.")
		}
		return Result{Data: value, Human: []string{"Update check complete; no files were changed."}}, nil
	case OpUpdateSelf:
		value, err := r.deps.Updates.Self(ctx, version)
		if err != nil {
			return Result{}, ensureTyped(err, "PMux self-update failed.")
		}
		return Result{Data: value, Human: []string{"PMux self-update complete."}}, nil
	case OpUpdateProxy:
		if !configured {
			return Result{}, notConfigured("update CLIProxyAPI", "Run `pmux setup --mode managed` first.")
		}
		if installation.Kind != "managed" {
			return Result{}, ownershipError("CLIProxyAPI is adopted, not PMux-managed.", "Update it with its owning installation method.")
		}
		value, err := r.deps.Updates.Proxy(ctx, installation, version)
		if err != nil {
			return Result{}, ensureTyped(err, "CLIProxyAPI update failed.")
		}
		return Result{Data: value, Human: []string{"CLIProxyAPI update complete."}}, nil
	}
	panic("unreachable")
}

func (r *Router) management(ctx context.Context, installation state.Installation) (management.ManagementClient, error) {
	if r.deps.Management == nil {
		return nil, dependencyError("The Management API adapter is unavailable.", "Run `pmux doctor` to inspect local management configuration.")
	}
	client, err := r.deps.Management(ctx, installation)
	if err != nil {
		return nil, ensureTyped(err, "The local Management API could not be configured.")
	}
	return client, nil
}
func (r *Router) models(ctx context.Context, installation state.Installation) (domainmodel.ModelCatalog, error) {
	if r.deps.Models == nil {
		return nil, dependencyError("The model catalog adapter is unavailable.", "Install a complete PMux build and retry.")
	}
	value, err := r.deps.Models(ctx, installation)
	if err != nil {
		return nil, ensureTyped(err, "The dynamic model catalog could not be configured.")
	}
	return value, nil
}
func (r *Router) service(ctx context.Context, installation state.Installation, foreground bool) (domainservice.ServiceManager, error) {
	if r.deps.Services == nil {
		return nil, dependencyError("The service adapter is unavailable.", "Run `pmux doctor` to inspect the configured service backend.")
	}
	value, err := r.deps.Services(ctx, installation, foreground)
	if err != nil {
		return nil, ensureTyped(err, "The recorded service backend could not be configured.")
	}
	return value, nil
}
func (r *Router) config(ctx context.Context, installation state.Installation) (domainconfig.ConfigFile, error) {
	if r.deps.Configs == nil {
		return nil, dependencyError("The configuration adapter is unavailable.", "Install a complete PMux build and retry.")
	}
	value, err := r.deps.Configs(ctx, installation)
	if err != nil {
		return nil, ensureTyped(err, "The configuration adapter could not be prepared.")
	}
	return value, nil
}
func (r *Router) readServiceStatus(ctx context.Context, installation state.Installation) (domainservice.ServiceStatus, error) {
	manager, err := r.service(ctx, installation, false)
	if err != nil {
		return domainservice.ServiceStatus{}, err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return domainservice.ServiceStatus{}, ensureTyped(err, "PMux could not read the recorded service status.")
	}
	return status, nil
}

func selectInstallation(st state.State, selected string) (state.Installation, bool) {
	if selected != "" {
		for _, installation := range st.Installations {
			if installation.ID == selected {
				return installation, true
			}
		}
	}
	if len(st.Installations) > 0 {
		return st.Installations[0], true
	}
	return state.Installation{}, false
}
func safeInstallation(value state.Installation) map[string]any {
	return map[string]any{"id": value.ID, "kind": value.Kind, "binary_path": value.BinaryPath, "config_path": value.ConfigPath, "auth_dir": value.AuthDir, "runtime_dir": value.RuntimeDir, "host": value.Host, "port": value.Port, "service_backend": value.ServiceBackend, "core_version": value.CoreVersionSeen, "proxy_key": value.ProxyKeyRef.Masked}
}
func safeConfig(snapshot domainconfig.ConfigSnapshot, revealPaths bool) map[string]any {
	values := map[string]any{"host": snapshot.Config.Host, "port": snapshot.Config.Port, "ws-auth": snapshot.Config.WSAuth, "management-local": snapshot.Config.ManagementLocal, "api-keys": fmt.Sprintf("%d configured (redacted)", len(snapshot.Config.APIKeys))}
	if revealPaths {
		values["auth-dir"] = snapshot.Config.AuthDir
	}
	return map[string]any{"configured": true, "path": snapshot.Path, "fingerprint": fmt.Sprintf("%x", snapshot.Fingerprint), "values": values}
}
func safeError(err error) string {
	var typed *pmuxerr.Error
	if ok := errorAs(err, &typed); ok {
		return typed.Message
	}
	return "operation failed; run `pmux doctor` for safe details"
}
func errorAs(err error, target **pmuxerr.Error) bool {
	for err != nil {
		if typed, ok := err.(*pmuxerr.Error); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = unwrapper.Unwrap()
	}
	return false
}
func usableStatus(value string) bool {
	switch strings.ToLower(value) {
	case "", "ok", "usable", "active", "authenticated":
		return true
	default:
		return false
	}
}
func selectFlow(flows []provider.AuthFlow, method string) provider.AuthFlow {
	preferred := []provider.AuthFlow{provider.FlowDeviceCode, provider.FlowBrowser}
	if method == "browser" {
		preferred = []provider.AuthFlow{provider.FlowBrowser}
	}
	if method == "device" {
		preferred = []provider.AuthFlow{provider.FlowDeviceCode}
	}
	for _, want := range preferred {
		if slices.Contains(flows, want) {
			return want
		}
	}
	return ""
}
func emit(sink EventSink, event Event) error {
	if sink == nil {
		return nil
	}
	return sink(event)
}
func filterModels(entries []domainmodel.CatalogEntry, providerID, search string, available, favorite bool) []domainmodel.CatalogEntry {
	out := make([]domainmodel.CatalogEntry, 0, len(entries))
	search = strings.ToLower(search)
	for _, entry := range entries {
		if available && !entry.Available || favorite && !entry.Favorite {
			continue
		}
		if providerID != "" && !slices.Contains(entry.Providers, management.ProviderID(providerID)) {
			continue
		}
		if search != "" {
			text := strings.ToLower(entry.ID + " " + entry.Owner)
			for _, p := range entry.Providers {
				text += " " + strings.ToLower(string(p))
			}
			if !strings.Contains(text, search) {
				continue
			}
		}
		out = append(out, entry)
	}
	return out
}
func serviceSpec(roots domainplatform.Roots, installation state.Installation, backend domainservice.ServiceBackend) domainservice.ServiceSpec {
	executable, _ := os.Executable()
	return domainservice.ServiceSpec{InstanceID: installation.ID, Identity: domainservice.Identity(backend, installation.ID), PMuxPath: executable, BinaryPath: installation.BinaryPath, ConfigPath: installation.ConfigPath, RuntimeDir: installation.RuntimeDir, LogDir: roots.State + "/logs", Environment: []string{}}
}
func baseURL(installation state.Installation) string {
	host := installation.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + strconv.Itoa(installation.Port)
}
func pmuxSetting(cfg state.Config, key string) (any, bool) {
	if slot, ok := persistentClaudeSlot(key); ok {
		value, managed := cfg.PersistentClaudeModels[slot]
		if !managed {
			return "unmanaged", true
		}
		return value, true
	}
	switch key {
	case "theme":
		return cfg.Theme, true
	case "update.check", "update-check":
		return cfg.UpdateCheck, true
	case "default-installation":
		return cfg.DefaultInstallation, true
	case "default-client":
		return cfg.DefaultClient, true
	case "default-model":
		return cfg.DefaultModel, true
	case "log-line-limit":
		return cfg.LogLineLimit, true
	}
	return nil, false
}
func setPMuxSetting(cfg *state.Config, key, raw string) error {
	switch key {
	case "theme":
		cfg.Theme = raw
	case "update.check", "update-check":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return typedUsage("update.check must be true or false.")
		}
		cfg.UpdateCheck = value
	case "default-installation":
		cfg.DefaultInstallation = raw
	case "default-client":
		if raw != "" && raw != "claude" {
			return typedUsage("default-client must be claude or empty.")
		}
		cfg.DefaultClient = raw
	case "default-model":
		cfg.DefaultModel = raw
	case "log-line-limit":
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return typedUsage("log-line-limit must be a nonnegative integer.")
		}
		cfg.LogLineLimit = value
	default:
		return typedConfig(fmt.Sprintf("Unknown PMux setting %q.", key), "Run `pmux config --scope pmux show` to list settings.")
	}
	return nil
}
func proxySetting(cfg domainconfig.Config, key string) (any, bool) {
	switch key {
	case "host":
		return cfg.Host, true
	case "port":
		return cfg.Port, true
	case "auth-dir":
		return cfg.AuthDir, true
	case "ws-auth":
		return cfg.WSAuth, true
	case "remote-management.allow-remote":
		return !cfg.ManagementLocal, true
	}
	return nil, false
}

type managementSettingKind string

const (
	managementBool    managementSettingKind = "bool"
	managementInteger managementSettingKind = "integer"
	managementRouting managementSettingKind = "routing"
	managementURL     managementSettingKind = "url"
)

var managementSettingKinds = map[string]managementSettingKind{
	"debug":                               managementBool,
	"logging-to-file":                     managementBool,
	"logs-max-total-size-mb":              managementInteger,
	"error-logs-max-files":                managementInteger,
	"usage-statistics-enabled":            managementBool,
	"request-log":                         managementBool,
	"ws-auth":                             managementBool,
	"request-retry":                       managementInteger,
	"max-retry-interval":                  managementInteger,
	"force-model-prefix":                  managementBool,
	"routing/strategy":                    managementRouting,
	"quota-exceeded/switch-project":       managementBool,
	"quota-exceeded/switch-preview-model": managementBool,
	"proxy-url":                           managementURL,
}

func managementSettingKindFor(key string) (managementSettingKind, bool) {
	kind, ok := managementSettingKinds[key]
	return kind, ok
}

func parseManagementSetting(key, raw string) (any, error) {
	kind, ok := managementSettingKindFor(key)
	if !ok {
		return nil, typedConfig(fmt.Sprintf("Unknown or unsupported proxy setting %q.", key), "Run `pmux config show` to list supported settings.")
	}
	switch kind {
	case managementBool:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, typedUsage(key + " must be true or false.")
		}
		return value, nil
	case managementInteger:
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return nil, typedUsage(key + " must be a nonnegative integer.")
		}
		return value, nil
	case managementRouting:
		if raw != "round-robin" && raw != "fill-first" {
			return nil, typedUsage("routing/strategy must be round-robin or fill-first.")
		}
		return raw, nil
	case managementURL:
		if raw == "" {
			return raw, nil
		}
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, typedUsage("proxy-url must be an absolute http or https URL, or empty.")
		}
		return raw, nil
	default:
		return nil, typedConfig(fmt.Sprintf("Unsupported proxy setting %q.", key), "")
	}
}

func managementSettingValue(raw management.SettingValue) (any, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, normalize(err, pmuxerr.UnhandledUpstreamShape, "CLIProxyAPI returned malformed setting JSON.")
	}
	if object, ok := decoded.(map[string]any); ok {
		if value, found := object["value"]; found {
			return value, nil
		}
	}
	return decoded, nil
}

func (r *Router) configSetManagement(ctx context.Context, in Invocation, sink EventSink, key, raw string, installation state.Installation) (Result, error) {
	value, err := parseManagementSetting(key, raw)
	if err != nil {
		return Result{}, err
	}
	client, err := r.management(ctx, installation)
	if err != nil {
		return Result{}, err
	}
	name := management.SettingName(key)
	before, err := client.GetSetting(ctx, name)
	if err != nil {
		return Result{}, ensureTyped(err, "PMux could not read the prior CLIProxyAPI setting.")
	}
	body, err := json.Marshal(map[string]any{"value": value})
	if err != nil {
		return Result{}, normalize(err, pmuxerr.UnhandledInternal, "PMux could not encode the validated CLIProxyAPI setting.")
	}
	if err := r.emitPreview(sink, installation.ID, fmt.Sprintf("Preview: set CLIProxyAPI setting %s to %v through the Management API.", key, value), map[string]any{"scope": "proxy", "key": key, "value": value, "transport": "management"}); err != nil {
		return Result{}, err
	}
	if in.Interactive && !in.Yes {
		ok, confirmErr := r.confirmPhrase("write")
		if confirmErr != nil {
			return Result{}, confirmErr
		}
		if !ok {
			return Result{}, canceled(errors.New("CLIProxyAPI setting change was not confirmed"))
		}
	}
	mutationErr := client.PutSetting(ctx, name, management.SettingValue(body))
	after, verifyErr := client.GetSetting(ctx, name)
	if mutationErr == nil && verifyErr == nil {
		got, valueErr := managementSettingValue(after)
		if valueErr == nil {
			want, _ := managementSettingValue(management.SettingValue(body))
			if reflect.DeepEqual(got, want) {
				return Result{
					Data:  map[string]any{"key": key, "value": value, "restart_required": false, "restarted": false, "transport": "management"},
					Human: []string{fmt.Sprintf("Updated CLIProxyAPI setting %s and verified it.", key)},
				}, nil
			}
			verifyErr = errors.New("setting read-back did not match requested value")
		} else {
			verifyErr = valueErr
		}
	}
	cause := mutationErr
	if cause == nil {
		cause = verifyErr
	}
	restoreErr := client.PutSetting(ctx, name, before)
	if restoreErr == nil {
		restored, readErr := client.GetSetting(ctx, name)
		if readErr != nil || !jsonEquivalent(restored, before) {
			if readErr != nil {
				restoreErr = readErr
			} else {
				restoreErr = errors.New("compensating setting restore did not verify")
			}
		}
	}
	if restoreErr != nil {
		return Result{}, normalize(fmt.Errorf("%w; compensating restore failed: %v", cause, restoreErr), pmuxerr.ConfigMutationConflict, "CLIProxyAPI setting verification failed and its prior value could not be restored.")
	}
	return Result{}, normalize(cause, pmuxerr.ConfigMutationConflict, "CLIProxyAPI setting verification failed; its prior value was restored.")
}

func jsonEquivalent(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}

func parseProxyValue(key, raw string) (any, error) {
	switch key {
	case "host", "auth-dir":
		return raw, nil
	case "port":
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 65535 {
			return nil, typedUsage("port must be an integer from 1 through 65535.")
		}
		return value, nil
	case "ws-auth", "remote-management.allow-remote":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, typedUsage(key + " must be true or false.")
		}
		return value, nil
	default:
		return nil, typedConfig(fmt.Sprintf("Unknown or unsupported proxy setting %q.", key), "Run `pmux config show` to list supported settings.")
	}
}

const maxProtectedInput = 64 * 1024

type readerProtectedInput struct{ reader io.Reader }

func (input *readerProtectedInput) ReadSecret(ctx context.Context) ([]byte, error) {
	return readProtectedBytes(ctx, input.reader)
}

type promptProtectedInput struct {
	read   func(context.Context, string) ([]byte, error)
	prompt string
}

func (input *promptProtectedInput) ReadSecret(ctx context.Context) ([]byte, error) {
	if input.read == nil {
		return nil, fmt.Errorf("protected terminal input is unavailable")
	}
	return input.read(ctx, input.prompt)
}

type fileProtectedInput struct {
	path   string
	verify func(string) error
}

func (input *fileProtectedInput) ReadSecret(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input.verify == nil {
		return nil, fmt.Errorf("protected input permission verification is unavailable")
	}
	info, err := os.Lstat(input.path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("protected input is not a regular file")
	}
	file, err := os.Open(input.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := input.verify(input.path); err != nil {
		return nil, err
	}
	current, err := os.Lstat(input.path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(opened, current) {
		return nil, fmt.Errorf("protected input changed during permission verification")
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("protected input changed while opening")
	}
	return readProtectedBytes(ctx, file)
}

func readProtectedBytes(ctx context.Context, reader io.Reader) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("protected input is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := io.ReadAll(io.LimitReader(reader, maxProtectedInput+1))
	if err != nil {
		clearBytes(value)
		return nil, err
	}
	if len(value) > maxProtectedInput {
		clearBytes(value)
		return nil, fmt.Errorf("protected input exceeds %d bytes", maxProtectedInput)
	}
	if err := ctx.Err(); err != nil {
		clearBytes(value)
		return nil, err
	}
	return value, nil
}

func safeChallenge(challenge management.OAuthChallenge) map[string]any {
	data := make(map[string]any)
	if challenge.URL != "" {
		data["url"] = challenge.URL
	}
	if challenge.VerificationURI != "" {
		data["verification_uri"] = challenge.VerificationURI
	}
	if challenge.UserCode != "" {
		data["user_code"] = challenge.UserCode
	}
	if !challenge.ExpiresAt.IsZero() {
		data["expires_at"] = challenge.ExpiresAt
	}
	if challenge.Interval > 0 {
		data["interval"] = challenge.Interval
	}
	return data
}

func optionString(in Invocation, key string) string {
	if value, ok := in.Options[key].(string); ok {
		return value
	}
	return ""
}
func providerKeyApplication(in Invocation, input provider.ProtectedInput) provider.APIKeyApplication {
	application := provider.APIKeyApplication{
		Input: input,
		ID:    optionString(in, "provider_entry_id"),
		Label: optionString(in, "provider_label"),
	}
	if supplied, ok := in.Options["provider_fields"].(map[string]string); ok {
		application.Fields = make(map[string]string, len(supplied))
		for key, value := range supplied {
			application.Fields[key] = value
		}
	}
	return application
}
func optionBool(in Invocation, key string) bool { value, _ := in.Options[key].(bool); return value }
func optionInt(in Invocation, key string, fallback int) int {
	value, ok := in.Options[key].(int)
	if !ok {
		return fallback
	}
	return value
}
func optionStrings(in Invocation, key string) []string {
	value, _ := in.Options[key].([]string)
	return append([]string(nil), value...)
}
func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
func safeErrorMessage(err error) string {
	var typed *pmuxerr.Error
	if errorAs(err, &typed) {
		return typed.Message
	}
	return "operation failed"
}
func canceled(cause error) error {
	return pmuxerr.Wrap(cause, pmuxerr.CodeCanceled, pmuxerr.User, "Operation was canceled before commit.")
}
func normalize(cause error, code, message string) error {
	var typed *pmuxerr.Error
	if errorAs(cause, &typed) {
		return typed
	}
	return pmuxerr.Wrap(cause, code, pmuxerr.Environment, message)
}
func ensureTyped(cause error, message string) error {
	var typed *pmuxerr.Error
	if errorAs(cause, &typed) {
		return typed
	}
	return pmuxerr.Wrap(cause, pmuxerr.UnhandledInternal, pmuxerr.Internal, message)
}
func typedUsage(message string) error { return pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, message) }
func typedConfig(message, repair string) error {
	err := pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.Environment, message)
	err.Repair = []string{repair}
	return err
}
func dependencyError(message, repair string) error {
	err := pmuxerr.New(pmuxerr.CodeDependencyMissing, pmuxerr.Environment, message)
	err.Repair = []string{repair}
	return err
}
func notConfigured(action, repair string) error {
	err := pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux cannot "+action+" because no CLIProxyAPI installation is configured.")
	err.Repair = []string{repair}
	return err
}
func ownershipError(message, repair string) error {
	err := pmuxerr.New(pmuxerr.ServiceForeignOwner, pmuxerr.Environment, message)
	err.Repair = []string{repair}
	return err
}
func authError(message, repair string) error {
	err := pmuxerr.New(pmuxerr.CodeAuth, pmuxerr.Upstream, message)
	err.Repair = []string{repair}
	return err
}
func unhealthy(message, repair string) error {
	err := pmuxerr.New(pmuxerr.CodeUnhealthy, pmuxerr.Environment, message)
	err.Repair = []string{repair}
	return err
}
func sinkError(cause error) error {
	return pmuxerr.Wrap(cause, pmuxerr.UnhandledInternal, pmuxerr.Internal, "PMux could not stream an application event.")
}

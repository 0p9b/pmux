package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/0p9b/pmux/internal/app"
	pmuxtui "github.com/0p9b/pmux/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

type tuiFacade struct {
	useCases app.UseCases
	configDir string
	snapshot pmuxtui.Snapshot
}

type tuiRequest struct {
	Operation app.Operation
	Arguments []string
	Options   map[string]any
}

func runTUI(ctx context.Context, deps dependencies, flags *globalFlags, request tuiRequest) error {
	initialScreen := screenForRequest(request)
	snapshot := pmuxtui.Snapshot{Version: versionLabel(deps), Context: "local", UpdatedAt: time.Now().UTC()}
	snapshot.Dashboard.Installation = "Press r to load local status"
	snapshot.Dashboard.Service = pmuxtui.Status{Kind: pmuxtui.StatusUnknown, Text: "not loaded"}
	snapshot.Dashboard.Health = pmuxtui.Status{Kind: pmuxtui.StatusUnknown, Text: "not loaded"}
	if request.Operation == app.OpLaunch {
		snapshot.Launch.ClientPath = "claude"
		snapshot.Launch.ModelID, _ = request.Options["model"].(string)
		snapshot.Launch.Arguments = append([]string(nil), request.Arguments...)
	}
	facade := &tuiFacade{useCases: deps.UseCases, configDir: flags.ConfigDir, snapshot: snapshot}
	options := pmuxtui.Options{
		Context: ctx,
		InitialScreen: initialScreen,
		Setup: &setupWizardFacade{useCases: deps.UseCases, configDir: flags.ConfigDir, out: deps.Out},
		NoColor: os.Getenv("NO_COLOR") != "",
		Plain: os.Getenv("TERM") == "dumb",
		ReducedMotion: os.Getenv("PMUX_NO_ANIMATION") == "1",
	}
	model := pmuxtui.New(facade, facade.snapshot, options)
	programOptions := []tea.ProgramOption{tea.WithInput(deps.In), tea.WithOutput(deps.Out)}
	if !options.Plain {
		programOptions = append(programOptions, tea.WithAltScreen())
	}
	finalModel, err := tea.NewProgram(model, programOptions...).Run()
	if err != nil {
		return err
	}
	if setup, ok := finalModel.(*pmuxtui.SetupModel); ok {
		return finishSetupTUI(ctx, deps, flags, setup)
	}
	if shell, ok := finalModel.(*pmuxtui.Shell); ok {
		return finishShellTUI(ctx, facade, shell)
	}
	return nil
}

// finishShellTUI runs only after tea.Program.Run has returned and restored the
// terminal. Launch may exec on Unix, so it must never run from a Bubble Tea
// command goroutine.
func finishShellTUI(ctx context.Context, facade *tuiFacade, shell *pmuxtui.Shell) error {
	handoff, requested := shell.TakeHandoff()
	if !requested {
		return nil
	}
	_, err := facade.Execute(ctx, handoff)
	return err
}

func versionLabel(deps dependencies) string {
	if deps.Version == nil {
		return "dev"
	}
	return deps.Version().Version
}

func screenForRequest(request tuiRequest) pmuxtui.Screen {
	if request.Operation == app.OpLaunch {
		if model, _ := request.Options["model"].(string); strings.TrimSpace(model) != "" {
			return pmuxtui.Launch
		}
		return pmuxtui.Models
	}
	return screenForOperation(request.Operation)
}

func screenForOperation(operation app.Operation) pmuxtui.Screen {
	switch operation {
	case app.OpTUIProviders:
		return pmuxtui.Providers
	case app.OpTUIModels:
		return pmuxtui.Models
	case app.OpTUIService:
		return pmuxtui.Service
	case app.OpTUIConfig:
		return pmuxtui.Config
	default:
		return pmuxtui.Dashboard
	}
}

func isTUIOperation(operation app.Operation) bool {
	switch operation {
	case app.OpTUIDashboard, app.OpTUIProviders, app.OpTUIModels, app.OpTUIService, app.OpTUIConfig:
		return true
	default:
		return false
	}
}

func (f *tuiFacade) Execute(ctx context.Context, request pmuxtui.ActionRequest) (pmuxtui.Snapshot, error) {
	operation, options, err := tuiAction(request.ID)
	if err != nil {
		return f.snapshot, err
	}
	defer clearTUISecret(request.Secret)
	if len(request.Arguments) > 0 {
		switch request.ID {
		case pmuxtui.ActionProviderLogin:
			if len(request.Arguments) < 2 {
				return f.snapshot, fmt.Errorf("provider authentication method selection is required")
			}
			route := request.Arguments[1]
			switch route {
			case "browser":
				options["method"] = "browser"
				if request.ProtectedInput != nil {
					options["interactive_protected_input"] = request.ProtectedInput
				}
			case "device_code":
				options["method"] = "device"
				options["no_browser"] = true
			case "paste_callback":
				options["method"] = "browser"
				options["no_browser"] = true
				if request.ProtectedInput == nil {
					return f.snapshot, fmt.Errorf("protected callback input channel is unavailable")
				}
				options["interactive_protected_input"] = request.ProtectedInput
			case "api_key":
				if len(request.Secret) == 0 {
					return f.snapshot, fmt.Errorf("protected API-key input is required")
				}
				options["api_key_stdin"] = true
				options["protected_input"] = bytes.NewReader(request.Secret)
			case "vertex_import":
				if len(request.Arguments) < 3 {
					return f.snapshot, fmt.Errorf("Vertex service-account path is required")
				}
				options["service_account"] = request.Arguments[2]
				if len(request.Arguments) > 3 {
					options["vertex_prefix"] = request.Arguments[3]
				}
			default:
				return f.snapshot, fmt.Errorf("unsupported provider authentication method %q", route)
			}
			request.Arguments = request.Arguments[:1]
		case pmuxtui.ActionModelLaunch, pmuxtui.ActionLaunchRun:
			if len(request.Arguments) == 0 {
				return f.snapshot, fmt.Errorf("launch model selection is required")
			}
			options["model"] = request.Arguments[0]
			request.Arguments = append([]string(nil), request.Arguments[1:]...)
		case pmuxtui.ActionDoctorFix:
			options["fixes"] = append([]string(nil), request.Arguments...)
			request.Arguments = nil
		case pmuxtui.ActionLogsFollow:
			options["follow"] = true
			request.Arguments = nil
		case pmuxtui.ActionLogsExport:
			options["output"] = request.Arguments[0]
			request.Arguments = nil
		case pmuxtui.ActionLogsClear:
			options["clear"] = request.Arguments[0]
			request.Arguments = nil
		case pmuxtui.ActionLaunchPersist:
			parts := strings.SplitN(request.Arguments[0], "=", 2)
			if len(parts) != 2 || (parts[0] != "opus" && parts[0] != "sonnet" && parts[0] != "haiku") || strings.TrimSpace(parts[1]) == "" {
				return f.snapshot, fmt.Errorf("persistent assignment must be opus, sonnet, or haiku followed by =<model|unmanaged>")
			}
			operation = app.OpConfigSet
			options = map[string]any{"scope": "pmux"}
			request.Arguments = []string{"integrations.claude.persistent-models." + parts[0], parts[1]}
		}
	}
	result, err := f.executeEvents(ctx, operation, request.Arguments, options, request.Events)
	if err != nil {
		return f.snapshot, err
	}
	switch operation {
	case app.OpProvidersLogin, app.OpProvidersVerify, app.OpProvidersEnable, app.OpProvidersDisable, app.OpProvidersRemove:
		result, err = f.execute(ctx, app.OpProvidersList, nil, nil)
		operation = app.OpProvidersList
	case app.OpModelsFavorite, app.OpModelsUnfavorite, app.OpModelsTest:
		result, err = f.execute(ctx, app.OpModelsList, nil, nil)
		operation = app.OpModelsList
	case app.OpConfigSet, app.OpConfigEdit, app.OpConfigRestore:
		scope := options["scope"]
		result, err = f.execute(ctx, app.OpConfigShow, nil, map[string]any{"scope": scope})
		operation = app.OpConfigShow
	}
	if err != nil {
		return f.snapshot, err
	}
	f.project(operation, result.Data)
	return f.snapshot, nil
}

func (f *tuiFacade) execute(ctx context.Context, operation app.Operation, arguments []string, options map[string]any) (app.Result, error) {
	return f.executeEvents(ctx, operation, arguments, options, nil)
}

func (f *tuiFacade) executeEvents(ctx context.Context, operation app.Operation, arguments []string, options map[string]any, events chan<- pmuxtui.ActionEvent) (app.Result, error) {
	var lastLog time.Time
	return f.useCases.Execute(ctx, app.Invocation{Operation: operation, Arguments: arguments, Options: options, ConfigDir: f.configDir, Interactive: true, Yes: true}, func(event app.Event) error {
		if events == nil {
			return nil
		}
		if event.Type == "log" && !lastLog.IsZero() {
			wait := 250*time.Millisecond - time.Since(lastLog)
			if wait > 0 {
				timer := time.NewTimer(wait)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
		actionEvent := projectActionEvent(event)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case events <- actionEvent:
			if event.Type == "log" {
				lastLog = time.Now()
			}
			return nil
		}
	})
}

func projectActionEvent(event app.Event) pmuxtui.ActionEvent {
	projected := pmuxtui.ActionEvent{
		Type:      pmuxtui.SafeText(event.Type),
		Timestamp: event.Timestamp,
		Message:   pmuxtui.SafeText(event.Human),
		Fields:    make(map[string]string),
	}
	if values, ok := event.Data.(map[string]any); ok {
		for _, key := range []string{"provider", "flow", "url", "verification_uri", "user_code", "status", "source", "level"} {
			if value, exists := values[key]; exists {
				projected.Fields[key] = pmuxtui.SafeText(fmt.Sprint(value))
			}
		}
	}
	return projected
}

func tuiAction(id pmuxtui.ActionID) (app.Operation, map[string]any, error) {
	switch id {
	case pmuxtui.ActionDashboardRefresh, pmuxtui.ActionRecommended:
		return app.OpDashboardStatus, nil, nil
	case pmuxtui.ActionProvidersList, pmuxtui.ActionProviderDetails:
		return app.OpProvidersList, map[string]any{"refresh": true}, nil
	case pmuxtui.ActionProviderLogin:
		return app.OpProvidersLogin, map[string]any{}, nil
	case pmuxtui.ActionProviderVerify:
		return app.OpProvidersVerify, nil, nil
	case pmuxtui.ActionProviderEnable:
		return app.OpProvidersEnable, nil, nil
	case pmuxtui.ActionProviderDisable:
		return app.OpProvidersDisable, nil, nil
	case pmuxtui.ActionProviderRemove:
		return app.OpProvidersRemove, nil, nil
	case pmuxtui.ActionModelsList, pmuxtui.ActionModelDetails:
		return app.OpModelsList, map[string]any{"refresh": true}, nil
	case pmuxtui.ActionModelTest:
		return app.OpModelsTest, nil, nil
	case pmuxtui.ActionModelFavorite:
		return app.OpModelsFavorite, nil, nil
	case pmuxtui.ActionModelUnfavorite:
		return app.OpModelsUnfavorite, nil, nil
	case pmuxtui.ActionModelLaunch, pmuxtui.ActionLaunchRun:
		return app.OpLaunch, map[string]any{"client": "claude"}, nil
	case pmuxtui.ActionLaunchDoctor, pmuxtui.ActionDoctorRun, pmuxtui.ActionDoctorDetails:
		return app.OpDoctor, nil, nil
	case pmuxtui.ActionDoctorFix:
		return app.OpDoctor, map[string]any{"fix": true, "fixes": []string{}}, nil
	case pmuxtui.ActionDoctorFixAll:
		return app.OpDoctor, map[string]any{"fix": true}, nil
	case pmuxtui.ActionDoctorBundle:
		return app.OpDoctor, map[string]any{"bundle": "<default>"}, nil
	case pmuxtui.ActionServiceStatus:
		return app.OpServiceStatus, nil, nil
	case pmuxtui.ActionServiceStart:
		return app.OpServiceStart, nil, nil
	case pmuxtui.ActionServiceStop:
		return app.OpServiceStop, nil, nil
	case pmuxtui.ActionServiceRestart:
		return app.OpServiceRestart, nil, nil
	case pmuxtui.ActionServiceInstall:
		return app.OpServiceInstall, nil, nil
	case pmuxtui.ActionServiceUninstall:
		return app.OpServiceUninstall, nil, nil
	case pmuxtui.ActionServiceLogs, pmuxtui.ActionLogsList:
		return app.OpServiceLogs, map[string]any{"lines": 100}, nil
	case pmuxtui.ActionLogsFollow:
		return app.OpServiceLogs, map[string]any{"lines": 100, "follow": true}, nil
	case pmuxtui.ActionLogsExport:
		return app.OpServiceLogs, map[string]any{"lines": 100}, nil
	case pmuxtui.ActionLogsClear:
		return app.OpServiceLogs, map[string]any{}, nil
	case pmuxtui.ActionServiceForeground:
		return app.OpServiceStart, map[string]any{"foreground": true}, nil
	case pmuxtui.ActionConfigShow:
		return app.OpConfigShow, map[string]any{"scope": "proxy"}, nil
	case pmuxtui.ActionConfigGet:
		return app.OpConfigGet, map[string]any{"scope": "proxy"}, nil
	case pmuxtui.ActionConfigSet:
		return app.OpConfigSet, map[string]any{"scope": "proxy"}, nil
	case pmuxtui.ActionConfigEdit:
		return app.OpConfigEdit, map[string]any{"scope": "proxy"}, nil
	case pmuxtui.ActionConfigRestore:
		return app.OpConfigRestore, map[string]any{"scope": "proxy"}, nil
	case pmuxtui.ActionConfigBackup:
		return app.OpConfigBackup, map[string]any{"scope": "proxy"}, nil
	case pmuxtui.ActionSettingsShow:
		return app.OpConfigShow, map[string]any{"scope": "pmux"}, nil
	case pmuxtui.ActionSettingsGet:
		return app.OpConfigGet, map[string]any{"scope": "pmux"}, nil
	case pmuxtui.ActionSettingsSet, pmuxtui.ActionLaunchPersist:
		return app.OpConfigSet, map[string]any{"scope": "pmux"}, nil
	case pmuxtui.ActionSettingsRestore:
		return app.OpConfigRestore, map[string]any{"scope": "pmux"}, nil
	case pmuxtui.ActionSettingsBackup:
		return app.OpConfigBackup, map[string]any{"scope": "pmux"}, nil
	default:
		return "", nil, fmt.Errorf("TUI action %q requires a value-entry workflow that is not active", id)
	}
}

func (f *tuiFacade) project(operation app.Operation, data any) {
	f.snapshot.UpdatedAt = time.Now().UTC()
	value := genericMap(data)
	switch operation {
	case app.OpDashboardStatus, app.OpTUIDashboard:
		f.projectDashboard(value)
	case app.OpProvidersList, app.OpTUIProviders, app.OpProvidersVerify, app.OpProvidersEnable, app.OpProvidersDisable, app.OpProvidersRemove:
		f.projectProviders(value)
	case app.OpModelsList, app.OpTUIModels:
		f.projectModels(value)
	case app.OpDoctor:
		f.projectDoctor(value)
	case app.OpServiceStatus, app.OpTUIService, app.OpServiceStart, app.OpServiceStop, app.OpServiceRestart, app.OpServiceInstall, app.OpServiceUninstall:
		f.projectService(value)
	case app.OpConfigShow, app.OpTUIConfig:
		f.projectConfig(value)
	}
}

func (f *tuiFacade) projectDashboard(value map[string]any) {
	configured, _ := value["configured"].(bool)
	f.snapshot.Dashboard.Installation = "not configured"
	f.snapshot.Dashboard.Service = pmuxtui.Status{Kind: pmuxtui.StatusStopped, Text: "not installed"}
	if !configured {
		f.snapshot.Dashboard.Recommended = pmuxtui.ActionSetup
		return
	}
	installation, _ := value["installation"].(map[string]any)
	f.snapshot.Dashboard.Installation = text(installation["id"])
	f.snapshot.Dashboard.CoreVersion = text(installation["core_version"])
	f.snapshot.Dashboard.ConfigPath = text(installation["config_path"])
	f.snapshot.Dashboard.AuthDir = text(installation["auth_dir"])
	f.snapshot.Dashboard.Bind = fmt.Sprintf("%s:%v", text(installation["host"]), installation["port"])
	if status, ok := value["service"].(map[string]any); ok {
		state := text(status["state"])
		f.snapshot.Dashboard.Service = displayStatus(state)
		f.snapshot.Dashboard.Health = displayStatus(fmt.Sprint(status["healthy"]))
	}
}

func (f *tuiFacade) projectProviders(value map[string]any) {
	rows, _ := value["providers"].([]any)
	providers := make([]pmuxtui.ProviderRow, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		accounts := intValue(row["accounts"])
		flows := stringSlice(row["flows"])
		providers = append(providers, pmuxtui.ProviderRow{ID: text(row["id"]), Name: text(row["name"]), Kind: strings.Join(flows, ", "), Flows: flows, Enabled: text(row["status"]) != "disabled", Status: displayStatus(text(row["status"])), Accounts: fmt.Sprintf("%d", accounts)})
	}
	f.snapshot.Providers = providers
	f.snapshot.Dashboard.Providers = len(providers)
}

func (f *tuiFacade) projectModels(value map[string]any) {
	rows, _ := value["models"].([]any)
	models := make([]pmuxtui.ModelRow, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		providers := strings.Join(stringSlice(row["providers"]), ", ")
		models = append(models, pmuxtui.ModelRow{ID: text(row["id"]), Owner: text(row["owner"]), Provider: providers, Available: boolValue(row["available"]), Favorite: boolValue(row["favorite"]), Stale: boolValue(row["stale"])})
	}
	f.snapshot.Models = models
	f.snapshot.Dashboard.Models = len(models)
	if len(models) > 0 {
		f.snapshot.Launch.ModelID = models[0].ID
		f.snapshot.Launch.Provider = models[0].Provider
	}
}

func (f *tuiFacade) projectDoctor(value map[string]any) {
	if nested, ok := value["report"].(map[string]any); ok {
		value = nested
	}
	rows, _ := value["checks"].([]any)
	checks := make([]pmuxtui.DoctorRow, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		repair, _ := row["repair"].(map[string]any)
		checks = append(checks, pmuxtui.DoctorRow{ID: text(row["id"]), Status: displayStatus(text(row["status"])), Severity: text(row["severity"]), Summary: text(row["summary"]), Evidence: stringSlice(row["evidence"]), Fixable: boolValue(repair["available"])})
	}
	f.snapshot.Doctor = checks
}

func (f *tuiFacade) projectService(value map[string]any) {
	state := text(value["state"])
	f.snapshot.Service = pmuxtui.ServiceSnapshot{Backend: text(value["backend"]), Status: displayStatus(state), PID: intValue(value["pid"]), CoreVersion: text(value["core_version"]), Warning: text(value["warning"])}
}

func (f *tuiFacade) projectConfig(value map[string]any) {
	if values, ok := value["values"].(map[string]any); ok {
		rows := make([]pmuxtui.ConfigRow, 0, len(values))
		for key, raw := range values {
			lower := strings.ToLower(key)
			rows = append(rows, pmuxtui.ConfigRow{Key: key, Value: text(raw), Sensitive: strings.Contains(lower, "key") || strings.Contains(lower, "secret") || strings.Contains(lower, "token")})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
		f.snapshot.Config = rows
		return
	}
	keys := []string{"theme", "update_check", "default_installation", "default_client", "default_model", "log_line_limit"}
	rows := make([]pmuxtui.SettingRow, 0, len(keys))
	for _, key := range keys {
		if raw, exists := value[key]; exists {
			rows = append(rows, pmuxtui.SettingRow{Key: strings.ReplaceAll(key, "_", "-"), Value: text(raw)})
		}
	}
	f.snapshot.Settings = rows
}

func genericMap(value any) map[string]any {
	body, _ := json.Marshal(value)
	out := make(map[string]any)
	_ = json.Unmarshal(body, &out)
	return out
}

func text(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
func boolValue(value any) bool { value, _ = value.(bool); return value == true }
func intValue(value any) int { if number, ok := value.(float64); ok { return int(number) }; return 0 }
func stringSlice(value any) []string {
	rows, _ := value.([]any)
	out := make([]string, 0, len(rows))
	for _, row := range rows { out = append(out, text(row)) }
	return out
}
func displayStatus(value string) pmuxtui.Status {
	lower := strings.ToLower(value)
	kind := pmuxtui.StatusUnknown
	switch {
	case lower == "true" || lower == "running" || lower == "authenticated" || lower == "pass" || lower == "ok": kind = pmuxtui.StatusHealthy
	case lower == "stopped" || lower == "not-installed" || lower == "not-configured": kind = pmuxtui.StatusStopped
	case lower == "warning" || lower == "warn" || lower == "unknown": kind = pmuxtui.StatusWarning
	case lower == "working" || lower == "starting" || lower == "stopping": kind = pmuxtui.StatusWorking
	case lower == "false" || lower == "failed" || lower == "fail" || lower == "unavailable": kind = pmuxtui.StatusError
	}
	return pmuxtui.Status{Kind: kind, Text: value}
}

func clearTUISecret(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

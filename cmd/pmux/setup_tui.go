package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/0p9b/pmux/internal/app"
	pmuxtui "github.com/0p9b/pmux/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

type setupWizardFacade struct {
	useCases  app.UseCases
	configDir string
	out       io.Writer
}

func runSetupTUI(ctx context.Context, deps dependencies, flags *globalFlags, start setupTUIOptions) error {
	facade := &setupWizardFacade{useCases: deps.UseCases, configDir: flags.ConfigDir, out: deps.Out}
	options := pmuxtui.Options{
		Context:       ctx,
		NoColor:       os.Getenv("NO_COLOR") != "",
		Plain:         os.Getenv("TERM") == "dumb",
		ReducedMotion: os.Getenv("PMUX_NO_ANIMATION") == "1",
	}
	model := pmuxtui.NewSetup(facade, pmuxtui.SetupStart{
		Mode: start.Mode, ProxyPath: start.ProxyPath, ConfigPath: start.ConfigPath, Harden: start.Harden,
	}, options)
	programOptions := []tea.ProgramOption{tea.WithInput(deps.In), tea.WithOutput(deps.Out)}
	if !options.Plain {
		programOptions = append(programOptions, tea.WithAltScreen())
	}
	finalModel, err := tea.NewProgram(model, programOptions...).Run()
	if err != nil {
		return err
	}
	final, ok := finalModel.(*pmuxtui.SetupModel)
	if !ok {
		return fmt.Errorf("setup terminal returned unexpected model %T", finalModel)
	}
	return finishSetupTUI(ctx, deps, flags, final)
}

func finishSetupTUI(ctx context.Context, deps dependencies, flags *globalFlags, final *pmuxtui.SetupModel) error {
	progress := final.Progress()
	for _, message := range progress.Messages {
		if message != "" {
			_, _ = fmt.Fprintln(deps.Out, pmuxtui.SafeText(message))
		}
	}
	if progress.CoreComplete {
		_, _ = fmt.Fprintln(deps.Out, "CLIProxyAPI core setup is complete.")
	}
	for _, action := range progress.NextActions {
		_, _ = fmt.Fprintln(deps.Out, "Next:", pmuxtui.SafeText(action))
	}
	if !final.LaunchRequested() {
		return nil
	}
	result, err := deps.UseCases.Execute(ctx, app.Invocation{
		Operation: app.OpLaunch,
		Options:   map[string]any{"client": "claude", "model": progress.SelectedModel},
		ConfigDir: flags.ConfigDir, Interactive: true,
	}, nil)
	if err != nil {
		return err
	}
	return renderResult(deps.Out, result, false)
}

func (f *setupWizardFacade) Execute(ctx context.Context, action pmuxtui.SetupAction) (pmuxtui.SetupProgress, error) {
	switch action.Kind {
	case pmuxtui.SetupActionCore:
		result, err := f.execute(ctx, app.Invocation{
			Operation:   app.OpSetup,
			Options:     map[string]any{"mode": action.Mode, "proxy_path": action.ProxyPath, "config_path": action.ConfigPath, "harden": action.Harden},
			Interactive: true,
		}, nil)
		if err != nil {
			return pmuxtui.SetupProgress{}, err
		}
		coreComplete := boolFrom(result.Data, "core_complete")
		if action.Mode == "adopt" {
			return pmuxtui.SetupProgress{
				Stage: pmuxtui.SetupComplete, Mode: action.Mode, CoreComplete: coreComplete,
				Messages:    append(result.Human, "Adoption is complete; no provider or client setting was changed implicitly."),
				NextActions: []string{"pmux providers login <provider>", "pmux models list --refresh", "pmux launch --client claude --model <id>"},
			}, nil
		}
		return pmuxtui.SetupProgress{
			Stage: pmuxtui.SetupProviderOffer, Mode: action.Mode, CoreComplete: coreComplete,
			Messages: result.Human,
		}, nil
	case pmuxtui.SetupActionLoadProviders:
		result, err := f.execute(ctx, app.Invocation{Operation: app.OpProvidersList, Options: map[string]any{"refresh": true}, Interactive: true}, nil)
		if err != nil {
			return pmuxtui.SetupProgress{}, err
		}
		providers := setupProviderChoices(result.Data)
		if len(providers) == 0 {
			return pmuxtui.SetupProgress{Stage: pmuxtui.SetupComplete, Mode: "managed", CoreComplete: true, Messages: []string{"No provider authentication methods are available."}, NextActions: []string{"pmux providers list", "pmux doctor"}}, nil
		}
		return pmuxtui.SetupProgress{Stage: pmuxtui.SetupProviderSelect, Mode: "managed", CoreComplete: true, Providers: providers}, nil
	case pmuxtui.SetupActionLoginProvider:
		messages := make([]string, 0, 4)
		result, err := f.execute(ctx, app.Invocation{Operation: app.OpProvidersLogin, Arguments: []string{action.Provider}, Options: map[string]any{}, Interactive: true}, func(event app.Event) error {
			if line := setupEventText(event); line != "" {
				messages = append(messages, line)
				if f.out != nil {
					if err := renderEvent(f.out, app.Event{Type: event.Type, Timestamp: event.Timestamp, Data: event.Data, Human: line}, false); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return pmuxtui.SetupProgress{}, err
		}
		messages = append(messages, result.Human...)
		modelsResult, err := f.execute(ctx, app.Invocation{Operation: app.OpModelsList, Options: map[string]any{"refresh": true}, Interactive: true}, nil)
		if err != nil {
			return pmuxtui.SetupProgress{}, err
		}
		models := setupModelChoices(modelsResult.Data)
		if len(models) == 0 {
			return pmuxtui.SetupProgress{Stage: pmuxtui.SetupComplete, Mode: "managed", CoreComplete: true, Messages: append(messages, "Provider authentication completed, but no live model is available."), NextActions: []string{"pmux providers verify " + action.Provider, "pmux models list --refresh"}}, nil
		}
		return pmuxtui.SetupProgress{Stage: pmuxtui.SetupModelSelect, Mode: "managed", CoreComplete: true, Models: models, Messages: messages}, nil
	case pmuxtui.SetupActionPreflight:
		result, err := f.execute(ctx, app.Invocation{Operation: app.OpLaunchPreflight, Options: map[string]any{"client": "claude", "model": action.Model}, Interactive: true}, nil)
		if err != nil {
			return pmuxtui.SetupProgress{}, err
		}
		value := mapFrom(result.Data)
		client := nestedMap(value, "client")
		return pmuxtui.SetupProgress{
			Stage: pmuxtui.SetupLaunchOffer, Mode: "managed", CoreComplete: true, SelectedModel: action.Model,
			ClientPath: stringValue(client["path"]), ClientVersion: stringValue(client["version"]), Messages: result.Human,
		}, nil
	default:
		return pmuxtui.SetupProgress{}, fmt.Errorf("unsupported setup action %q", action.Kind)
	}
}

func (f *setupWizardFacade) execute(ctx context.Context, invocation app.Invocation, sink app.EventSink) (app.Result, error) {
	invocation.ConfigDir = f.configDir
	return f.useCases.Execute(ctx, invocation, sink)
}

func setupProviderChoices(data any) []pmuxtui.SetupChoice {
	value := mapFrom(data)
	rows, _ := value["providers"].([]any)
	choices := make([]pmuxtui.SetupChoice, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		id := stringValue(row["id"])
		if id == "" || !setupOAuthCapable(row["flows"]) {
			continue
		}
		label := stringValue(row["name"])
		if label == "" {
			label = id
		}
		choices = append(choices, pmuxtui.SetupChoice{ID: id, Label: label, Note: stringValue(row["status"])})
	}
	return choices
}
func setupOAuthCapable(value any) bool {
	flows, _ := value.([]any)
	for _, flow := range flows {
		switch stringValue(flow) {
		case "browser", "paste_callback", "device_code":
			return true
		}
	}
	return false
}

func setupModelChoices(data any) []pmuxtui.SetupChoice {
	value := mapFrom(data)
	rows, _ := value["models"].([]any)
	choices := make([]pmuxtui.SetupChoice, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		id := stringValue(row["id"])
		available, _ := row["available"].(bool)
		if id == "" || !available {
			continue
		}
		providers := make([]string, 0)
		if values, ok := row["providers"].([]any); ok {
			for _, value := range values {
				providers = append(providers, stringValue(value))
			}
		}
		choices = append(choices, pmuxtui.SetupChoice{ID: id, Label: id, Note: strings.Join(providers, ", ")})
	}
	return choices
}

func setupEventText(event app.Event) string {
	if event.Human != "" {
		return event.Human
	}
	if event.Type != "verification_required" {
		return "Provider authentication: " + event.Type
	}
	value := mapFrom(event.Data)
	parts := make([]string, 0, 2)
	if uri := stringValue(value["verification_uri"]); uri != "" {
		parts = append(parts, "Open: "+uri)
	} else if uri := stringValue(value["url"]); uri != "" {
		parts = append(parts, "Open: "+uri)
	}
	if code := stringValue(value["user_code"]); code != "" {
		parts = append(parts, "Code: "+code)
	}
	return strings.Join(parts, "\n")
}

func mapFrom(value any) map[string]any {
	encoded, _ := json.Marshal(value)
	out := make(map[string]any)
	_ = json.Unmarshal(encoded, &out)
	return out
}

func nestedMap(value map[string]any, key string) map[string]any {
	nested, _ := value[key].(map[string]any)
	return nested
}

func boolFrom(value any, key string) bool {
	result, _ := mapFrom(value)[key].(bool)
	return result
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

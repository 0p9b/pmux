package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type setupFacadeFixture struct {
	actions []SetupAction
}

func (f *setupFacadeFixture) Execute(_ context.Context, action SetupAction) (SetupProgress, error) {
	f.actions = append(f.actions, action)
	switch action.Kind {
	case SetupActionCore:
		return SetupProgress{Stage: SetupProviderOffer, Mode: action.Mode, CoreComplete: true, Messages: []string{"Core and service verified."}}, nil
	case SetupActionLoadProviders:
		return SetupProgress{Stage: SetupProviderSelect, Mode: "managed", CoreComplete: true, Providers: []SetupChoice{{ID: "runtime-provider", Label: "Runtime Provider", Note: "device code"}}}, nil
	case SetupActionLoginProvider:
		return SetupProgress{Stage: SetupModelSelect, Mode: "managed", CoreComplete: true, Models: []SetupChoice{{ID: "runtime-model/exact", Label: "runtime-model/exact", Note: action.Provider}}, Messages: []string{"Provider authenticated."}}, nil
	case SetupActionPreflight:
		return SetupProgress{Stage: SetupLaunchOffer, Mode: "managed", CoreComplete: true, SelectedModel: action.Model, ClientPath: "/bin/claude", ClientVersion: "2.1.0", Messages: []string{"Claude preflight passed."}}, nil
	default:
		return SetupProgress{}, nil
	}
}

func updateSetup(t *testing.T, model *SetupModel, message tea.Msg) (*SetupModel, tea.Cmd) {
	t.Helper()
	updated, command := model.Update(message)
	result, ok := updated.(*SetupModel)
	if !ok {
		t.Fatalf("Update returned %T, want *SetupModel", updated)
	}
	return result, command
}

func runSetupCommand(t *testing.T, model *SetupModel, command tea.Cmd) *SetupModel {
	t.Helper()
	if command == nil {
		t.Fatal("expected setup application command")
	}
	updated, _ := updateSetup(t, model, command())
	return updated
}

func TestSetupOmittedModeStartsWithManagedAdoptChoice(t *testing.T) {
	facade := &setupFacadeFixture{}
	model := NewSetup(facade, SetupStart{}, Options{Plain: true, NoColor: true, ReducedMotion: true})
	if command := model.Init(); command != nil {
		t.Fatal("omitted mode performed work before the user selected a journey")
	}
	view := model.View()
	if !strings.Contains(view, "Install a managed copy (recommended)") || !strings.Contains(view, "Adopt an existing installation") {
		t.Fatalf("mode choice missing:\n%s", view)
	}
	model, command := updateSetup(t, model, key("enter"))
	model = runSetupCommand(t, model, command)
	if model.Progress().Stage != SetupProviderOffer || !model.Progress().CoreComplete {
		t.Fatalf("managed selection did not reach verified core: %#v", model.Progress())
	}
	if len(facade.actions) != 1 || facade.actions[0].Mode != "managed" {
		t.Fatalf("actions = %#v", facade.actions)
	}
}

func TestSetupManagedHappyPathReachesExplicitLaunchOffer(t *testing.T) {
	facade := &setupFacadeFixture{}
	model := NewSetup(facade, SetupStart{}, Options{Plain: true, NoColor: true, ReducedMotion: true})

	model, command := updateSetup(t, model, key("enter")) // managed
	model = runSetupCommand(t, model, command)
	model, command = updateSetup(t, model, key("enter")) // authenticate now
	model = runSetupCommand(t, model, command)
	model, command = updateSetup(t, model, key("enter")) // exact provider
	model = runSetupCommand(t, model, command)
	if model.Progress().Stage != SetupModelSelect {
		t.Fatalf("stage after provider = %s", model.Progress().Stage)
	}
	model, command = updateSetup(t, model, key("enter")) // exact model + preflight
	model = runSetupCommand(t, model, command)
	if model.Progress().Stage != SetupLaunchOffer {
		t.Fatalf("stage = %s, want launch offer", model.Progress().Stage)
	}
	view := model.View()
	for _, required := range []string{"Claude Code 2.1.0", "runtime-model/exact", "Launch Claude Code now"} {
		if !strings.Contains(view, required) {
			t.Fatalf("launch offer missing %q:\n%s", required, view)
		}
	}
	model, command = updateSetup(t, model, key("enter"))
	if command == nil || !model.LaunchRequested() || model.Progress().Stage != SetupComplete {
		t.Fatalf("launch was not explicitly requested: launch=%v progress=%#v", model.LaunchRequested(), model.Progress())
	}
	wantKinds := []SetupActionKind{SetupActionCore, SetupActionLoadProviders, SetupActionLoginProvider, SetupActionPreflight}
	if len(facade.actions) != len(wantKinds) {
		t.Fatalf("actions = %#v", facade.actions)
	}
	for index, kind := range wantKinds {
		if facade.actions[index].Kind != kind {
			t.Fatalf("action %d = %s, want %s", index, facade.actions[index].Kind, kind)
		}
	}
}

func TestSetupCancelAfterCorePreservesSetupAndReportsResumeCommands(t *testing.T) {
	facade := &setupFacadeFixture{}
	model := NewSetup(facade, SetupStart{Mode: "managed"}, Options{Plain: true})
	model = runSetupCommand(t, model, model.Init())
	if !model.Progress().CoreComplete {
		t.Fatal("fixture did not complete core")
	}
	model, command := updateSetup(t, model, key("q"))
	if command == nil || !model.Canceled() || !model.Progress().CoreComplete {
		t.Fatalf("cancel lost completed core: %#v", model.Progress())
	}
	view := model.View()
	for _, next := range []string{"pmux providers login <provider>", "pmux models list --refresh", "pmux launch --client claude --model <id>"} {
		if !strings.Contains(view, next) {
			t.Fatalf("cancel view missing %q:\n%s", next, view)
		}
	}
}

func TestSetupPlainNoColorReducedMotionAndSecretSafe(t *testing.T) {
	model := NewSetup(nil, SetupStart{}, Options{Plain: true, NoColor: true, ReducedMotion: true})
	model.progress.Messages = []string{"Bearer complete-management-secret", "sk-complete-proxy-secret-value"}
	model.busy = true
	view := model.View()
	if strings.Contains(view, "\x1b[") || strings.Contains(view, "⠋") {
		t.Fatalf("plain reduced-motion setup view has terminal decoration: %q", view)
	}
	for _, secret := range []string{"complete-management-secret", "sk-complete-proxy-secret-value"} {
		if strings.Contains(view, secret) {
			t.Fatalf("setup view exposed %q: %s", secret, view)
		}
	}
}

func TestDashboardSetupRecommendationEntersGuidedChoice(t *testing.T) {
	facade := &setupFacadeFixture{}
	snapshot := cachedFixture()
	snapshot.Dashboard.Installation = "not configured"
	snapshot.Dashboard.Recommended = ActionSetup
	shell := New(nil, snapshot, Options{Plain: true, Setup: facade})
	updated, command := shell.Update(key("enter"))
	setup, ok := updated.(*SetupModel)
	if !ok {
		t.Fatalf("dashboard setup returned %T, want *SetupModel", updated)
	}
	if command != nil {
		t.Fatal("omitted-mode dashboard setup performed work before mode selection")
	}
	if !strings.Contains(setup.View(), "Install a managed copy (recommended)") {
		t.Fatalf("guided setup choice missing:\n%s", setup.View())
	}
}

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/0p9b/pmux/internal/app"
	"github.com/0p9b/pmux/internal/domain/client"
	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/domain/model"
	"github.com/0p9b/pmux/internal/state"
	pmuxtui "github.com/0p9b/pmux/internal/tui"
)

type setupJourneyUseCases struct {
	calls []app.Invocation
}

func (f *setupJourneyUseCases) Execute(_ context.Context, invocation app.Invocation, sink app.EventSink) (app.Result, error) {
	f.calls = append(f.calls, invocation)
	switch invocation.Operation {
	case app.OpSetup:
		return app.Result{Data: app.SetupOutcome{Installation: state.Installation{ID: "default", Kind: "managed"}, CoreComplete: true}, Human: []string{"Core ready."}}, nil
	case app.OpProvidersList:
		return app.Result{Data: map[string]any{
			"providers": []any{
				map[string]any{"id": "runtime-provider", "name": "Runtime Provider", "status": "not-configured", "flows": []any{"device_code"}},
			},
		}}, nil
	case app.OpProvidersLogin:
		if sink != nil {
			_ = sink(app.Event{Type: "auth_started", Human: "Authentication started."})
		}
		return app.Result{Data: map[string]any{"provider": "runtime-provider", "status": "complete"}}, nil
	case app.OpModelsList:
		return app.Result{Data: map[string]any{"models": []model.CatalogEntry{{ID: "runtime-model/exact", Providers: []management.ProviderID{"runtime-provider"}, Available: true, Source: "management"}}}}, nil
	case app.OpLaunchPreflight:
		return app.Result{Data: map[string]any{"ready": true, "client": client.ClientInstall{Path: "/bin/claude", Version: "2.1.0", Supported: true}, "model": "runtime-model/exact"}, Human: []string{"Claude preflight passed."}}, nil
	default:
		return app.Result{}, nil
	}
}

func TestSetupWizardFacadeUsesExistingJourneyOperations(t *testing.T) {
	useCases := &setupJourneyUseCases{}
	facade := &setupWizardFacade{useCases: useCases}
	ctx := context.Background()

	progress, err := facade.Execute(ctx, pmuxtui.SetupAction{Kind: pmuxtui.SetupActionCore, Mode: "managed"})
	if err != nil || progress.Stage != pmuxtui.SetupProviderOffer || !progress.CoreComplete {
		t.Fatalf("core progress=%#v err=%v", progress, err)
	}
	progress, err = facade.Execute(ctx, pmuxtui.SetupAction{Kind: pmuxtui.SetupActionLoadProviders})
	if err != nil || len(progress.Providers) != 1 || progress.Providers[0].ID != "runtime-provider" {
		t.Fatalf("provider progress=%#v err=%v", progress, err)
	}
	progress, err = facade.Execute(ctx, pmuxtui.SetupAction{Kind: pmuxtui.SetupActionLoginProvider, Provider: "runtime-provider"})
	if err != nil || len(progress.Models) != 1 || progress.Models[0].ID != "runtime-model/exact" {
		t.Fatalf("model progress=%#v err=%v", progress, err)
	}
	progress, err = facade.Execute(ctx, pmuxtui.SetupAction{Kind: pmuxtui.SetupActionPreflight, Model: "runtime-model/exact"})
	if err != nil || progress.Stage != pmuxtui.SetupLaunchOffer || progress.ClientVersion != "2.1.0" {
		t.Fatalf("preflight progress=%#v err=%v", progress, err)
	}
	want := []app.Operation{app.OpSetup, app.OpProvidersList, app.OpProvidersLogin, app.OpModelsList, app.OpLaunchPreflight}
	if len(useCases.calls) != len(want) {
		t.Fatalf("calls=%#v", useCases.calls)
	}
	for index, operation := range want {
		if useCases.calls[index].Operation != operation {
			t.Fatalf("call %d=%s want=%s", index, useCases.calls[index].Operation, operation)
		}
	}
	if strings.Contains(progress.ClientPath+strings.Join(progress.Messages, " "), "secret") {
		t.Fatalf("setup progress exposed a secret: %#v", progress)
	}
}

func TestUnconfiguredDashboardRecommendsGuidedSetup(t *testing.T) {
	facade := &tuiFacade{}
	facade.projectDashboard(map[string]any{"configured": false})
	if facade.snapshot.Dashboard.Recommended != pmuxtui.ActionSetup {
		t.Fatalf("recommended action = %s, want guided setup", facade.snapshot.Dashboard.Recommended)
	}
}

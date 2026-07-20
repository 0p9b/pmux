package app

import (
	"context"
	"errors"
	"testing"
	"time"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
	"github.com/0p9b/pmux/internal/domain/management"
	domainmodel "github.com/0p9b/pmux/internal/domain/model"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
)

type recordingModelTester struct {
	calls int
	model string
}

func (t *recordingModelTester) Test(_ context.Context, _ state.Installation, model string, _ time.Duration) (any, error) {
	t.calls++
	t.model = model
	return map[string]any{"model": model, "passed": true}, nil
}

func TestRouterModelTestRequiresLiveExactAvailableAttributedModel(t *testing.T) {
	const exactID = "runtime/exact:model"
	tests := []struct {
		name        string
		entry       domainmodel.CatalogEntry
		provider    string
		wantCalls   int
		wantCode    string
		wantRefresh bool
	}{
		{
			name:     "matching attributed provider",
			entry:    domainmodel.CatalogEntry{ID: exactID, Available: true, Providers: []management.ProviderID{"codex"}},
			provider: "codex", wantCalls: 1, wantRefresh: true,
		},
		{
			name:     "mismatched provider",
			entry:    domainmodel.CatalogEntry{ID: exactID, Available: true, Providers: []management.ProviderID{"codex"}},
			provider: "kimi", wantCode: pmuxerr.CodeAuth, wantRefresh: true,
		},
		{
			name:     "unknown fallback attribution",
			entry:    domainmodel.CatalogEntry{ID: exactID, Available: true, Providers: []management.ProviderID{"Unknown"}},
			provider: "codex", wantCode: pmuxerr.CodeAuth, wantRefresh: true,
		},
		{
			name:     "unavailable exact model",
			entry:    domainmodel.CatalogEntry{ID: exactID, Available: false, Providers: []management.ProviderID{"codex"}},
			provider: "codex", wantCode: pmuxerr.CodeAuth, wantRefresh: true,
		},
		{
			name:     "stale offline catalog",
			entry:    domainmodel.CatalogEntry{ID: exactID, Available: true, Stale: true, Providers: []management.ProviderID{"codex"}},
			provider: "codex", wantCode: pmuxerr.ManagementUnreachable, wantRefresh: true,
		},
		{
			name:     "missing exact model",
			entry:    domainmodel.CatalogEntry{ID: "other-runtime-model", Available: true, Providers: []management.ProviderID{"codex"}},
			provider: "codex", wantCode: pmuxerr.CodeUsage, wantRefresh: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			installation := state.Installation{ID: "default", Kind: "managed"}
			store := &memoryStore{state: state.State{Version: state.SchemaVersion, Installations: []state.Installation{installation}}}
			catalog := &fakeCatalog{entries: []domainmodel.CatalogEntry{test.entry}}
			tester := &recordingModelTester{}
			router, err := NewRouter(Dependencies{
				Roots: testRoots(), Store: store,
				Models:      func(context.Context, state.Installation) (domainmodel.ModelCatalog, error) { return catalog, nil },
				ModelTester: tester,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = router.Execute(context.Background(), Invocation{
				Operation: OpModelsTest,
				Arguments: []string{exactID},
				Options:   map[string]any{"provider": test.provider, "timeout": "1s"},
			}, nil)
			if catalog.refreshed != test.wantRefresh {
				t.Fatalf("catalog refreshed = %v, want %v", catalog.refreshed, test.wantRefresh)
			}
			if tester.calls != test.wantCalls {
				t.Fatalf("ModelTester calls = %d, want %d", tester.calls, test.wantCalls)
			}
			if test.wantCalls == 1 {
				if err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
				if tester.model != exactID {
					t.Fatalf("tested model = %q, want exact %q", tester.model, exactID)
				}
				return
			}
			var typed *pmuxerr.Error
			if !errors.As(err, &typed) || typed.Code != test.wantCode {
				t.Fatalf("error = %#v, want PMux code %q", err, test.wantCode)
			}
		})
	}
}

func TestRouterLaunchRejectsStaleCatalogBeforeLauncher(t *testing.T) {
	const exactID = "runtime/stale:model"
	installation := state.Installation{ID: "default", Kind: "managed"}
	store := &memoryStore{state: state.State{Version: state.SchemaVersion, Installations: []state.Installation{installation}}}
	catalog := &fakeCatalog{entries: []domainmodel.CatalogEntry{{
		ID: exactID, Available: true, Stale: true, Providers: []management.ProviderID{"codex"},
	}}}
	launcherCalls := 0
	router, err := NewRouter(Dependencies{
		Roots: testRoots(), Store: store,
		Models: func(context.Context, state.Installation) (domainmodel.ModelCatalog, error) { return catalog, nil },
		Launcher: func(context.Context, state.Installation, Invocation) (domainclient.ClientLauncher, error) {
			launcherCalls++
			return &fakeLauncher{}, nil
		},
		WorkingDir: func() (string, error) { return "/tmp/project", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Execute(context.Background(), Invocation{
		Operation: OpLaunch,
		Options:   map[string]any{"client": "claude", "model": exactID},
	}, nil)
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.ManagementUnreachable {
		t.Fatalf("error = %#v, want %s", err, pmuxerr.ManagementUnreachable)
	}
	if launcherCalls != 0 {
		t.Fatalf("stale catalog reached launcher factory %d time(s)", launcherCalls)
	}
	if !catalog.refreshed {
		t.Fatal("launch did not attempt a live catalog refresh")
	}
}

package models

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/domain/management"
	domainmodel "github.com/0p9b/pmux/internal/domain/model"
)

type fakeManagement struct {
	authFiles       []management.AuthFile
	authFilesErr    error
	models          map[string][]management.ModelRef
	modelErrors     map[string]error
	definitions     map[string][]management.ModelDef
	definitionError map[string]error
	public          []management.ModelRef
	publicErr       error
	publicCalls     int
}

func (f *fakeManagement) AuthFiles(context.Context) ([]management.AuthFile, error) {
	return slices.Clone(f.authFiles), f.authFilesErr
}

func (f *fakeManagement) AuthFileModels(_ context.Context, name string) ([]management.ModelRef, error) {
	return slices.Clone(f.models[name]), f.modelErrors[name]
}

func (f *fakeManagement) ModelDefinitions(_ context.Context, channel string) ([]management.ModelDef, error) {
	return slices.Clone(f.definitions[channel]), f.definitionError[channel]
}

func (f *fakeManagement) PublicModels(context.Context) ([]management.ModelRef, error) {
	f.publicCalls++
	return slices.Clone(f.public), f.publicErr
}

type memoryCache struct {
	snapshots map[string]Snapshot
	loadErr   error
	storeErr  error
}

func (m *memoryCache) Load(_ context.Context, key string) (Snapshot, error) {
	if m.loadErr != nil {
		return Snapshot{}, m.loadErr
	}
	snapshot, ok := m.snapshots[key]
	if !ok {
		return Snapshot{}, ErrCacheMiss
	}
	snapshot.Entries = cloneEntries(snapshot.Entries)
	return snapshot, nil
}

func (m *memoryCache) Store(_ context.Context, key string, snapshot Snapshot) error {
	if m.storeErr != nil {
		return m.storeErr
	}
	if m.snapshots == nil {
		m.snapshots = make(map[string]Snapshot)
	}
	snapshot.Entries = cloneEntries(snapshot.Entries)
	m.snapshots[key] = snapshot
	return nil
}

type staticFavorites struct {
	ids []string
	err error
}

func (f staticFavorites) FavoriteIDs(context.Context) ([]string, error) {
	return slices.Clone(f.ids), f.err
}

func TestRefreshManagementAttributionWinsAndDeduplicates(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 30, 0, 0, time.UTC)
	client := &fakeManagement{
		authFiles: []management.AuthFile{
			{Name: "z-account.json", Provider: "zeta"},
			{Name: "disabled.json", Provider: "ignored", Disabled: true},
			{Name: "a-account.json", Provider: "alpha"},
		},
		models: map[string][]management.ModelRef{
			"z-account.json": {
				{ID: "shared/runtime-id", Owner: "z-owner", Channel: "shared-channel"},
				{ID: "z-only", Owner: "z-owner"},
			},
			"a-account.json": {
				{ID: "new-upstream-model-2026-07-20", Owner: "novel-vendor"},
				{ID: "shared/runtime-id", Owner: "a-owner", Channel: "shared-channel"},
			},
		},
		definitions: map[string][]management.ModelDef{
			"alpha":          {{ID: "definition-only", Owner: "definition-vendor"}},
			"shared-channel": {{ID: "shared/runtime-id", Owner: "definition-vendor"}},
		},
		public: []management.ModelRef{{ID: "must-not-be-read"}},
	}
	cache := &memoryCache{}
	catalog := New(client, cache, nil, Options{Now: func() time.Time { return now }})

	got, err := catalog.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if client.publicCalls != 0 {
		t.Fatalf("PublicModels calls = %d, want 0 when management succeeds", client.publicCalls)
	}
	wantIDs := []string{"definition-only", "new-upstream-model-2026-07-20", "shared/runtime-id", "z-only"}
	if ids := entryIDs(got); !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("IDs = %#v, want %#v", ids, wantIDs)
	}

	shared := entryByID(t, got, "shared/runtime-id")
	if want := []management.ProviderID{"alpha", "zeta"}; !reflect.DeepEqual(shared.Providers, want) {
		t.Fatalf("shared providers = %#v, want %#v", shared.Providers, want)
	}
	if shared.Accounts != 2 {
		t.Fatalf("shared accounts = %d, want 2", shared.Accounts)
	}
	if shared.Owner != "a-owner" {
		t.Fatalf("shared owner = %q, want deterministic lexical owner", shared.Owner)
	}
	if shared.Source != SourceManagement || !shared.Available || shared.Stale {
		t.Fatalf("shared live state = %#v", shared)
	}
	if gotState := catalog.State(); gotState.Source != SourceManagement || gotState.Stale || gotState.FetchedAt != now {
		t.Fatalf("State() = %#v", gotState)
	}
}

func TestRefreshReflectsNewUpstreamModelsWithoutCodeCatalog(t *testing.T) {
	client := &fakeManagement{
		authFiles: []management.AuthFile{{Name: "account.json", Provider: "runtime-provider"}},
		models:    map[string][]management.ModelRef{"account.json": nil},
	}
	catalog := New(client, &memoryCache{}, nil, Options{})

	first, err := catalog.Refresh(context.Background())
	if err != nil {
		t.Fatalf("first Refresh() error = %v", err)
	}
	if len(first) != 0 {
		t.Fatalf("empty upstream produced %#v; compiled fallback catalog is forbidden", first)
	}

	const newlyPublished = "provider-published-after-pmux-build-9f7d"
	client.models["account.json"] = []management.ModelRef{{ID: newlyPublished, Owner: "runtime-only"}}
	second, err := catalog.Refresh(context.Background())
	if err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}
	if len(second) != 1 || second[0].ID != newlyPublished {
		t.Fatalf("second Refresh() = %#v, want exact newly published ID", second)
	}
}

func TestRefreshFallsBackToV1WithoutProviderAttribution(t *testing.T) {
	client := &fakeManagement{
		authFilesErr: errors.New("management models unavailable"),
		public: []management.ModelRef{
			{ID: "z-runtime", Owner: "vendor-z"},
			{ID: "a-runtime", Owner: "vendor-a", Channel: "must-not-be-trusted"},
			{ID: "a-runtime", Owner: "vendor-a"},
		},
	}
	catalog := New(client, &memoryCache{}, nil, Options{})

	got, err := catalog.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if ids := entryIDs(got); !reflect.DeepEqual(ids, []string{"a-runtime", "z-runtime"}) {
		t.Fatalf("IDs = %#v", ids)
	}
	for _, entry := range got {
		if !reflect.DeepEqual(entry.Providers, []management.ProviderID{UnknownProvider}) {
			t.Errorf("%s providers = %#v, want Unknown", entry.ID, entry.Providers)
		}
		if entry.Source != SourceV1 {
			t.Errorf("%s source = %q, want %q", entry.ID, entry.Source, SourceV1)
		}
	}
}

func TestRefreshEmptyManagementResultIsValidAndDoesNotFallback(t *testing.T) {
	client := &fakeManagement{public: []management.ModelRef{{ID: "invented-if-used"}}}
	catalog := New(client, &memoryCache{}, nil, Options{})

	got, err := catalog.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Refresh() = %#v, want valid empty result", got)
	}
	if client.publicCalls != 0 {
		t.Fatalf("PublicModels calls = %d, want 0", client.publicCalls)
	}
	if state := catalog.State(); state.Source != SourceManagement || state.Stale {
		t.Fatalf("State() = %#v", state)
	}
}

func TestRefreshOfflineReturnsTimestampedStaleCacheAndWarning(t *testing.T) {
	fetchedAt := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	cache := &memoryCache{snapshots: map[string]Snapshot{
		"instance-a": {
			FetchedAt: fetchedAt,
			Source:    SourceManagement,
			Entries: []domainmodel.CatalogEntry{{
				ID: "cached-runtime-id", Providers: []management.ProviderID{"cached-provider"}, Available: true,
			}},
		},
	}}
	client := &fakeManagement{authFilesErr: errors.New("management offline"), publicErr: errors.New("proxy offline")}
	catalog := New(client, cache, nil, Options{CacheKey: "instance-a", Now: func() time.Time {
		return fetchedAt.Add(2 * time.Hour)
	}})

	got, err := catalog.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if len(got) != 1 || !got[0].Stale || got[0].Source != SourceCache || got[0].ObservedAt != fetchedAt {
		t.Fatalf("Refresh() stale result = %#v", got)
	}
	state := catalog.State()
	if !state.Stale || state.FetchedAt != fetchedAt || state.Source != SourceCache {
		t.Fatalf("State() = %#v", state)
	}
	if !strings.Contains(state.Warning, fetchedAt.Format(time.RFC3339)) || !strings.Contains(state.Warning, "live discovery failed") {
		t.Fatalf("warning = %q, want timestamp and offline warning", state.Warning)
	}
}

func TestListFavoritesAndFilterAreLocalDeterministicOverlays(t *testing.T) {
	now := time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC)
	cache := &memoryCache{snapshots: map[string]Snapshot{"default": {
		FetchedAt: now.Add(-30 * time.Second),
		Entries: []domainmodel.CatalogEntry{
			{ID: "zeta", Owner: "Other", Providers: []management.ProviderID{"provider-z"}, Available: false},
			{ID: "alpha-model", Owner: "München Labs", Providers: []management.ProviderID{"provider-a"}, Available: true},
			{ID: "beta", Owner: "Alpha Vendor", Providers: []management.ProviderID{"provider-a"}, Available: true},
		},
	}}}
	catalog := New(&fakeManagement{}, cache, staticFavorites{ids: []string{"beta"}}, Options{Now: func() time.Time { return now }})

	got, err := catalog.Query(context.Background(), Query{Provider: "provider-a", AvailableOnly: true, FavoritesOnly: true, Search: "ALPHA"})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if ids := entryIDs(got); !reflect.DeepEqual(ids, []string{"beta"}) {
		t.Fatalf("Query IDs = %#v, want [beta]", ids)
	}
	all, err := catalog.Query(context.Background(), Query{Search: "mÜNCHEN"})
	if err != nil {
		t.Fatalf("Unicode Query() error = %v", err)
	}
	if ids := entryIDs(all); !reflect.DeepEqual(ids, []string{"alpha-model"}) {
		t.Fatalf("Unicode Query IDs = %#v", ids)
	}
}

func TestDeterministicRecordsAcrossUpstreamOrder(t *testing.T) {
	now := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)
	newClient := func(reverse bool) *fakeManagement {
		auth := []management.AuthFile{{Name: "a", Provider: "p2"}, {Name: "b", Provider: "p1"}}
		models := map[string][]management.ModelRef{
			"a": {{ID: "same", Owner: "z"}, {ID: "second", Owner: "x"}},
			"b": {{ID: "same", Owner: "a"}},
		}
		if reverse {
			slices.Reverse(auth)
			slices.Reverse(models["a"])
		}
		return &fakeManagement{authFiles: auth, models: models}
	}
	one := New(newClient(false), &memoryCache{}, nil, Options{Now: func() time.Time { return now }})
	two := New(newClient(true), &memoryCache{}, nil, Options{Now: func() time.Time { return now }})

	first, err := one.Refresh(context.Background())
	if err != nil {
		t.Fatalf("first Refresh() error = %v", err)
	}
	second, err := two.Refresh(context.Background())
	if err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("records depend on upstream order:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func entryIDs(entries []domainmodel.CatalogEntry) []string {
	ids := make([]string, len(entries))
	for i, entry := range entries {
		ids[i] = entry.ID
	}
	return ids
}

func entryByID(t *testing.T, entries []domainmodel.CatalogEntry, id string) domainmodel.CatalogEntry {
	t.Helper()
	for _, entry := range entries {
		if entry.ID == id {
			return entry
		}
	}
	t.Fatalf("entry %q not found in %#v", id, entries)
	return domainmodel.CatalogEntry{}
}

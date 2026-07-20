package models

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/0p9b/pmux/internal/domain/management"
	domainmodel "github.com/0p9b/pmux/internal/domain/model"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	SourceManagement = "management"
	SourceV1         = "v1"
	SourceCache      = "cache"

	UnknownProvider management.ProviderID = "Unknown"
)

// ErrCacheMiss is returned by a Cache when it has no snapshot for a key.
var ErrCacheMiss = errors.New("model cache miss")

// Management is the narrow part of the management plane required for model
// discovery. A management.ManagementClient satisfies this interface.
type Management interface {
	AuthFiles(context.Context) ([]management.AuthFile, error)
	AuthFileModels(context.Context, string) ([]management.ModelRef, error)
	ModelDefinitions(context.Context, string) ([]management.ModelDef, error)
	PublicModels(context.Context) ([]management.ModelRef, error)
}

// Cache persists the last complete live catalog. Implementations must treat a
// Snapshot as a value: callers may reuse or mutate their input after Store.
type Cache interface {
	Load(ctx context.Context, key string) (Snapshot, error)
	Store(ctx context.Context, key string, snapshot Snapshot) error
}

// Favorites supplies the current favorites overlay. Favorites are deliberately
// not persisted in the live cache, so preference changes take effect without a
// model refresh.
type Favorites interface {
	FavoriteIDs(ctx context.Context) ([]string, error)
}

// Snapshot is the non-secret persistent model-cache record.
type Snapshot struct {
	FetchedAt time.Time                  `json:"fetched_at"`
	Source    string                     `json:"source"`
	Entries   []domainmodel.CatalogEntry `json:"entries"`
}

// State describes how the most recently returned records were obtained.
type State struct {
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	Stale     bool      `json:"stale"`
	Warning   string    `json:"warning,omitempty"`
}

// Query is a deterministic local projection over the current catalog.
type Query struct {
	Provider      management.ProviderID
	AvailableOnly bool
	FavoritesOnly bool
	Search        string
}

type Options struct {
	CacheKey string
	MaxAge   time.Duration
	Now      func() time.Time
}

// Catalog performs management-first model discovery and implements
// domain/model.ModelCatalog.
type Catalog struct {
	management Management
	cache      Cache
	favorites  Favorites
	cacheKey   string
	maxAge     time.Duration
	now        func() time.Time

	mu    sync.RWMutex
	state State
}

var _ domainmodel.ModelCatalog = (*Catalog)(nil)

func New(managementClient Management, cache Cache, favorites Favorites, options Options) *Catalog {
	if options.CacheKey == "" {
		options.CacheKey = "default"
	}
	if options.MaxAge <= 0 {
		options.MaxAge = time.Minute
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Catalog{
		management: managementClient,
		cache:      cache,
		favorites:  favorites,
		cacheKey:   options.CacheKey,
		maxAge:     options.MaxAge,
		now:        options.Now,
	}
}

// List returns the persisted catalog without network access. Cache age controls
// stale state; it never causes a hidden refresh.
func (c *Catalog) List(ctx context.Context) ([]domainmodel.CatalogEntry, error) {
	if c.cache == nil {
		return nil, pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Model cache is not configured.")
	}
	snapshot, err := c.cache.Load(ctx, c.cacheKey)
	if err != nil {
		return nil, wrapCacheError(err)
	}
	entries, err := c.overlayFavorites(ctx, snapshot.Entries)
	if err != nil {
		return nil, err
	}

	now := c.now()
	stale := snapshot.FetchedAt.IsZero() || now.Sub(snapshot.FetchedAt) > c.maxAge
	warning := ""
	if stale {
		warning = fmt.Sprintf("Showing models cached at %s; model list may be outdated.", snapshot.FetchedAt.UTC().Format(time.RFC3339))
	}
	entries = cachedEntries(entries, snapshot.FetchedAt, stale)
	c.setState(State{Source: SourceCache, FetchedAt: snapshot.FetchedAt, Stale: stale, Warning: warning})
	return entries, nil
}

// Refresh replaces the cache only after one complete live source succeeds.
// Management attribution is authoritative. /v1/models is used only when the
// management discovery path fails, never unioned into a management result.
func (c *Catalog) Refresh(ctx context.Context) ([]domainmodel.CatalogEntry, error) {
	if c.management == nil {
		return nil, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "Model management client is not configured.")
	}

	observedAt := c.now().UTC()
	entries, managementErr := c.discoverManagement(ctx, observedAt)
	source := SourceManagement
	if managementErr != nil {
		entries, managementErr = c.discoverPublic(ctx, observedAt, managementErr)
		source = SourceV1
	}
	if managementErr != nil {
		return c.offlineCache(ctx, managementErr)
	}

	snapshot := Snapshot{FetchedAt: observedAt, Source: source, Entries: cloneEntries(entries)}
	warning := ""
	if c.cache != nil {
		if err := c.cache.Store(ctx, c.cacheKey, snapshot); err != nil {
			warning = "Live models were discovered, but the model cache could not be saved."
		}
	}
	entries, err := c.overlayFavorites(ctx, entries)
	if err != nil {
		return nil, err
	}
	c.setState(State{Source: source, FetchedAt: observedAt, Warning: warning})
	return cloneEntries(entries), nil
}

func (c *Catalog) Attribution(ctx context.Context) (map[string][]management.ProviderID, error) {
	entries, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]management.ProviderID, len(entries))
	for _, entry := range entries {
		result[entry.ID] = slices.Clone(entry.Providers)
	}
	return result, nil
}

func (c *Catalog) Query(ctx context.Context, query Query) ([]domainmodel.CatalogEntry, error) {
	entries, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	return Filter(entries, query), nil
}

func (c *Catalog) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

func (c *Catalog) discoverManagement(ctx context.Context, observedAt time.Time) ([]domainmodel.CatalogEntry, error) {
	authFiles, err := c.management.AuthFiles(ctx)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Upstream, "Management model discovery failed.")
	}
	slices.SortFunc(authFiles, func(a, b management.AuthFile) int { return strings.Compare(a.Name, b.Name) })

	modelsByID := make(map[string]*aggregate)
	channelProviders := make(map[string]map[management.ProviderID]struct{})
	providerAccounts := make(map[management.ProviderID]map[string]struct{})
	for _, authFile := range authFiles {
		if authFile.Disabled {
			continue
		}
		provider := authFile.Provider
		if provider == "" {
			provider = UnknownProvider
		}
		addSet(providerAccounts, provider, authFile.Name)
		channel := string(authFile.Provider)
		if channel != "" {
			addSet(channelProviders, channel, provider)
		}

		refs, err := c.management.AuthFileModels(ctx, authFile.Name)
		if err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Upstream, "Management credential model discovery failed.")
		}
		for _, ref := range refs {
			if ref.ID == "" {
				continue
			}
			entry := aggregateFor(modelsByID, ref.ID)
			entry.addOwner(ref.Owner)
			entry.providers[provider] = struct{}{}
			entry.accounts[authFile.Name] = struct{}{}
			if ref.Channel != "" {
				addSet(channelProviders, ref.Channel, provider)
			}
		}
	}

	channels := sortedKeys(channelProviders)
	for _, channel := range channels {
		definitions, err := c.management.ModelDefinitions(ctx, channel)
		if err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Upstream, "Management model-definition discovery failed.")
		}
		providers := sortedSet(channelProviders[channel])
		for _, definition := range definitions {
			if definition.ID == "" {
				continue
			}
			entry := aggregateFor(modelsByID, definition.ID)
			entry.addOwner(definition.Owner)
			for _, provider := range providers {
				entry.providers[provider] = struct{}{}
				for account := range providerAccounts[provider] {
					entry.accounts[account] = struct{}{}
				}
			}
		}
	}

	return renderAggregates(modelsByID, SourceManagement, observedAt), nil
}

func (c *Catalog) discoverPublic(ctx context.Context, observedAt time.Time, managementErr error) ([]domainmodel.CatalogEntry, error) {
	refs, err := c.management.PublicModels(ctx)
	if err != nil {
		return nil, pmuxerr.Wrap(errors.Join(managementErr, err), pmuxerr.ManagementUnreachable, pmuxerr.Upstream, "Could not discover models from CLIProxyAPI.")
	}
	modelsByID := make(map[string]*aggregate)
	for _, ref := range refs {
		if ref.ID == "" {
			continue
		}
		entry := aggregateFor(modelsByID, ref.ID)
		entry.addOwner(ref.Owner)
		entry.providers[UnknownProvider] = struct{}{}
	}
	return renderAggregates(modelsByID, SourceV1, observedAt), nil
}

func (c *Catalog) offlineCache(ctx context.Context, liveErr error) ([]domainmodel.CatalogEntry, error) {
	if c.cache == nil {
		return nil, liveErr
	}
	snapshot, err := c.cache.Load(ctx, c.cacheKey)
	if err != nil {
		if errors.Is(err, ErrCacheMiss) {
			return nil, liveErr
		}
		return nil, pmuxerr.Wrap(errors.Join(liveErr, err), pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Live model discovery failed and the cached catalog could not be read.")
	}
	entries, err := c.overlayFavorites(ctx, snapshot.Entries)
	if err != nil {
		return nil, err
	}
	entries = cachedEntries(entries, snapshot.FetchedAt, true)
	warning := fmt.Sprintf("Showing models cached at %s; model list may be outdated (live discovery failed: %s).", snapshot.FetchedAt.UTC().Format(time.RFC3339), liveErr.Error())
	c.setState(State{Source: SourceCache, FetchedAt: snapshot.FetchedAt, Stale: true, Warning: warning})
	return entries, nil
}

func (c *Catalog) overlayFavorites(ctx context.Context, entries []domainmodel.CatalogEntry) ([]domainmodel.CatalogEntry, error) {
	out := cloneEntries(entries)
	if c.favorites == nil {
		return out, nil
	}
	ids, err := c.favorites.FavoriteIDs(ctx)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not load model favorites.")
	}
	favorites := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		favorites[id] = struct{}{}
	}
	for i := range out {
		_, out[i].Favorite = favorites[out[i].ID]
	}
	return out, nil
}

func (c *Catalog) setState(state State) {
	c.mu.Lock()
	c.state = state
	c.mu.Unlock()
}

// Filter applies local model browsing predicates and returns a deterministic,
// detached result. Search is case-insensitive across ID, owner, and provider.
func Filter(entries []domainmodel.CatalogEntry, query Query) []domainmodel.CatalogEntry {
	needle := fold(query.Search)
	filtered := make([]domainmodel.CatalogEntry, 0, len(entries))
	for _, entry := range entries {
		if query.AvailableOnly && !entry.Available {
			continue
		}
		if query.FavoritesOnly && !entry.Favorite {
			continue
		}
		if query.Provider != "" && !slices.Contains(entry.Providers, query.Provider) {
			continue
		}
		if needle != "" && !entryMatches(entry, needle) {
			continue
		}
		filtered = append(filtered, cloneEntry(entry))
	}
	sortEntries(filtered)
	return filtered
}

func entryMatches(entry domainmodel.CatalogEntry, foldedNeedle string) bool {
	if strings.Contains(fold(entry.ID), foldedNeedle) || strings.Contains(fold(entry.Owner), foldedNeedle) {
		return true
	}
	for _, provider := range entry.Providers {
		if strings.Contains(fold(string(provider)), foldedNeedle) {
			return true
		}
	}
	return false
}

func fold(value string) string {
	return strings.Map(func(r rune) rune { return unicode.ToLower(r) }, value)
}

type aggregate struct {
	id        string
	owners    map[string]struct{}
	providers map[management.ProviderID]struct{}
	accounts  map[string]struct{}
}

func aggregateFor(all map[string]*aggregate, id string) *aggregate {
	if existing := all[id]; existing != nil {
		return existing
	}
	created := &aggregate{id: id, owners: make(map[string]struct{}), providers: make(map[management.ProviderID]struct{}), accounts: make(map[string]struct{})}
	all[id] = created
	return created
}

func (a *aggregate) addOwner(owner string) {
	if owner != "" {
		a.owners[owner] = struct{}{}
	}
}

func renderAggregates(all map[string]*aggregate, source string, observedAt time.Time) []domainmodel.CatalogEntry {
	entries := make([]domainmodel.CatalogEntry, 0, len(all))
	for _, aggregate := range all {
		owners := sortedSet(aggregate.owners)
		owner := ""
		if len(owners) != 0 {
			owner = owners[0]
		}
		providers := sortedSet(aggregate.providers)
		if len(providers) == 0 {
			providers = []management.ProviderID{UnknownProvider}
		}
		entries = append(entries, domainmodel.CatalogEntry{
			ID:         aggregate.id,
			Owner:      owner,
			Providers:  providers,
			Available:  true,
			Accounts:   len(aggregate.accounts),
			Source:     source,
			ObservedAt: observedAt,
		})
	}
	sortEntries(entries)
	return entries
}

func cachedEntries(entries []domainmodel.CatalogEntry, fetchedAt time.Time, stale bool) []domainmodel.CatalogEntry {
	out := cloneEntries(entries)
	for i := range out {
		out[i].Source = SourceCache
		out[i].ObservedAt = fetchedAt
		out[i].Stale = stale
	}
	sortEntries(out)
	return out
}

func cloneEntries(entries []domainmodel.CatalogEntry) []domainmodel.CatalogEntry {
	out := make([]domainmodel.CatalogEntry, len(entries))
	for i, entry := range entries {
		out[i] = cloneEntry(entry)
	}
	return out
}

func cloneEntry(entry domainmodel.CatalogEntry) domainmodel.CatalogEntry {
	entry.Providers = slices.Clone(entry.Providers)
	return entry
}

func sortEntries(entries []domainmodel.CatalogEntry) {
	slices.SortFunc(entries, func(a, b domainmodel.CatalogEntry) int { return strings.Compare(a.ID, b.ID) })
}

func addSet[K comparable, V comparable](sets map[K]map[V]struct{}, key K, value V) {
	set := sets[key]
	if set == nil {
		set = make(map[V]struct{})
		sets[key] = set
	}
	set[value] = struct{}{}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func sortedSet[T ~string](values map[T]struct{}) []T {
	items := make([]T, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	slices.Sort(items)
	return items
}

func wrapCacheError(err error) error {
	if errors.Is(err, ErrCacheMiss) {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "No model catalog has been cached; run `pmux models list --refresh`.")
	}
	return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not read the model cache.")
}

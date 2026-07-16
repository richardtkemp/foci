// Package modelcaps holds, per backend type, a process-wide cache of live model
// capabilities (context window, max output, effort levels, thinking modes).
//
// Design (see ~/clutch/docs/effort-per-backend-caps.md):
//   - Capabilities are a property of the BACKEND TYPE, not the model alone. Each
//     backend ("ccstream", "api", "codex") owns its own record, keyed
//     by backend type. Records are NOT shared across backend types, even though
//     the two Anthropic-backed ones both fill from the /v1/models catalogue: a
//     non-Anthropic backend (e.g. openai codex) has a different effort concept
//     and a different capability source.
//   - Each record is a process-wide singleton shared across all agents on that
//     backend, so N agents on the same backend+model trigger at most one fetch
//     per TTL.
//   - The fetch itself lives in the anthropic package (it owns the OAuth token);
//     it is injected here via SetFetcher so this package stays a pure leaf with
//     no dependency on anthropic (avoids an import cycle). Wired at startup by
//     cmd/foci-gw, once per Anthropic-backed backend key.
//   - LookupFor is best-effort: a miss returns ok=false and the caller falls
//     back to the static internal/modelinfo registry. This package never blocks
//     a command path on the network — refreshes run in the background.
//
// Consumers read caps THROUGH the agent's backend (Agent.ModelCaps), never via a
// global lookup, so each agent sees only its own backend's record.
//
// Pricing and the speed/fast flag are deliberately NOT here: the models API
// does not expose them, and pricing is reporting-only (no decision branches on
// it). They stay in internal/modelinfo.
package modelcaps

import (
	"context"
	"sort"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

const logComponent = "modelcaps"

// Backend-type keys. ccstream and api are live Anthropic-backed backends;
// codex is populated from the app-server's model/list catalogue.
const (
	BackendCCStream = "ccstream"
	BackendAPI      = "api"
	BackendCodex    = "codex"
)

// BackendKey maps an agent's configured backend name to its modelcaps key.
// Empty or "api" is the traditional API loop; both Claude Code transports
// share ccstream; Codex owns a separate app-server catalogue.
func BackendKey(configBackend string) string {
	switch configBackend {
	case "claude-code", "claude-code-tmux", "ccstream", "cctmux":
		return BackendCCStream
	case "codex":
		return BackendCodex
	case "", "api":
		return BackendAPI
	default:
		return configBackend
	}
}

// defaultTTL is how long a successful catalogue fetch is considered fresh.
// Model capabilities change only on model launches, so a long TTL is fine; the
// process also refreshes at startup and persists across restarts (DB-backed).
const defaultTTL = 48 * time.Hour

// Caps is the live capability set for a single model. Zero/empty fields mean
// "unknown" or "unsupported" — callers fall back to the static registry on a
// cache miss, and treat empty level slices as "capability not supported".
type Caps struct {
	ContextWindow int      // max input tokens (0 = unknown)
	MaxOutput     int      // max output tokens (0 = unknown)
	Effort        []string // valid effort levels in catalogue order; empty = effort unsupported
	Thinking      []string // valid thinking modes (e.g. "adaptive"); empty = unsupported
}

// Fetcher fetches the full catalogue, keyed by bare (normalized) model id.
type Fetcher func(ctx context.Context) (map[string]Caps, error)

// Persister optionally persists a backend's catalogue across process restarts.
// Injected at startup (SetPersister) so this package stays a leaf with no DB
// dependency — the sqlite-backed implementation lives in cmd/foci-gw. Both
// methods are best-effort: errors are logged, never block a fetch or a lookup.
// Save replaces the backend's persisted set; Load returns the last-persisted
// set and the time it was fetched (zero entries = nothing persisted yet).
type Persister interface {
	Save(backend string, entries map[string]Caps, fetchedAt time.Time) error
	Load(backend string) (entries map[string]Caps, fetchedAt time.Time, err error)
}

// registry holds one store per backend type.
var (
	registryMu sync.RWMutex
	registry   = map[string]*store{}
)

type store struct {
	mu        sync.RWMutex
	backend   string
	entries   map[string]Caps
	fetcher   Fetcher
	persister Persister // nil = no cross-restart persistence
	ttl       time.Duration
	lastFetch time.Time
	fetching  bool // single-flight guard
}

// getStore returns the store for a backend, creating an empty one on first use.
func getStore(backend string) *store {
	registryMu.RLock()
	s := registry[backend]
	registryMu.RUnlock()
	if s != nil {
		return s
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if s = registry[backend]; s != nil {
		return s
	}
	s = &store{backend: backend, ttl: defaultTTL}
	registry[backend] = s
	return s
}

// SetFetcher installs the catalogue fetcher for a backend (called once per
// backend at startup). Passing a nil fetcher is a no-op. Installing a fetcher
// does not itself fetch — call Refresh or rely on the background trigger in
// LookupFor.
func SetFetcher(backend string, f Fetcher) {
	if f == nil {
		return
	}
	s := getStore(backend)
	s.mu.Lock()
	s.fetcher = f
	s.mu.Unlock()
}

// SetPersister installs the cross-restart persister for a backend (called once
// at startup, before Restore). A nil persister is a no-op. Installing it does
// not itself load — call Restore to seed the cache from the DB.
func SetPersister(backend string, p Persister) {
	if p == nil {
		return
	}
	s := getStore(backend)
	s.mu.Lock()
	s.persister = p
	s.mu.Unlock()
}

// Restore seeds a backend's cache from its persister, making the last-persisted
// catalogue available immediately at startup — covering the gap before the
// first background fetch lands (without it, LookupFor falls back to the static
// modelinfo registry until the network responds). Best-effort: a miss or error
// leaves the cache cold. Call synchronously at startup BEFORE kicking off the
// background Refresh so a fast network result isn't clobbered by stale DB data;
// Restore also declines to overwrite a cache a fetch has already populated.
func Restore(backend string) {
	s := getStore(backend)
	s.mu.RLock()
	p := s.persister
	s.mu.RUnlock()
	if p == nil {
		return
	}
	entries, fetchedAt, err := p.Load(backend)
	if err != nil {
		log.NewComponentLogger(logComponent).Warnf("[%s] restore from db failed: %v", backend, err)
		return
	}
	if len(entries) == 0 {
		return
	}
	s.mu.Lock()
	if s.entries == nil { // don't clobber a fetch that already won the race
		s.entries = entries
		s.lastFetch = fetchedAt
	}
	s.mu.Unlock()
	log.NewComponentLogger(logComponent).Infof("[%s] restored %d models from db (fetched %s)",
		backend, len(entries), fetchedAt.Format(time.RFC3339))
}

// LookupFor returns the cached caps for a (backend, model) and whether a fresh
// entry was found. Model is matched by the same normalization the modelinfo
// registry uses. On a cold or stale cache it kicks off a background refresh and
// returns the current (possibly empty) state immediately — it never blocks on
// the network.
func LookupFor(backend, model string) (Caps, bool) {
	bare := modelinfo.Normalize(model)
	s := getStore(backend)

	s.mu.RLock()
	c, ok := s.entries[bare]
	stale := time.Since(s.lastFetch) > s.ttl
	hasFetcher := s.fetcher != nil
	s.mu.RUnlock()

	if (stale || !ok) && hasFetcher {
		s.triggerRefresh()
	}
	return c, ok
}

// ModelsFor returns the bare model ids the backend's catalogue currently knows,
// sorted for stable presentation. Like LookupFor it kicks off a background
// refresh on a cold or stale cache and returns the current (possibly empty)
// snapshot immediately — never blocking on the network. A cold cache returns
// nil, so callers (e.g. the /model keyboard) fall back to type-the-name.
func ModelsFor(backend string) []string {
	s := getStore(backend)

	s.mu.RLock()
	models := make([]string, 0, len(s.entries))
	for m := range s.entries {
		models = append(models, m)
	}
	stale := time.Since(s.lastFetch) > s.ttl
	hasFetcher := s.fetcher != nil
	s.mu.RUnlock()

	if (stale || len(models) == 0) && hasFetcher {
		s.triggerRefresh()
	}
	if len(models) == 0 {
		return nil
	}
	sort.Strings(models)
	return models
}

// triggerRefresh starts a background refresh unless one is already running or
// the entries are still fresh. Single-flighted per store.
func (s *store) triggerRefresh() {
	s.mu.Lock()
	fresh := time.Since(s.lastFetch) <= s.ttl && s.entries != nil
	if s.fetching || s.fetcher == nil || fresh {
		s.mu.Unlock()
		return
	}
	s.fetching = true
	fetcher := s.fetcher
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = s.doFetch(ctx, fetcher) // background refresh; errors are logged in doFetch
	}()
}

// Refresh fetches a backend's catalogue synchronously and replaces its cache.
// Used at startup (off the hot path) and in tests. Returns the fetch error, if
// any. A backend with no fetcher installed is a no-op.
func Refresh(ctx context.Context, backend string) error {
	s := getStore(backend)
	s.mu.RLock()
	fetcher := s.fetcher
	s.mu.RUnlock()
	if fetcher == nil {
		return nil
	}
	s.mu.Lock()
	s.fetching = true
	s.mu.Unlock()
	return s.doFetch(ctx, fetcher)
}

// Publish replaces a backend's catalogue with entries obtained by a live
// backend instance. It is the push counterpart to SetFetcher/Refresh for
// protocols such as Codex app-server, whose model/list call is available only
// after that instance completes its initialize handshake.
func Publish(backend string, entries map[string]Caps) {
	s := getStore(backend)
	s.storeEntries(entries, time.Now())
}

// doFetch performs the fetch and swaps in the result. Clears the single-flight
// guard on every exit. On error the previous entries are retained (serve-stale).
func (s *store) doFetch(ctx context.Context, fetcher Fetcher) error {
	entries, err := fetcher(ctx)
	s.mu.Lock()
	s.fetching = false
	if err != nil {
		n := len(s.entries)
		s.mu.Unlock()
		log.NewComponentLogger(logComponent).Warnf("[%s] catalogue refresh failed (keeping %d cached entries): %v", s.backend, n, err)
		return err
	}
	s.mu.Unlock()
	s.storeEntries(entries, time.Now())
	return nil
}

// storeEntries swaps and persists a successful catalogue snapshot.
func (s *store) storeEntries(entries map[string]Caps, fetchedAt time.Time) {
	s.mu.Lock()
	s.entries = entries
	s.lastFetch = fetchedAt
	persister := s.persister
	s.mu.Unlock()
	log.NewComponentLogger(logComponent).Infof("[%s] catalogue refreshed: %d models", s.backend, len(entries))

	// Persist outside the store lock — DB I/O must not block lookups. Sources
	// hand ownership of a completed snapshot to the store and never mutate it.
	if persister != nil {
		if err := persister.Save(s.backend, entries, fetchedAt); err != nil {
			log.NewComponentLogger(logComponent).Warnf("[%s] persist to db failed: %v", s.backend, err)
		}
	}
}

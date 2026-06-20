// Package modelcaps holds, per backend type, a process-wide cache of live model
// capabilities (context window, max output, effort levels, thinking modes).
//
// Design (see ~/clutch/docs/effort-per-backend-caps.md):
//   - Capabilities are a property of the BACKEND TYPE, not the model alone. Each
//     backend ("ccstream", "api", a future "codex") owns its own record, keyed
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
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

const logComponent = "modelcaps"

// Backend-type keys. ccstream and api are the live Anthropic-backed backends;
// future non-Anthropic backends (e.g. codex) register their own key.
const (
	BackendCCStream = "ccstream"
	BackendAPI      = "api"
)

// BackendKey maps an agent's config backend string ("", "api", "ccstream",
// "cctmux") to its modelcaps backend-type key. Empty or "api" is the
// traditional API loop; the Claude Code delegated backends share the ccstream
// key (they expose the same Anthropic catalogue).
func BackendKey(configBackend string) string {
	switch configBackend {
	case "ccstream", "cctmux":
		return BackendCCStream
	default:
		return BackendAPI
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

// doFetch performs the fetch and swaps in the result. Clears the single-flight
// guard on every exit. On error the previous entries are retained (serve-stale).
func (s *store) doFetch(ctx context.Context, fetcher Fetcher) error {
	entries, err := fetcher(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetching = false
	if err != nil {
		log.Warnf(logComponent, "[%s] catalogue refresh failed (keeping %d cached entries): %v", s.backend, len(s.entries), err)
		return err
	}
	s.entries = entries
	s.lastFetch = time.Now()
	log.Infof(logComponent, "[%s] catalogue refreshed: %d models", s.backend, len(entries))
	return nil
}

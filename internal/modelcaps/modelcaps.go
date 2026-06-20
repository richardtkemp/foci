// Package modelcaps holds a process-wide cache of live model capabilities
// (context window, max output, effort levels, thinking modes) sourced from the
// Anthropic /v1/models catalogue — the golden source for these attributes.
//
// Design (see ~/clutch/docs/model-info-from-backend.md):
//   - The cache is a process-wide singleton, shared across all agents, so N
//     agents on the same model trigger at most one fetch per TTL.
//   - The fetch itself lives in the anthropic package (it owns the OAuth token);
//     it is injected here via SetFetcher so this package stays a pure leaf with
//     no dependency on anthropic (avoids an import cycle). Wired at startup by
//     cmd/foci-gw.
//   - Lookup is best-effort: a miss returns ok=false and the caller falls back
//     to the static internal/modelinfo registry. This package never blocks a
//     command path on the network — refreshes run in the background.
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

// defaultTTL is how long a successful catalogue fetch is considered fresh.
// Model capabilities change only on model launches, so a long TTL is fine; the
// process also refreshes at startup.
const defaultTTL = 6 * time.Hour

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

// cache is the process-wide singleton.
var cache = &store{ttl: defaultTTL}

type store struct {
	mu        sync.RWMutex
	entries   map[string]Caps
	fetcher   Fetcher
	ttl       time.Duration
	lastFetch time.Time
	fetching  bool // single-flight guard
}

// SetFetcher installs the catalogue fetcher (called once at startup). Passing
// nil is a no-op. Installing a fetcher does not itself fetch — call Refresh or
// rely on the background trigger in Lookup.
func SetFetcher(f Fetcher) {
	if f == nil {
		return
	}
	cache.mu.Lock()
	cache.fetcher = f
	cache.mu.Unlock()
}

// Lookup returns the cached caps for a model (matched by the same normalization
// the modelinfo registry uses) and whether a fresh entry was found. On a cold
// or stale cache it kicks off a background refresh and returns the current
// (possibly empty) state immediately — it never blocks on the network.
func Lookup(model string) (Caps, bool) {
	bare := modelinfo.Normalize(model)

	cache.mu.RLock()
	c, ok := cache.entries[bare]
	stale := time.Since(cache.lastFetch) > cache.ttl
	hasFetcher := cache.fetcher != nil
	cache.mu.RUnlock()

	if (stale || !ok) && hasFetcher {
		triggerRefresh()
	}
	return c, ok
}

// triggerRefresh starts a background refresh unless one is already running or
// the entries are still fresh. Single-flighted.
func triggerRefresh() {
	cache.mu.Lock()
	fresh := time.Since(cache.lastFetch) <= cache.ttl && cache.entries != nil
	if cache.fetching || cache.fetcher == nil || fresh {
		cache.mu.Unlock()
		return
	}
	cache.fetching = true
	fetcher := cache.fetcher
	cache.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = doFetch(ctx, fetcher) // background refresh; errors are logged in doFetch
	}()
}

// Refresh fetches the catalogue synchronously and replaces the cache. Used at
// startup (off the hot path) and in tests. Returns the fetch error, if any.
func Refresh(ctx context.Context) error {
	cache.mu.RLock()
	fetcher := cache.fetcher
	cache.mu.RUnlock()
	if fetcher == nil {
		return nil
	}
	cache.mu.Lock()
	cache.fetching = true
	cache.mu.Unlock()
	return doFetch(ctx, fetcher)
}

// doFetch performs the fetch and swaps in the result. Clears the single-flight
// guard on every exit. On error the previous entries are retained (serve-stale).
func doFetch(ctx context.Context, fetcher Fetcher) error {
	entries, err := fetcher(ctx)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.fetching = false
	if err != nil {
		log.Warnf(logComponent, "catalogue refresh failed (keeping %d cached entries): %v", len(cache.entries), err)
		return err
	}
	cache.entries = entries
	cache.lastFetch = time.Now()
	log.Infof(logComponent, "catalogue refreshed: %d models", len(entries))
	return nil
}

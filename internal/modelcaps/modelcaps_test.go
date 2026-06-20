package modelcaps

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// tb is the backend key used by these tests.
const tb = "test"

// reset returns the per-backend registry to a clean state for an isolated test.
func reset() {
	registryMu.Lock()
	registry = map[string]*store{}
	registryMu.Unlock()
}

func TestRefreshAndLookup(t *testing.T) {
	// Proves Refresh populates the cache and LookupFor matches by normalized id.
	reset()
	SetFetcher(tb, func(_ context.Context) (map[string]Caps, error) {
		return map[string]Caps{
			"claude-opus-4-8": {ContextWindow: 1000000, Effort: []string{"low", "max"}},
		}, nil
	})
	if err := Refresh(context.Background(), tb); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Lookup with a dated/prefixed id must normalize to the bare key.
	c, ok := LookupFor(tb, "anthropic/claude-opus-4-8-20260528")
	if !ok {
		t.Fatal("LookupFor miss after Refresh")
	}
	if c.ContextWindow != 1000000 || len(c.Effort) != 2 {
		t.Errorf("caps = %+v", c)
	}

	if _, ok := LookupFor(tb, "gemini-2.5-pro"); ok {
		t.Error("unknown model should miss")
	}
}

func TestModelsFor(t *testing.T) {
	// Proves ModelsFor returns the catalogue model ids sorted, and an unknown
	// backend yields nil (a cold cache → callers fall back to type-the-name).
	reset()
	SetFetcher(tb, func(_ context.Context) (map[string]Caps, error) {
		return map[string]Caps{
			"claude-sonnet-4-6": {ContextWindow: 200000},
			"claude-opus-4-8":   {ContextWindow: 1000000},
		}, nil
	})
	if err := Refresh(context.Background(), tb); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := ModelsFor(tb)
	want := []string{"claude-opus-4-8", "claude-sonnet-4-6"} // sorted
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ModelsFor = %v, want %v", got, want)
	}
	if m := ModelsFor("never-fetched"); m != nil {
		t.Errorf("cold backend should be nil, got %v", m)
	}
}

func TestPerBackendIsolation(t *testing.T) {
	// Proves two backends keep separate records — a model fetched for one
	// backend does not leak into another.
	reset()
	SetFetcher("ccstream", func(_ context.Context) (map[string]Caps, error) {
		return map[string]Caps{"claude-opus-4-8": {Effort: []string{"low", "medium", "high", "max"}}}, nil
	})
	SetFetcher("api", func(_ context.Context) (map[string]Caps, error) {
		return map[string]Caps{"claude-opus-4-8": {Effort: []string{"low", "medium", "high"}}}, nil
	})
	if err := Refresh(context.Background(), "ccstream"); err != nil {
		t.Fatalf("ccstream Refresh: %v", err)
	}
	if err := Refresh(context.Background(), "api"); err != nil {
		t.Fatalf("api Refresh: %v", err)
	}
	cc, _ := LookupFor("ccstream", "claude-opus-4-8")
	api, _ := LookupFor("api", "claude-opus-4-8")
	if len(cc.Effort) != 4 || len(api.Effort) != 3 {
		t.Errorf("records leaked: ccstream=%v api=%v", cc.Effort, api.Effort)
	}
}

func TestServeStaleOnFetchError(t *testing.T) {
	// Proves a failed refresh keeps the previously cached entries (serve-stale).
	reset()
	good := map[string]Caps{"claude-opus-4-8": {ContextWindow: 1000000}}
	var fail bool
	SetFetcher(tb, func(_ context.Context) (map[string]Caps, error) {
		if fail {
			return nil, fmt.Errorf("boom")
		}
		return good, nil
	})
	if err := Refresh(context.Background(), tb); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	fail = true
	if err := Refresh(context.Background(), tb); err == nil {
		t.Fatal("want error on failed refresh")
	}
	if c, ok := LookupFor(tb, "claude-opus-4-8"); !ok || c.ContextWindow != 1000000 {
		t.Errorf("stale entry not retained: ok=%v caps=%+v", ok, c)
	}
}

func TestNoFetcherIsSafe(t *testing.T) {
	// Proves LookupFor/Refresh are no-ops (not panics) when no fetcher is installed.
	reset()
	if _, ok := LookupFor(tb, "claude-opus-4-8"); ok {
		t.Error("want miss with no fetcher")
	}
	if err := Refresh(context.Background(), tb); err != nil {
		t.Errorf("Refresh with no fetcher should be nil, got %v", err)
	}
}

// fakePersister is an in-memory Persister for tests, recording Save calls and
// serving a preloaded set from Load.
type fakePersister struct {
	mu         sync.Mutex
	saved      map[string]Caps
	savedAt    time.Time
	saveCalls  int
	loadReturn map[string]Caps
	loadAt     time.Time
	loadErr    error
}

func (p *fakePersister) Save(_ string, entries map[string]Caps, fetchedAt time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveCalls++
	p.saved = entries
	p.savedAt = fetchedAt
	return nil
}

func (p *fakePersister) Load(_ string) (map[string]Caps, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadReturn, p.loadAt, p.loadErr
}

func TestRestoreSeedsCacheFromPersister(t *testing.T) {
	// Proves Restore makes the persisted catalogue available immediately —
	// LookupFor hits before any fetcher runs.
	reset()
	fp := &fakePersister{
		loadReturn: map[string]Caps{"claude-opus-4-8": {ContextWindow: 999, Effort: []string{"low", "max"}}},
		loadAt:     time.Now().Add(-time.Hour),
	}
	SetPersister(tb, fp)
	Restore(tb)

	c, ok := LookupFor(tb, "anthropic/claude-opus-4-8-20260528")
	if !ok || c.ContextWindow != 999 || len(c.Effort) != 2 {
		t.Errorf("restore did not seed cache: ok=%v caps=%+v", ok, c)
	}
}

func TestRestoreEmptyIsColdMiss(t *testing.T) {
	// Proves an empty persisted set leaves the cache cold (caller falls back).
	reset()
	SetPersister(tb, &fakePersister{loadReturn: nil})
	Restore(tb)
	if _, ok := LookupFor(tb, "claude-opus-4-8"); ok {
		t.Error("empty restore should leave a cold cache")
	}
}

func TestDoFetchPersists(t *testing.T) {
	// Proves a successful Refresh persists the fetched catalogue via the
	// installed Persister (so it survives a restart).
	reset()
	fp := &fakePersister{}
	SetPersister(tb, fp)
	SetFetcher(tb, func(_ context.Context) (map[string]Caps, error) {
		return map[string]Caps{"claude-opus-4-8": {ContextWindow: 1000000, Effort: []string{"high", "max"}}}, nil
	})
	if err := Refresh(context.Background(), tb); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.saveCalls != 1 {
		t.Errorf("Save called %d times, want 1", fp.saveCalls)
	}
	if c, ok := fp.saved["claude-opus-4-8"]; !ok || c.ContextWindow != 1000000 {
		t.Errorf("persisted entries wrong: %+v", fp.saved)
	}
	if fp.savedAt.IsZero() {
		t.Error("fetchedAt not stamped on persist")
	}
}

func TestRestoreDoesNotClobberFetched(t *testing.T) {
	// Proves Restore declines to overwrite entries a fetch already populated
	// (the startup race guard).
	reset()
	SetFetcher(tb, func(_ context.Context) (map[string]Caps, error) {
		return map[string]Caps{"claude-opus-4-8": {ContextWindow: 1000000}}, nil
	})
	if err := Refresh(context.Background(), tb); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// A persister offering different (older) data must not win.
	SetPersister(tb, &fakePersister{
		loadReturn: map[string]Caps{"claude-opus-4-8": {ContextWindow: 1}},
		loadAt:     time.Now().Add(-48 * time.Hour),
	})
	Restore(tb)
	if c, _ := LookupFor(tb, "claude-opus-4-8"); c.ContextWindow != 1000000 {
		t.Errorf("restore clobbered live cache: %+v", c)
	}
}

func TestBackgroundRefreshSingleFlight(t *testing.T) {
	// Proves a cold LookupFor triggers exactly one background fetch even under
	// concurrent callers, and the result lands.
	reset()
	var mu sync.Mutex
	calls := 0
	done := make(chan struct{}, 1)
	SetFetcher(tb, func(_ context.Context) (map[string]Caps, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // hold the single-flight window open
		select {
		case done <- struct{}{}:
		default:
		}
		return map[string]Caps{"claude-opus-4-8": {ContextWindow: 1000000}}, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); LookupFor(tb, "claude-opus-4-8") }()
	}
	wg.Wait()
	<-done
	// Allow the in-flight goroutine to store its result.
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("fetcher called %d times, want 1 (single-flight)", got)
	}
	if _, ok := LookupFor(tb, "claude-opus-4-8"); !ok {
		t.Error("background refresh result did not land")
	}
}

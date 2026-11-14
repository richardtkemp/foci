package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestCacheExpiryResolution(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_000_000, 0)

	t.Run("config override wins", func(t *testing.T) {
		mgr, _ := newTestManager(t, nil)
		if _, err := mgr.Get(context.Background(), "test-agent/c1"); err != nil {
			t.Fatalf("Get: %v", err)
		}
		mgr.Spec.CacheTTL = time.Hour
		ag := &Agent{
			DelegatedManager: mgr,
			ModelDefaultsFn:  func(string) config.ModelDefaults { return config.ModelDefaults{CacheTTL: "30m"} },
		}
		if got := ag.CacheExpiry("test-agent/c1", at); !got.Equal(at.Add(30 * time.Minute)) {
			t.Errorf("got %v, want +30m", got.Sub(at))
		}
	})

	t.Run("backend TTL when no config", func(t *testing.T) {
		mgr, _ := newTestManager(t, nil)
		if _, err := mgr.Get(context.Background(), "test-agent/c1"); err != nil {
			t.Fatalf("Get: %v", err)
		}
		mgr.Spec.CacheTTL = time.Hour
		ag := &Agent{DelegatedManager: mgr}
		if got := ag.CacheExpiry("test-agent/c1", at); !got.Equal(at.Add(time.Hour)) {
			t.Errorf("got %v, want +1h", got.Sub(at))
		}
	})

	t.Run("5m default when nothing reports", func(t *testing.T) {
		ag := &Agent{}
		if got := ag.CacheExpiry("test-agent/c1", at); !got.Equal(at.Add(5 * time.Minute)) {
			t.Errorf("got %v, want +5m", got.Sub(at))
		}
	})

	t.Run("static backend TTL survives a restart (no live session yet)", func(t *testing.T) {
		// Models a freshly-restarted gateway: DelegatedManager.backends is empty
		// (no session has spawned a live backend since restart), so
		// DelegatedManager.CacheTTL(sessionKey) — which only consults a RUNNING
		// backend — reports 0. But the backend TYPE's cache TTL is a static
		// constant knowable without a live session (ccstream reports CC's 1h via
		// StaticCacheTTL, exactly as periodic_setup.go already uses at startup
		// for the keepalive-interval sanity check). Before the fix, cacheTTLFor
		// had no fallback to that static value and fell straight through to the
		// hardcoded 5m default — understating a CC session's real TTL and
		// reporting a still-warm session as cold right after restart (#1446).
		mgr := &DelegatedManager{
			NewBackend: func() (delegator.Delegator, error) {
				return &mockBackendDM{running: true}, nil
			},
			Spec: delegator.Spec{CacheTTL: time.Hour},
		}
		ag := &Agent{DelegatedManager: mgr}
		if got := ag.CacheExpiry("test-agent/c1", at); !got.Equal(at.Add(time.Hour)) {
			t.Errorf("got %v, want +1h (backend's static TTL), not the 5m default", got.Sub(at))
		}
	})
}

func TestCacheExpiryFromTouch(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()
	ag := &Agent{
		SessionIndex:    idx,
		ModelDefaultsFn: func(string) config.ModelDefaults { return config.ModelDefaults{CacheTTL: "1h"} },
	}
	sk := "test-agent/c1"

	// No touch recorded → cold (zero time), not now+TTL.
	if got := ag.CacheExpiry(sk, time.Now()); !got.IsZero() {
		t.Errorf("no-touch expiry = %v, want zero (cold)", got)
	}

	// A recorded touch → touch + TTL (NOT now + TTL).
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: sk, FilePath: "/tmp/x.jsonl", CreatedAt: time.Now(),
		SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
	})
	touch := time.Now().Add(-10 * time.Minute)
	idx.TouchCacheTouch(sk, touch)
	want := touch.Add(time.Hour)
	if got := ag.CacheExpiry(sk, time.Now()); got.Sub(want).Abs() > time.Second {
		t.Errorf("touch expiry = %v, want ≈%v (touch+1h)", got, want)
	}
}

// TestCacheExpiry_NoTouchButPriorBackendSessionIsCold captures #2061 (b): a
// session with a saved backend resume id HAS a prompt cache on the provider
// side, so a missing last_cache_touch means "warmth unknown", not "never
// cached". It must report COLD (a positive past expiry) — the client reads 0
// as WARM, which showed a long-cold chat as warm. With no resume id (never
// ran, or reset cleared it) there is nothing to re-warm, so 0 stays correct.
func TestCacheExpiry_NoTouchButPriorBackendSessionIsCold(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()
	ag := &Agent{
		SessionIndex:    idx,
		ModelDefaultsFn: func(string) config.ModelDefaults { return config.ModelDefaults{CacheTTL: "1h"} },
	}
	sk := "test-agent/c1"
	now := time.Now()

	if got := ag.CacheExpiryMs(sk, now); got != 0 {
		t.Fatalf("no resume id: CacheExpiryMs = %d, want 0 (no cache)", got)
	}

	if err := idx.SetSessionMetadata(sk, session.MetaKeyCCResumeID, "372f7d38-resume"); err != nil {
		t.Fatal(err)
	}
	got := ag.CacheExpiryMs(sk, now)
	if got <= 0 || got > now.UnixMilli() {
		t.Errorf("resume id, no touch: CacheExpiryMs = %d, want a positive past expiry (COLD)", got)
	}
}

func TestEmitCacheExpiryHook(t *testing.T) {
	t.Parallel()
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()
	ag := &Agent{
		SessionIndex:    idx,
		ModelDefaultsFn: func(string) config.ModelDefaults { return config.ModelDefaults{CacheTTL: "1h"} },
	}
	var gotSk string
	var gotMs int64
	var calls int
	ag.SetOnCacheExpiry(func(sk string, ms int64) { calls++; gotSk = sk; gotMs = ms })

	sk := "test-agent/c1"
	idx.Upsert(session.SessionIndexEntry{
		SessionKey: sk, FilePath: "/tmp/x.jsonl", CreatedAt: time.Now(),
		SessionType: session.SessionTypeChat, Status: session.SessionStatusActive,
	})
	touch := time.Now().Add(-5 * time.Minute)
	idx.TouchCacheTouch(sk, touch)

	ag.emitCacheExpiry(sk)
	if calls != 1 || gotSk != sk {
		t.Fatalf("hook fired %d times, sk=%q", calls, gotSk)
	}
	wantMs := touch.Add(time.Hour).UnixMilli()
	if diff := gotMs - wantMs; diff < -1000 || diff > 1000 {
		t.Errorf("warm ms = %d, want ≈%d (touch+1h)", gotMs, wantMs)
	}

	// Cleared touch → cold → emit 0.
	idx.ClearCacheTouch(sk)
	ag.emitCacheExpiry(sk)
	if gotMs != 0 {
		t.Errorf("cold ms = %d, want 0", gotMs)
	}
}

func TestCacheStrategyInRequest(t *testing.T) {
	t.Parallel()
	// Verify that the agent sets CacheStrategy on the API request.
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("reply"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:        client,
		Sessions:      store,
		Tools:         registry,
		Bootstrap:     bootstrap,
		Model:         "claude-haiku-4-5",
		CacheStrategy: "explicit",
		ModelDefaultsFn: func(model string) config.ModelDefaults {
			return config.ModelDefaults{CacheTTL: "1h"}
		},
	}

	ag.hmTest(context.Background(), "test/icache", "Hello")

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// CacheStrategy should be set on the request
	if receivedReq.CacheStrategy != "explicit" {
		t.Errorf("CacheStrategy = %q, want explicit", receivedReq.CacheStrategy)
	}
	if receivedReq.CacheTTL != "1h" {
		t.Errorf("CacheTTL = %q, want 1h", receivedReq.CacheTTL)
	}

	// Messages should be passed as-is (no deep copy, no markers)
	// — cache markers are applied at the translate boundary, not here.
}

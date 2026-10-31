package periodic

import (
	"testing"
	"time"

	"foci/internal/config"
)

func TestApplySettingsUpdatesRunner(t *testing.T) {
	r := New(RunnerConfig{
		AgentID:      "t",
		Keepalive:    config.ResolvedKeepalive{Enabled: false, Interval: "55m", WarmOpenAppChats: true},
		TickInterval: "30s",
	})
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	r.applySettings(Settings{
		Keepalive:              config.ResolvedKeepalive{Enabled: true, Interval: "10m", WarmOpenAppChats: false},
		Background:             config.ResolvedBackground{Enabled: true, Interval: "5m"},
		TickInterval:           "10s",
		EphemeralRetentionDays: 7,
	}, ticker)

	if !r.kaCfg.Enabled || r.kaCfg.Interval != "10m" {
		t.Errorf("keepalive not updated: %+v", r.kaCfg)
	}
	if r.kaCfg.WarmOpenAppChats {
		t.Error("warm_open_app_chats must take the live value (#2641)")
	}
	if !r.bgCfg.Enabled || r.bgCfg.Interval != "5m" {
		t.Errorf("background not updated: %+v", r.bgCfg)
	}
	if r.tickInterval != 10*time.Second {
		t.Errorf("tickInterval = %s, want 10s", r.tickInterval)
	}
	if r.ephemeralRetentionDays != 7 {
		t.Errorf("ephemeralRetentionDays = %d, want 7", r.ephemeralRetentionDays)
	}
}

func TestApplySettingsInvalidTickFallsBackToDefault(t *testing.T) {
	r := New(RunnerConfig{AgentID: "t", TickInterval: "10s"})
	r.applySettings(Settings{TickInterval: "bogus"}, nil)
	if r.tickInterval != defaultTickInterval {
		t.Errorf("tickInterval = %s, want default %s", r.tickInterval, defaultTickInterval)
	}
}

func TestUpdateSettingsCoalesces(t *testing.T) {
	r := New(RunnerConfig{AgentID: "t"})
	r.UpdateSettings(Settings{TickInterval: "1s"})
	r.UpdateSettings(Settings{TickInterval: "2s"}) // replaces the undelivered update
	select {
	case s := <-r.updateCh:
		if s.TickInterval != "2s" {
			t.Errorf("got stale pending update %q, want latest (2s)", s.TickInterval)
		}
	default:
		t.Fatal("no pending update on channel")
	}
}

func TestUpdateSettingsNilChannelIsNoop(t *testing.T) {
	r := &Runner{} // struct-literal Runner (test pattern) — must not panic or block
	r.UpdateSettings(Settings{TickInterval: "1s"})
}

// TestWarmOpenAppChatsAppliesLive pins #2641: turning warm_open_app_chats on
// by a live settings update changes which sessions keepalive warms, with no
// restart. The runner is built exactly as production builds it with the flag
// off at boot: only the unconditional open-chats resolver is wired.
func TestWarmOpenAppChatsAppliesLive(t *testing.T) {
	r := New(RunnerConfig{
		AgentID:            "test",
		Keepalive:          config.ResolvedKeepalive{Enabled: true, Interval: "1m"},
		CacheTTL:           time.Hour,
		SessionIndex:       indexWithTouch(t, "test/open", 10*time.Minute), // only the open chat is warm
		Agent:              &fakeBackgroundAgent{sessionKeyFn: func() string { return "test/main" }},
		OpenChatSessionsFn: func() []string { return []string{"test/open"} },
	})

	if ready, _ := r.keepaliveTargets(time.Minute); len(ready) != 0 {
		t.Fatalf("flag off: ready = %v, want none (the default session is not warm)", ready)
	}

	r.applySettings(Settings{Keepalive: config.ResolvedKeepalive{Enabled: true, Interval: "1m", WarmOpenAppChats: true}}, nil)
	if ready, _ := r.keepaliveTargets(time.Minute); len(ready) != 1 || ready[0] != "test/open" {
		t.Errorf("flag turned on live: ready = %v, want [test/open]", ready)
	}

	r.applySettings(Settings{Keepalive: config.ResolvedKeepalive{Enabled: true, Interval: "1m", WarmOpenAppChats: false}}, nil)
	if ready, _ := r.keepaliveTargets(time.Minute); len(ready) != 0 {
		t.Errorf("flag turned off live: ready = %v, want none", ready)
	}
}

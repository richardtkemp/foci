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
	if !r.kaCfg.WarmOpenAppChats {
		t.Error("warm_open_app_chats must keep the boot value (its OpenSessionsFn consumer is fixed at construction)")
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

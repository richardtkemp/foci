package agent

import (
	"testing"

	"foci/internal/config"
)

func TestLiveConfigFnGettersPreferLiveThenFallBack(t *testing.T) {
	var cur *config.ResolvedAgentConfig
	a := &Agent{
		Streaming:        false, // static fallbacks (direct-constructed agents)
		MaxToolLoops:     5,
		CanRunBackground: "/static",
		LiveConfigFn:     func() *config.ResolvedAgentConfig { return cur },
	}

	cur = &config.ResolvedAgentConfig{
		Display:    config.ResolvedDisplay{Streaming: true},
		Loop:       config.ResolvedLoop{MaxToolLoops: 99},
		Background: config.ResolvedBackground{CanRunBackground: "/live"},
	}
	if !a.streaming() {
		t.Error("streaming() should read live true")
	}
	if got := a.maxToolLoops(); got != 99 {
		t.Errorf("maxToolLoops() = %d, want 99 (live)", got)
	}
	if got := a.canRunBackground(); got != "/live" {
		t.Errorf("canRunBackground() = %q, want /live", got)
	}

	// A swap is visible on the next read — the hot-reload property.
	cur = &config.ResolvedAgentConfig{Loop: config.ResolvedLoop{MaxToolLoops: 7}}
	if got := a.maxToolLoops(); got != 7 {
		t.Errorf("after swap maxToolLoops() = %d, want 7", got)
	}

	// nil LiveConfigFn falls back to the static fields.
	a.LiveConfigFn = nil
	if got := a.maxToolLoops(); got != 5 {
		t.Errorf("fallback maxToolLoops() = %d, want 5", got)
	}
	if a.streaming() {
		t.Error("fallback streaming() should be false")
	}
	if got := a.canRunBackground(); got != "/static" {
		t.Errorf("fallback canRunBackground() = %q, want /static", got)
	}
}

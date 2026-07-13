package agent

import (
	"testing"
	"time"

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
		Loop:       config.ResolvedLoop{MaxToolLoops: 99, Streaming: true},
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

// TestBucketEGettersPreferLiveThenFallBack covers the #1230 getters across
// every sub-struct they read from (Loop/Summary/Display/Compaction/Behavior),
// including turnLockWarnThreshold's per-call duration re-parse.
func TestBucketEGettersPreferLiveThenFallBack(t *testing.T) {
	var cur *config.ResolvedAgentConfig
	a := &Agent{
		// static fallbacks (direct-constructed agents / nil LiveConfigFn)
		MaxOutputTokens:       111,
		DuplicateMessages:     false,
		BatchPartialJoiner:    "static",
		AutoSummarise:         false,
		MaxImagePixels:        222,
		Statusline:            "static-line",
		ShowToolCalls:         "off",
		CompactionHandoffMsg:  "static-handoff",
		ReloadOnCompact:       false,
		TurnLockWarnThreshold: 3 * time.Minute,
		LiveConfigFn:          func() *config.ResolvedAgentConfig { return cur },
	}

	cur = &config.ResolvedAgentConfig{
		Loop:       config.ResolvedLoop{MaxOutputTokens: 999, DuplicateMessages: true, BatchPartialJoiner: "live"},
		Summary:    config.ResolvedSummary{AutoSummarise: true, MaxImagePixels: 888},
		Display:    config.ResolvedDisplay{Statusline: "live-line", ShowToolCalls: "full"},
		Compaction: config.ResolvedCompaction{CompactionHandoffMsg: "live-handoff", ReloadOnCompact: true},
		Behavior:   config.ResolvedBehavior{TurnLockWarnThreshold: "90s"},
	}
	if got := a.maxOutputTokens(); got != 999 {
		t.Errorf("maxOutputTokens() = %d, want 999 (live)", got)
	}
	if !a.duplicateMessages() {
		t.Error("duplicateMessages() should read live true")
	}
	if got := a.batchPartialJoiner(); got != "live" {
		t.Errorf("batchPartialJoiner() = %q, want live", got)
	}
	if !a.autoSummarise() {
		t.Error("autoSummarise() should read live true")
	}
	if got := a.maxImagePixels(); got != 888 {
		t.Errorf("maxImagePixels() = %d, want 888 (live)", got)
	}
	if got := a.statusline(); got != "live-line" {
		t.Errorf("statusline() = %q, want live-line", got)
	}
	if got := a.showToolCalls(); got != "full" {
		t.Errorf("showToolCalls() = %q, want full", got)
	}
	if got := a.compactionHandoffMsg(); got != "live-handoff" {
		t.Errorf("compactionHandoffMsg() = %q, want live-handoff", got)
	}
	if !a.reloadOnCompact() {
		t.Error("reloadOnCompact() should read live true")
	}
	if got := a.turnLockWarnThreshold(); got != 90*time.Second {
		t.Errorf("turnLockWarnThreshold() = %v, want 90s (live, re-parsed)", got)
	}

	// Unparseable live duration → fall back to the baked value, not zero.
	cur = &config.ResolvedAgentConfig{Behavior: config.ResolvedBehavior{TurnLockWarnThreshold: "not-a-duration"}}
	if got := a.turnLockWarnThreshold(); got != 3*time.Minute {
		t.Errorf("turnLockWarnThreshold() with bad live value = %v, want 3m fallback", got)
	}

	// nil LiveConfigFn → static fallbacks.
	a.LiveConfigFn = nil
	if got := a.maxOutputTokens(); got != 111 {
		t.Errorf("fallback maxOutputTokens() = %d, want 111", got)
	}
	if a.autoSummarise() {
		t.Error("fallback autoSummarise() should be false")
	}
	if got := a.statusline(); got != "static-line" {
		t.Errorf("fallback statusline() = %q, want static-line", got)
	}
	if got := a.turnLockWarnThreshold(); got != 3*time.Minute {
		t.Errorf("fallback turnLockWarnThreshold() = %v, want 3m", got)
	}
}

package agent

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/compaction"
	"foci/internal/modelcaps"
	"foci/internal/modelinfo"
	"foci/internal/session"
)

func TestSessionEffort(t *testing.T) {
	// Proves that per-session effort overrides are isolated per session and revert to empty when cleared.
	ag := &Agent{Model: "test"}

	// Default: empty (no agent-wide default)
	if got := ag.SessionEffort("s1"); got != "" {
		t.Errorf("SessionEffort default = %q, want %q", got, "")
	}

	// Set per-session override
	ag.SetSessionEffort("s1", "high")
	if got := ag.SessionEffort("s1"); got != "high" {
		t.Errorf("SessionEffort after set = %q, want %q", got, "high")
	}

	// Other session unaffected
	if got := ag.SessionEffort("s2"); got != "" {
		t.Errorf("SessionEffort other session = %q, want %q", got, "")
	}

	// Clear override — falls back to empty
	ag.SetSessionEffort("s1", "")
	if got := ag.SessionEffort("s1"); got != "" {
		t.Errorf("SessionEffort after clear = %q, want %q", got, "")
	}
}

func TestSessionNoCompact(t *testing.T) {
	// Proves that per-session no_compact overrides work independently per session and can be cleared to restore the default false value.
	ag := &Agent{Model: "test"}

	// Default: should return false (allow compaction)
	if got := ag.SessionNoCompact("s1"); got != false {
		t.Errorf("SessionNoCompact default = %v, want %v", got, false)
	}

	// Set per-session no_compact
	ag.SetSessionNoCompact("s1", true)
	if got := ag.SessionNoCompact("s1"); got != true {
		t.Errorf("SessionNoCompact after set = %v, want %v", got, true)
	}

	// Other session unaffected
	if got := ag.SessionNoCompact("s2"); got != false {
		t.Errorf("SessionNoCompact other session = %v, want %v", got, false)
	}

	// Clear override
	ag.SetSessionNoCompact("s1", false)
	if got := ag.SessionNoCompact("s1"); got != false {
		t.Errorf("SessionNoCompact after clear = %v, want %v", got, false)
	}
}

func TestSessionSpeed(t *testing.T) {
	// Proves that per-session speed overrides are isolated per session and revert to empty when cleared.
	ag := &Agent{Model: "test"}

	// Default: falls back to agent-wide (empty = standard)
	if got := ag.SessionSpeed("s1"); got != "" {
		t.Errorf("SessionSpeed fallback = %q, want %q", got, "")
	}

	// Set per-session override
	ag.SetSessionSpeed("s1", "fast")
	if got := ag.SessionSpeed("s1"); got != "fast" {
		t.Errorf("SessionSpeed after set = %q, want %q", got, "fast")
	}

	// Other session unaffected
	if got := ag.SessionSpeed("s2"); got != "" {
		t.Errorf("SessionSpeed other session = %q, want %q", got, "")
	}

	// Clear override — falls back to agent default
	ag.SetSessionSpeed("s1", "")
	if got := ag.SessionSpeed("s1"); got != "" {
		t.Errorf("SessionSpeed after clear = %q, want %q", got, "")
	}
}

func TestSessionThinking(t *testing.T) {
	// Proves that per-session thinking mode overrides are scoped to a single session and revert to empty when cleared.
	ag := &Agent{Model: "test"}

	// Default: empty (no agent-wide default)
	if got := ag.SessionThinking("s1"); got != "" {
		t.Errorf("SessionThinking default = %q, want %q", got, "")
	}

	// Set per-session override
	ag.SetSessionThinking("s1", "adaptive")
	if got := ag.SessionThinking("s1"); got != "adaptive" {
		t.Errorf("SessionThinking after set = %q, want %q", got, "adaptive")
	}

	// Other session unaffected
	if got := ag.SessionThinking("s2"); got != "" {
		t.Errorf("SessionThinking other session = %q, want %q", got, "")
	}

	// Clear override
	ag.SetSessionThinking("s1", "")
	if got := ag.SessionThinking("s1"); got != "" {
		t.Errorf("SessionThinking after clear = %q, want %q", got, "")
	}
}

func TestSessionModel(t *testing.T) {
	// Proves that per-session model and format overrides replace the agent-wide defaults, are isolated per session, and are fully removed when cleared.
	ag := &Agent{Model: "anthropic/claude-haiku-4-5", Format: "anthropic"}

	// Default: falls back to agent-wide
	if got := ag.SessionModel("s1"); got != "anthropic/claude-haiku-4-5" {
		t.Errorf("SessionModel fallback = %q, want %q", got, "anthropic/claude-haiku-4-5")
	}

	// Set per-session override
	ag.SetSessionModel("s1", "anthropic/claude-sonnet-4-5", "anthropic", "anthropic", nil)
	if got := ag.SessionModel("s1"); got != "anthropic/claude-sonnet-4-5" {
		t.Errorf("SessionModel after set = %q, want %q", got, "anthropic/claude-sonnet-4-5")
	}
	if got := ag.SessionFormat("s1"); got != "anthropic" {
		t.Errorf("SessionFormat after set = %q, want %q", got, "anthropic")
	}

	// Other session unaffected
	if got := ag.SessionModel("s2"); got != "anthropic/claude-haiku-4-5" {
		t.Errorf("SessionModel other session = %q, want %q", got, "anthropic/claude-haiku-4-5")
	}

	// Clear override
	ag.SetSessionModel("s1", "", "", "", nil)
	if got := ag.SessionModel("s1"); got != "anthropic/claude-haiku-4-5" {
		t.Errorf("SessionModel after clear = %q, want %q", got, "anthropic/claude-haiku-4-5")
	}
}

func TestSessionContextLimit(t *testing.T) {
	// Proves that SessionContextLimit returns config-defined context when
	// ModelMetaFn matches, and falls back to modelinfo registry otherwise.
	// Use a genuinely-unknown leaf: the model resolver reduces a
	// host/dev/model id to its last segment, so the leaf must not exist in the
	// built-in registry for the "unknown → 200k default" branch to hold.
	// (A real leaf like glm-5-turbo now correctly resolves to its own window.)
	ag := &Agent{Model: "openrouter/acme/nonexistent-turbo-x"}

	// No ModelMetaFn — falls back to registry (200k default for unknown models)
	if got := ag.SessionContextLimit("s1"); got != 200_000 {
		t.Errorf("SessionContextLimit without meta = %d, want 200000", got)
	}

	// Set ModelMetaFn with config-defined context
	ag.ModelMetaFn = func(model string) modelinfo.ModelMeta {
		if model == "openrouter/acme/nonexistent-turbo-x" {
			return modelinfo.ModelMeta{ContextWindow: 202_000}
		}
		return modelinfo.ModelMeta{}
	}
	if got := ag.SessionContextLimit("s1"); got != 202_000 {
		t.Errorf("SessionContextLimit with meta = %d, want 202000", got)
	}

	// Session with per-session model override — falls back to registry
	ag.SetSessionModel("s2", "anthropic/claude-opus-4-6", "anthropic", "anthropic", nil)
	if got := ag.SessionContextLimit("s2"); got != 1_000_000 {
		t.Errorf("SessionContextLimit for opus = %d, want 1000000", got)
	}
}

// TestCompactionLimitTokens proves the app-facing helper (backs meta's
// compactionLimitTokens, #1386) mirrors the /context command's "Compaction
// at:" computation: 0 with no Compactor wired (feature disabled — the sink
// omits the field rather than sending a bogus 0-as-real-limit), else
// Compactor.EffectiveThreshold applied to SessionContextLimit.
func TestCompactionLimitTokens(t *testing.T) {
	ag := &Agent{Model: "openrouter/z-ai/glm-5-turbo"}

	if got := ag.CompactionLimitTokens("s1"); got != 0 {
		t.Errorf("CompactionLimitTokens with no Compactor = %d, want 0", got)
	}

	ag.Compactor = compaction.NewCompactor(nil, 0.8)
	ag.ModelMetaFn = func(model string) modelinfo.ModelMeta {
		return modelinfo.ModelMeta{ContextWindow: 200_000}
	}
	want := int64(ag.Compactor.EffectiveThreshold(200_000))
	if got := ag.CompactionLimitTokens("s1"); got != want {
		t.Errorf("CompactionLimitTokens = %d, want %d (EffectiveThreshold(200000))", got, want)
	}
	if want <= 0 || want >= 200_000 {
		t.Fatalf("sanity: want=%d should be a positive fraction of the 200k window", want)
	}
}

// TestModelCapabilitiesUsesConfiguredBackend proves Codex live effort support
// augments static modelinfo without leaking into an API agent using the same
// otherwise-unregistered model id.
func TestModelCapabilitiesUsesConfiguredBackend(t *testing.T) {
	modelcaps.Publish(modelcaps.BackendCodex, map[string]modelcaps.Caps{
		"codex-live-only": {Effort: []string{"low", "ultra"}},
	})
	codexAgent := &Agent{Backend: "codex"}
	if got := codexAgent.BackendType(); got != modelcaps.BackendCodex {
		t.Fatalf("BackendType = %q, want codex", got)
	}
	if !codexAgent.ModelCapabilities("codex/codex-live-only").Effort {
		t.Error("Codex live effort support was not merged")
	}
	if (&Agent{}).ModelCapabilities("codex/codex-live-only").Effort {
		t.Error("Codex live effort support leaked into API backend")
	}
}

func TestRestoreSessionOverrides(t *testing.T) {
	// Proves that session overrides (effort, thinking, speed, model, format, no_compact) survive an agent restart by persisting to and reloading from the session index.
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ag := &Agent{
		Model:        "claude-haiku-4-5",
		SessionIndex: idx,
	}

	// Persist values via setters
	ag.SetSessionEffort("s1", "high")
	ag.SetSessionThinking("s1", "adaptive")
	ag.SetSessionSpeed("s1", "fast")
	ag.SetSessionModel("s1", "anthropic/claude-opus-4-6", "anthropic", "anthropic", nil)
	ag.SetSessionNoCompact("s1", true)

	// Create a fresh agent (simulating restart) with the same session index
	ag2 := &Agent{
		Model:        "claude-haiku-4-5",
		SessionIndex: idx,
	}

	// Before restore: should fall back to empty defaults
	if got := ag2.SessionEffort("s1"); got != "" {
		t.Errorf("before restore effort = %q, want %q", got, "")
	}

	// Restore
	ag2.RestoreSessionOverrides("s1")

	// After restore: should have overrides
	if got := ag2.SessionEffort("s1"); got != "high" {
		t.Errorf("after restore effort = %q, want %q", got, "high")
	}
	if got := ag2.SessionThinking("s1"); got != "adaptive" {
		t.Errorf("after restore thinking = %q, want %q", got, "adaptive")
	}
	if got := ag2.SessionSpeed("s1"); got != "fast" {
		t.Errorf("after restore speed = %q, want %q", got, "fast")
	}
	if got := ag2.SessionModel("s1"); got != "anthropic/claude-opus-4-6" {
		t.Errorf("after restore model = %q, want %q", got, "anthropic/claude-opus-4-6")
	}
	if got := ag2.SessionFormat("s1"); got != "anthropic" {
		t.Errorf("after restore format = %q, want %q", got, "anthropic")
	}
	if got := ag2.SessionNoCompact("s1"); got != true {
		t.Errorf("after restore no_compact = %v, want %v", got, true)
	}

	// Unrelated session should still use empty defaults
	if got := ag2.SessionEffort("s2"); got != "" {
		t.Errorf("unrelated session effort = %q, want %q", got, "")
	}
}

func TestRestoreSessionOverrides_NilSessionIndex(t *testing.T) {
	// Proves that RestoreSessionOverrides is safe to call with a nil SessionIndex — it is a no-op that does not panic.
	ag := &Agent{Model: "test", SessionIndex: nil}

	// Should not panic with nil SessionIndex
	ag.RestoreSessionOverrides("s1")

	if got := ag.SessionEffort("s1"); got != "" {
		t.Errorf("effort with nil SessionIndex = %q, want %q", got, "")
	}
}

func TestHydrateLastMessageTimeFromCacheTouch(t *testing.T) {
	// Proves the restart baseline for lastMessageTime now comes from the DB
	// (last_cache_touch) via lazy hydration in getSessionMeta — replacing the
	// old disk-parsing SeedSessionMeta.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()

	sessionKey := "test/chydrate"
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  sessionKey,
		CreatedAt:   time.Now().Add(-time.Hour),
		SessionType: session.ClassifySessionKey(sessionKey),
		Status:      session.SessionStatusActive,
	})
	touch := time.Now().Add(-30 * time.Minute)
	idx.TouchCacheTouch(sessionKey, touch)

	// Fresh agent (simulating restart): first getSessionMeta hydrates from DB.
	ag := &Agent{SessionIndex: idx}
	sm := ag.getSessionMeta(sessionKey)
	if diff := sm.lastMessageTime.Sub(touch); diff > time.Second || diff < -time.Second {
		t.Errorf("hydrated lastMessageTime = %v, want ~%v (from last_cache_touch)", sm.lastMessageTime, touch)
	}
}

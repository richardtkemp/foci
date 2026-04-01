package agent

import (
	"path/filepath"
	"testing"
	"time"

	"foci/internal/modelinfo"
	"foci/internal/provider"
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
	ag := &Agent{Model: "openrouter/z-ai/glm-5-turbo"}

	// No ModelMetaFn — falls back to registry (200k default for unknown models)
	if got := ag.SessionContextLimit("s1"); got != 200_000 {
		t.Errorf("SessionContextLimit without meta = %d, want 200000", got)
	}

	// Set ModelMetaFn with config-defined context
	ag.ModelMetaFn = func(model string) modelinfo.ModelMeta {
		if model == "openrouter/z-ai/glm-5-turbo" {
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

func TestParseMetaTime(t *testing.T) {
	// Proves that parseMetaTime correctly extracts the RFC3339 timestamp from well-formed [meta] headers and returns false for missing, malformed, or unrelated strings.
	tests := []struct {
		name    string
		text    string
		wantOK  bool
		wantStr string // RFC3339 string of expected time
	}{
		{
			name:    "valid meta with gap",
			text:    "[meta] time=2026-02-23T15:43:13Z gap=3h12m model=claude-haiku-4-5",
			wantOK:  true,
			wantStr: "2026-02-23T15:43:13Z",
		},
		{
			name:    "valid meta first message",
			text:    "[meta] time=2026-01-01T00:00:00Z gap=none model=claude-haiku-4-5",
			wantOK:  true,
			wantStr: "2026-01-01T00:00:00Z",
		},
		{
			name:   "no meta prefix",
			text:   "hello world",
			wantOK: false,
		},
		{
			name:   "meta prefix but no time field",
			text:   "[meta] gap=none model=claude-haiku-4-5",
			wantOK: false,
		},
		{
			name:   "invalid time format",
			text:   "[meta] time=not-a-time gap=none",
			wantOK: false,
		},
		{
			name:   "empty string",
			text:   "",
			wantOK: false,
		},
		{
			name:   "restart marker (not meta)",
			text:   "[System restarted at 2026-02-23T15:43:13Z]",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseMetaTime(tt.text)
			if ok != tt.wantOK {
				t.Fatalf("parseMetaTime(%q) ok = %v, want %v", tt.text, ok, tt.wantOK)
			}
			if ok && got.Format(time.RFC3339) != tt.wantStr {
				t.Errorf("parseMetaTime(%q) = %v, want %v", tt.text, got.Format(time.RFC3339), tt.wantStr)
			}
		})
	}
}

func TestSeedSessionMeta(t *testing.T) {
	// Proves that SeedSessionMeta reads the most recent [meta] timestamp from a populated session so that a restarted agent knows when the last message was sent.
	store := session.NewStore(t.TempDir())
	ag := &Agent{Sessions: store, Model: "claude-haiku-4-5"}

	sessionKey := "test/iseed/1000000000"

	// Seed with empty session — should not panic
	ag.SeedSessionMeta(sessionKey)
	sm := ag.getSessionMeta(sessionKey)
	if !sm.lastMessageTime.IsZero() {
		t.Error("lastMessageTime should be zero for empty session")
	}

	// Add some messages with meta headers
	store.TestAppend(sessionKey, provider.Message{
		Role:    "user",
		Content: provider.TextContent("[meta] time=2026-02-23T10:00:00Z gap=none model=claude-haiku-4-5\nHello"),
	})
	store.TestAppend(sessionKey, provider.Message{
		Role:    "assistant",
		Content: provider.TextContent("Hi there!"),
	})
	store.TestAppend(sessionKey, provider.Message{
		Role:    "user",
		Content: provider.TextContent("[meta] time=2026-02-23T12:30:00Z gap=2h30m model=claude-haiku-4-5\nHow are you?"),
	})
	store.TestAppend(sessionKey, provider.Message{
		Role:    "assistant",
		Content: provider.TextContent("Good!"),
	})

	// Seed from a fresh agent (simulating restart)
	ag2 := &Agent{Sessions: store, Model: "claude-haiku-4-5"}
	ag2.SeedSessionMeta(sessionKey)

	sm2 := ag2.getSessionMeta(sessionKey)
	expected := time.Date(2026, 2, 23, 12, 30, 0, 0, time.UTC)
	if !sm2.lastMessageTime.Equal(expected) {
		t.Errorf("lastMessageTime = %v, want %v", sm2.lastMessageTime, expected)
	}
}

func TestSeedSessionMetaSkipsNonMetaMessages(t *testing.T) {
	// Proves that SeedSessionMeta ignores non-meta user messages (e.g. restart markers) and correctly returns the timestamp from the last genuine [meta] header.
	store := session.NewStore(t.TempDir())
	ag := &Agent{Sessions: store, Model: "claude-haiku-4-5"}

	sessionKey := "test/iseedskip/1000000000"

	// First message has meta, second user message is a restart marker (no meta)
	store.TestAppend(sessionKey, provider.Message{
		Role:    "user",
		Content: provider.TextContent("[meta] time=2026-02-23T10:00:00Z gap=none model=claude-haiku-4-5\nHello"),
	})
	store.TestAppend(sessionKey, provider.Message{
		Role:    "assistant",
		Content: provider.TextContent("Hi!"),
	})
	store.TestAppend(sessionKey, provider.Message{
		Role:    "user",
		Content: provider.TextContent("[System restarted at 2026-02-23T11:00:00Z]"),
	})

	ag.SeedSessionMeta(sessionKey)

	sm := ag.getSessionMeta(sessionKey)
	expected := time.Date(2026, 2, 23, 10, 0, 0, 0, time.UTC)
	if !sm.lastMessageTime.Equal(expected) {
		t.Errorf("lastMessageTime = %v, want %v (should skip restart marker and find first meta)", sm.lastMessageTime, expected)
	}
}


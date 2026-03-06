package agent

import (
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/state"
)

func TestSessionEffort(t *testing.T) {
	ag := &Agent{Model: "test", Effort: "low"}

	// Default: falls back to agent-wide
	if got := ag.SessionEffort("s1"); got != "low" {
		t.Errorf("SessionEffort fallback = %q, want %q", got, "low")
	}

	// Set per-session override
	ag.SetSessionEffort("s1", "high")
	if got := ag.SessionEffort("s1"); got != "high" {
		t.Errorf("SessionEffort after set = %q, want %q", got, "high")
	}

	// Other session unaffected
	if got := ag.SessionEffort("s2"); got != "low" {
		t.Errorf("SessionEffort other session = %q, want %q", got, "low")
	}

	// Clear override — falls back to agent default
	ag.SetSessionEffort("s1", "")
	if got := ag.SessionEffort("s1"); got != "low" {
		t.Errorf("SessionEffort after clear = %q, want %q", got, "low")
	}
}

func TestSessionNoCompact(t *testing.T) {
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

func TestSessionThinking(t *testing.T) {
	ag := &Agent{Model: "test", Thinking: "off"}

	// Default: falls back to agent-wide
	if got := ag.SessionThinking("s1"); got != "off" {
		t.Errorf("SessionThinking fallback = %q, want %q", got, "off")
	}

	// Set per-session override
	ag.SetSessionThinking("s1", "adaptive")
	if got := ag.SessionThinking("s1"); got != "adaptive" {
		t.Errorf("SessionThinking after set = %q, want %q", got, "adaptive")
	}

	// Other session unaffected
	if got := ag.SessionThinking("s2"); got != "off" {
		t.Errorf("SessionThinking other session = %q, want %q", got, "off")
	}

	// Clear override
	ag.SetSessionThinking("s1", "")
	if got := ag.SessionThinking("s1"); got != "off" {
		t.Errorf("SessionThinking after clear = %q, want %q", got, "off")
	}
}

func TestSessionModel(t *testing.T) {
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

func TestRestoreSessionOverrides(t *testing.T) {
	dir := t.TempDir()
	ss := state.New(dir + "/state.json")
	if err := ss.Load(); err != nil {
		t.Fatal(err)
	}

	ag := &Agent{
		Model:      "claude-haiku-4-5",
		Effort:     "low",
		Thinking:   "off",
		StateStore: ss,
	}

	// Persist values via setters
	ag.SetSessionEffort("s1", "high")
	ag.SetSessionThinking("s1", "adaptive")
	ag.SetSessionModel("s1", "anthropic/claude-opus-4-6", "anthropic", "anthropic", nil)
	ag.SetSessionNoCompact("s1", true)

	// Create a fresh agent (simulating restart) with the same state store
	ag2 := &Agent{
		Model:      "claude-haiku-4-5",
		Effort:     "low",
		Thinking:   "off",
		StateStore: ss,
	}

	// Before restore: should fall back to defaults
	if got := ag2.SessionEffort("s1"); got != "low" {
		t.Errorf("before restore effort = %q, want %q", got, "low")
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
	if got := ag2.SessionModel("s1"); got != "anthropic/claude-opus-4-6" {
		t.Errorf("after restore model = %q, want %q", got, "anthropic/claude-opus-4-6")
	}
	if got := ag2.SessionFormat("s1"); got != "anthropic" {
		t.Errorf("after restore format = %q, want %q", got, "anthropic")
	}
	if got := ag2.SessionNoCompact("s1"); got != true {
		t.Errorf("after restore no_compact = %v, want %v", got, true)
	}

	// Unrelated session should still use defaults
	if got := ag2.SessionEffort("s2"); got != "low" {
		t.Errorf("unrelated session effort = %q, want %q", got, "low")
	}
}

func TestRestoreSessionOverrides_NilStateStore(t *testing.T) {
	ag := &Agent{Model: "test", Effort: "low"}

	// Should not panic with nil state store
	ag.RestoreSessionOverrides("s1")

	if got := ag.SessionEffort("s1"); got != "low" {
		t.Errorf("effort with nil store = %q, want %q", got, "low")
	}
}

func TestParseMetaTime(t *testing.T) {
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


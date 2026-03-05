package main

import (
	"path/filepath"
	"testing"

	"foci/internal/session"
	"foci/internal/state"
)

// helper: create a state.Store pre-loaded with key/value pairs.
func stateWith(t *testing.T, kvs map[string]interface{}) *state.Store {
	t.Helper()
	dir := t.TempDir()
	s := state.New(filepath.Join(dir, "state.json"))
	for k, v := range kvs {
		if err := s.Set(k, v); err != nil {
			t.Fatalf("state.Set(%q): %v", k, err)
		}
	}
	return s
}

// helper: create a fresh SessionIndex backed by a temp db.
func tempSessionIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

func TestMigrate_AgentMetadata(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"agent:bot1:model":  "claude-3",
		"agent:bot1:effort": "high",
		"agent:bot2:model":  "gpt-4",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetAgentMetadata("bot1", "model"); v != "claude-3" {
		t.Errorf("bot1 model = %q, want %q", v, "claude-3")
	}
	if v, _ := idx.GetAgentMetadata("bot1", "effort"); v != "high" {
		t.Errorf("bot1 effort = %q, want %q", v, "high")
	}
	if v, _ := idx.GetAgentMetadata("bot2", "model"); v != "gpt-4" {
		t.Errorf("bot2 model = %q, want %q", v, "gpt-4")
	}
}

func TestMigrate_ChatMetadata(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"agent:bot1:chat:42:model":  "claude-3",
		"agent:bot1:chat:42:effort": "high",
		"agent:bot1:chat:99:model":  "gpt-4",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetChatMetadata("bot1", 42, "model"); v != "claude-3" {
		t.Errorf("chat 42 model = %q, want %q", v, "claude-3")
	}
	if v, _ := idx.GetChatMetadata("bot1", 42, "effort"); v != "high" {
		t.Errorf("chat 42 effort = %q, want %q", v, "high")
	}
	if v, _ := idx.GetChatMetadata("bot1", 99, "model"); v != "gpt-4" {
		t.Errorf("chat 99 model = %q, want %q", v, "gpt-4")
	}
}

func TestMigrate_ChatMetadata_InvalidChatID(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"agent:bot1:chat:notanumber:model": "claude",
	})
	idx := tempSessionIndex(t)

	// Should not panic, should skip the invalid key
	migrateStateToDatabase(ss, idx)

	// Nothing should have been written
	v, _ := idx.GetChatMetadata("bot1", 0, "model")
	if v != "" {
		t.Errorf("invalid chat ID should be skipped, got %q", v)
	}
}

func TestMigrate_BotPrefix(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"bot:mybot:somesetting": "value1",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	// bot: prefix maps to agent_metadata with "bot_" key prefix
	if v, _ := idx.GetAgentMetadata("mybot", "bot_somesetting"); v != "value1" {
		t.Errorf("bot prefix migration: got %q, want %q", v, "value1")
	}
}

func TestMigrate_ConsolidationLast(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"consolidation_last:bot1": "2025-01-01T00:00:00Z",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetAgentMetadata("bot1", "consolidation_last"); v != "2025-01-01T00:00:00Z" {
		t.Errorf("consolidation_last = %q, want %q", v, "2025-01-01T00:00:00Z")
	}
}

func TestMigrate_NoCompact(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"no_compact:bot/c1/1000000000": true,
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetSessionMetadata("bot/c1/1000000000", "no_compact"); v != "true" {
		t.Errorf("no_compact = %q, want %q", v, "true")
	}
}

func TestMigrate_EffortChatKey(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"effort:agent:bot1:chat:42": "high",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetChatMetadata("bot1", 42, "effort"); v != "high" {
		t.Errorf("effort chat key = %q, want %q", v, "high")
	}
}

func TestMigrate_ModelChatKey(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"model:agent:bot1:chat:7": "claude-3-opus",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetChatMetadata("bot1", 7, "model"); v != "claude-3-opus" {
		t.Errorf("model chat key = %q, want %q", v, "claude-3-opus")
	}
}

func TestMigrate_ThinkingChatKey(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"thinking:agent:bot1:chat:10": "true",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetChatMetadata("bot1", 10, "thinking"); v != "true" {
		t.Errorf("thinking chat key = %q, want %q", v, "true")
	}
}

func TestMigrate_VoiceChatKey(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"voice:agent:bot1:chat:5": "enabled",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetChatMetadata("bot1", 5, "voice"); v != "enabled" {
		t.Errorf("voice chat key = %q, want %q", v, "enabled")
	}
}

func TestMigrate_VoiceAgentKey(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"voice:agent:bot1:default_lang": "en",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetAgentMetadata("bot1", "voice_default_lang"); v != "en" {
		t.Errorf("voice agent key = %q, want %q", v, "en")
	}
}

func TestMigrate_TmuxState(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"tmux:bot1":         "session-data",
		"tmux:bot1:watches": "watch-list",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetAgentMetadata("bot1", "tmux_state"); v != "session-data" {
		t.Errorf("tmux state = %q, want %q", v, "session-data")
	}
	if v, _ := idx.GetAgentMetadata("bot1", "tmux_watches"); v != "watch-list" {
		t.Errorf("tmux watches = %q, want %q", v, "watch-list")
	}
}

func TestMigrate_SystemState(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"some_global_flag": "yes",
		"another_setting":  42,
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	if v, _ := idx.GetSystemState("some_global_flag"); v != "yes" {
		t.Errorf("system state flag = %q, want %q", v, "yes")
	}
	// Numbers get fmt.Sprintf("%v")'d
	if v, _ := idx.GetSystemState("another_setting"); v != "42" {
		t.Errorf("system state number = %q, want %q", v, "42")
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"agent:bot1:model": "claude-3",
	})
	idx := tempSessionIndex(t)

	// Run migration twice
	migrateStateToDatabase(ss, idx)
	migrateStateToDatabase(ss, idx)

	// Should still have the value (second run is a no-op thanks to marker)
	if v, _ := idx.GetAgentMetadata("bot1", "model"); v != "claude-3" {
		t.Errorf("after double migration: got %q, want %q", v, "claude-3")
	}

	// Marker should be set
	marker, _ := idx.GetSystemState("_migration_state_to_db_done")
	if marker != "true" {
		t.Errorf("migration marker = %q, want %q", marker, "true")
	}
}

func TestMigrate_MarkerPreventsRerun(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"agent:bot1:model": "claude-3",
	})
	idx := tempSessionIndex(t)

	// Pre-set the migration marker
	idx.SetSystemState("_migration_state_to_db_done", "true")

	// Run migration — should be a no-op since marker is set
	migrateStateToDatabase(ss, idx)

	// The agent metadata should NOT have been written
	v, _ := idx.GetAgentMetadata("bot1", "model")
	if v != "" {
		t.Errorf("migration should not run when marker is set, but got %q", v)
	}
}

func TestMigrate_EmptyStateStore(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{})
	idx := tempSessionIndex(t)

	// Should not panic on empty state
	migrateStateToDatabase(ss, idx)

	// Marker should still be set
	marker, _ := idx.GetSystemState("_migration_state_to_db_done")
	if marker != "true" {
		t.Errorf("migration marker = %q, want %q", marker, "true")
	}
}

func TestMigrate_MixedKeys(t *testing.T) {
	ss := stateWith(t, map[string]interface{}{
		"agent:bot1:model":              "claude-3",
		"agent:bot1:chat:42:effort":     "high",
		"bot:bot1:greeting":             "hello",
		"consolidation_last:bot1":       "2025-01-01",
		"no_compact:bot/c1/1000000000":  true,
		"effort:agent:bot1:chat:99":     "medium",      // different chat to avoid conflict with chat:42:effort
		"model:agent:bot1:chat:99":      "claude",
		"thinking:agent:bot1:chat:99":   "true",
		"voice:agent:bot1:chat:99":      "on",
		"voice:agent:bot1:default_lang": "en",
		"tmux:bot1":                     "session",
		"tmux:bot1:watches":             "watches",
		"global_flag":                   "yes",
	})
	idx := tempSessionIndex(t)

	migrateStateToDatabase(ss, idx)

	// Verify each category was routed correctly
	checks := []struct {
		desc string
		got  string
		want string
	}{
		{"agent model", must(idx.GetAgentMetadata("bot1", "model")), "claude-3"},
		{"chat effort (agent:X:chat:Y:Z)", must(idx.GetChatMetadata("bot1", 42, "effort")), "high"},
		{"bot prefix", must(idx.GetAgentMetadata("bot1", "bot_greeting")), "hello"},
		{"consolidation_last", must(idx.GetAgentMetadata("bot1", "consolidation_last")), "2025-01-01"},
		{"no_compact", must(idx.GetSessionMetadata("bot/c1/1000000000", "no_compact")), "true"},
		{"effort chat (effort:agent:)", must(idx.GetChatMetadata("bot1", 99, "effort")), "medium"},
		{"model chat", must(idx.GetChatMetadata("bot1", 99, "model")), "claude"},
		{"thinking chat", must(idx.GetChatMetadata("bot1", 99, "thinking")), "true"},
		{"voice chat", must(idx.GetChatMetadata("bot1", 99, "voice")), "on"},
		{"voice agent", must(idx.GetAgentMetadata("bot1", "voice_default_lang")), "en"},
		{"tmux state", must(idx.GetAgentMetadata("bot1", "tmux_state")), "session"},
		{"tmux watches", must(idx.GetAgentMetadata("bot1", "tmux_watches")), "watches"},
		{"system state", must(idx.GetSystemState("global_flag")), "yes"},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.desc, c.got, c.want)
		}
	}
}

// must extracts the string value, ignoring the error (for table-driven test readability).
func must(v string, _ error) string { return v }

package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/session"
	"foci/internal/tools"
)

func TestSessionDisplayOverrides(t *testing.T) {
	// Proves that all four per-session display settings start empty, can be set to specific values, and are all cleared together by ClearSessionDisplayOverrides.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ag := &Agent{SessionIndex: idx, AsyncNotifier: tools.NewAsyncNotifier(func(_, _, _, _ string) {})}
	sk := "bot/c100"

	// Initially empty
	if v := ag.SessionShowToolCalls(sk); v != "" {
		t.Errorf("initial show_tool_calls = %q, want empty", v)
	}
	if v := ag.SessionDisplayShowThinking(sk); v != "" {
		t.Errorf("initial display_show_thinking = %q, want empty", v)
	}
	if v := ag.SessionStreamOutput(sk); v != "" {
		t.Errorf("initial stream_output = %q, want empty", v)
	}
	if v := ag.SessionDisplayWidth(sk); v != "" {
		t.Errorf("initial display_width = %q, want empty", v)
	}

	// Set overrides
	ag.SetSessionShowToolCalls(sk, "full")
	ag.SetSessionDisplayShowThinking(sk, "compact")
	ag.SetSessionStreamOutput(sk, "true")
	ag.SetSessionDisplayWidth(sk, "80")

	if v := ag.SessionShowToolCalls(sk); v != "full" {
		t.Errorf("show_tool_calls = %q, want full", v)
	}
	if v := ag.SessionDisplayShowThinking(sk); v != "compact" {
		t.Errorf("display_show_thinking = %q, want compact", v)
	}
	if v := ag.SessionStreamOutput(sk); v != "true" {
		t.Errorf("stream_output = %q, want true", v)
	}
	if v := ag.SessionDisplayWidth(sk); v != "80" {
		t.Errorf("display_width = %q, want 80", v)
	}

	// Clear all
	ag.ClearSessionDisplayOverrides(sk)

	if v := ag.SessionShowToolCalls(sk); v != "" {
		t.Errorf("after clear show_tool_calls = %q, want empty", v)
	}
	if v := ag.SessionStreamOutput(sk); v != "" {
		t.Errorf("after clear stream_output = %q, want empty", v)
	}
}

func TestSessionDisplayOverrides_Restore(t *testing.T) {
	// Proves that display overrides survive an agent restart by writing to and reading from the shared state store via RestoreSessionOverrides.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ag := &Agent{SessionIndex: idx, AsyncNotifier: tools.NewAsyncNotifier(func(_, _, _, _ string) {})}
	sk := "bot/c100"

	ag.SetSessionShowToolCalls(sk, "preview")
	ag.SetSessionDisplayShowThinking(sk, "true")
	ag.SetSessionStreamOutput(sk, "false")
	ag.SetSessionDisplayWidth(sk, "60")

	// Simulate restart: new Agent, same session index
	ag2 := &Agent{SessionIndex: idx, AsyncNotifier: tools.NewAsyncNotifier(func(_, _, _, _ string) {})}
	ag2.RestoreSessionOverrides(sk)

	if v := ag2.SessionShowToolCalls(sk); v != "preview" {
		t.Errorf("restored show_tool_calls = %q, want preview", v)
	}
	if v := ag2.SessionDisplayShowThinking(sk); v != "true" {
		t.Errorf("restored display_show_thinking = %q, want true", v)
	}
	if v := ag2.SessionStreamOutput(sk); v != "false" {
		t.Errorf("restored stream_output = %q, want false", v)
	}
	if v := ag2.SessionDisplayWidth(sk); v != "60" {
		t.Errorf("restored display_width = %q, want 60", v)
	}
}

func TestSessionShowToolCalls_AgentDefault(t *testing.T) {
	// Proves that SessionShowToolCalls returns the agent-level ShowToolCalls default
	// when no per-session override is set, and that a per-session override takes precedence.
	ag := &Agent{ShowToolCalls: "preview", AsyncNotifier: tools.NewAsyncNotifier(func(_, _, _, _ string) {})}
	sk := "bot/c100"

	// No per-session override → agent default
	if v := ag.SessionShowToolCalls(sk); v != "preview" {
		t.Errorf("agent default: SessionShowToolCalls = %q, want preview", v)
	}

	// Per-session override takes precedence
	ag.SetSessionShowToolCalls(sk, "full")
	if v := ag.SessionShowToolCalls(sk); v != "full" {
		t.Errorf("with override: SessionShowToolCalls = %q, want full", v)
	}

	// Clear override → back to agent default
	ag.SetSessionShowToolCalls(sk, "")
	if v := ag.SessionShowToolCalls(sk); v != "preview" {
		t.Errorf("after clear: SessionShowToolCalls = %q, want preview", v)
	}
}

func TestToolDisplayNote(t *testing.T) {
	// Proves that toolDisplayNote returns correct descriptions for each display mode.
	tests := []struct {
		mode     string
		contains string
	}{
		{"off", "hidden"},
		{"", "hidden"},
		{"preview", "preview"},
		{"full", "visible"},
	}
	for _, tt := range tests {
		note := toolDisplayNote(tt.mode)
		if !strings.Contains(note, tt.contains) {
			t.Errorf("toolDisplayNote(%q) = %q, want to contain %q", tt.mode, note, tt.contains)
		}
		// All notes start with [display]
		if !strings.Contains(note, "[display]") {
			t.Errorf("toolDisplayNote(%q) missing [display] prefix", tt.mode)
		}
	}
}

func TestSessionDisplayOverrides_ClearedByClearSessionState(t *testing.T) {
	// Proves that per-session display overrides (in-memory and their persisted
	// session_metadata rows) are wiped by ClearSessionState — the reset path —
	// so a reset session starts with agent-default display settings.
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	ag := &Agent{SessionIndex: idx, AsyncNotifier: tools.NewAsyncNotifier(func(_, _, _, _ string) {})}

	key := "bot/c100"

	ag.SetSessionShowToolCalls(key, "full")
	ag.SetSessionDisplayWidth(key, "80")

	ag.ClearSessionState(key)

	if v := ag.SessionShowToolCalls(key); v != "" {
		t.Errorf("show_tool_calls after clear = %q, want empty", v)
	}
	if v := ag.SessionDisplayWidth(key); v != "" {
		t.Errorf("display_width after clear = %q, want empty", v)
	}

	// Persisted rows are gone too.
	for _, k := range []string{"show_tool_calls", "display_width"} {
		if v, _ := idx.GetSessionMetadata(key, k); v != "" {
			t.Errorf("metadata row %q survived clear: %q", k, v)
		}
	}
}

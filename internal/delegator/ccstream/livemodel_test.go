package ccstream

import (
	"encoding/json"
	"testing"

	"foci/internal/delegator"
)

// TestLiveModel_TracksInitAndAssistant proves the #2252 fix at the source:
// LiveModel reports the model CC's own process named — from system/init
// before any turn completes, then from each top-level assistant message —
// with the same prefixedModel spelling TurnResult.Model carries. No
// OnResult is fed: the value must be available mid-first-turn.
func TestLiveModel_TracksInitAndAssistant(t *testing.T) {
	t.Parallel()

	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	applyHandler(b, &testHandler{})

	raw := json.RawMessage(`{
		"type": "system",
		"subtype": "init",
		"claude_code_version": "2.1.293",
		"cwd": "/tmp",
		"model": "claude-opus-5-5",
		"permissionMode": "default",
		"tools": ["Bash"],
		"session_id": "sess-live-001"
	}`)
	b.OnSystem("init", raw)
	if got := b.LiveModel(); got != "claude/claude-opus-5-5" {
		t.Errorf("LiveModel after init = %q, want %q (the resolved id, before any turn result)", got, "claude/claude-opus-5-5")
	}

	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Model:   "claude-sonnet-4-6",
			Content: []ContentBlock{{Type: "text", Text: "working"}},
			Usage:   TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
	})
	if got := b.LiveModel(); got != "claude/claude-sonnet-4-6" {
		t.Errorf("LiveModel after top-level assistant message = %q, want %q", got, "claude/claude-sonnet-4-6")
	}
}

// TestLiveModel_UnknownAndSyntheticStayEmpty pins LiveModel's empty arms: a
// backend whose process has not named a model yet reports "", and CC's
// synthetic sentinel (a no-API-call placeholder, never a launchable model)
// never leaks out as a reported model.
func TestLiveModel_UnknownAndSyntheticStayEmpty(t *testing.T) {
	t.Parallel()

	t.Run("no model learned yet", func(t *testing.T) {
		t.Parallel()
		b := &Backend{}
		applyHandler(b, &testHandler{})
		if got := b.LiveModel(); got != "" {
			t.Errorf("LiveModel on a fresh backend = %q, want \"\"", got)
		}
	})

	t.Run("synthetic assistant message keeps it empty", func(t *testing.T) {
		t.Parallel()
		b := &Backend{}
		applyHandler(b, &testHandler{})
		b.OnAssistant(&AssistantMessage{
			Message: BetaMessage{
				Model:   syntheticModel,
				Content: []ContentBlock{{Type: "text", Text: "No response requested."}},
			},
		})
		if got := b.LiveModel(); got != "" {
			t.Errorf("LiveModel after a synthetic message = %q, want \"\" (the sentinel is never reported)", got)
		}
	})
}

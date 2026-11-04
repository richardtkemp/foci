package ccstream

import (
	"encoding/json"
	"errors"
	"testing"

	"foci/internal/delegator"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// failingBackend is a Backend whose stdin write always fails.
func failingBackend() *Backend {
	return &Backend{
		writer:         NewWriter(nopWriteCloser{failingWriter{}}),
		pendingPerms:   make(map[string]*pendingPermission),
		pendingElicits: make(map[string]*pendingElicitation),
		outstanding:    delegator.NewOutstandingRegistry(),
		permPromptFn:   func(string, string, string, string, []delegator.PromptChoice) {},
	}
}

// TestRespondToQuestion_WriteFailureClearsOutstanding pins #2762: an answer that
// claimed the prompt and failed to write leaves no prompt anyone can answer, so
// it must also leave the registry. Otherwise WaitForPermission parks the session
// worker behind a prompt that no longer exists.
func TestRespondToQuestion_WriteFailureClearsOutstanding(t *testing.T) {
	t.Parallel()
	b := failingBackend()
	cleared := 0
	b.SetOnPromptsCleared(func() { cleared++ })
	b.handleUserQuestion(&PermissionRequest{
		Type:      "control_request",
		RequestID: "req-w",
		Request: PermissionRequestPayload{
			Subtype:   "can_use_tool",
			ToolName:  "AskUserQuestion",
			ToolUseID: "tu-w",
			Input:     json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"A","description":"a"}]}]}`),
		},
	})

	if err := b.RespondToQuestion("req-w", "typed"); err == nil {
		t.Fatal("RespondToQuestion returned nil on a failed write")
	}
	if id := b.HasPendingQuestion(); id != "" {
		t.Errorf("HasPendingQuestion = %q after the claim, want none", id)
	}
	if b.outstanding.Has("req-w") || cleared != 1 {
		t.Errorf("outstanding has req-w=%v, cleared=%d: the prompt must leave the registry and fire the all-clear", b.outstanding.Has("req-w"), cleared)
	}
}

// TestRespondToElicitation_WriteFailureClearsOutstanding is the elicitation twin.
func TestRespondToElicitation_WriteFailureClearsOutstanding(t *testing.T) {
	t.Parallel()
	b := failingBackend()
	cleared := 0
	b.SetOnPromptsCleared(func() { cleared++ })
	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	b.OnElicitationRequest(&ElicitationRequest{
		RequestID: "elic-w",
		Request: ElicitationRequestPayload{
			Subtype:         "elicitation",
			Mode:            "form",
			RequestedSchema: schema,
		},
	})

	if err := b.RespondToElicitation("elic-w", "Alice"); err == nil {
		t.Fatal("RespondToElicitation returned nil on a failed write")
	}
	if id := b.HasPendingElicitation(); id != "" {
		t.Errorf("HasPendingElicitation = %q after the claim, want none", id)
	}
	if b.outstanding.Has("elic-w") || cleared != 1 {
		t.Errorf("outstanding has elic-w=%v, cleared=%d: the prompt must leave the registry and fire the all-clear", b.outstanding.Has("elic-w"), cleared)
	}
}

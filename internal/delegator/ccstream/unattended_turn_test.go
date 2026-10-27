package ccstream

import (
	"bytes"
	"encoding/json"
	"testing"

	"foci/internal/delegator"
)

// unattendedCCBackend is newTestBackend with an unattended turn in flight
// (TurnEvents{Trigger, Unattended: true} — what buildTurnEvents installs for
// an unattended trigger), auto-approve rules, and a permPromptFn that records
// any prompt.
func unattendedCCBackend(buf *bytes.Buffer, rules []string, prompted *string) *Backend {
	b := newTestBackend(buf)
	b.autoApproveRules = parseAutoApproveRules(rules)
	b.turnEvents = &delegator.TurnEvents{Trigger: "reflection", Unattended: true}
	b.permPromptFn = func(reqID, _, _, _ string, _ []delegator.PromptChoice) { *prompted = reqID }
	return b
}

// TestUnattendedTurn_ToolRequestFromRules: a can_use_tool during an unattended
// turn on an ordinary (not skip-permissions) session is decided by foci's
// rules — allowed when a rule permits it, denied with UnattendedDenyMessage
// when none does (including a command the engine refuses to match, such as a
// live substitution argument). Never prompted, nothing left
// pending or outstanding: CC waits for the answer with no timeout.
func TestUnattendedTurn_ToolRequestFromRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, command, want string
	}{
		{"rule permits", "ls -la", "allow"},
		{"no rule", "rm -rf build", "deny"},
		{"rule refuses substitution", `ls "$(pwd)"`, "deny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			prompted := ""
			b := unattendedCCBackend(&buf, []string{"Bash:ls *"}, &prompted)
			input, _ := json.Marshal(map[string]string{"command": tc.command})
			b.handleToolRequest(&PermissionRequest{
				RequestID: "req-u",
				Request:   PermissionRequestPayload{ToolName: "Bash", ToolUseID: "toolu_U", Input: input},
			})
			if prompted != "" {
				t.Errorf("prompted (%q) during an unattended turn; want no prompt", prompted)
			}
			if b.PendingPermissions() != 0 || b.outstanding.Has("req-u") {
				t.Error("ask left pending/outstanding; nothing will ever resolve it")
			}
			inner := parseControlResponse(t, buf.String())["response"].(map[string]any)
			if inner["behavior"] != tc.want {
				t.Errorf("behavior = %v, want %s", inner["behavior"], tc.want)
			}
			if tc.want == "deny" && inner["message"] != delegator.UnattendedDenyMessage {
				t.Errorf("deny message = %v, want UnattendedDenyMessage", inner["message"])
			}
		})
	}
}

// TestUnattendedTurn_AskUserQuestionDeclined: AskUserQuestion during an
// unattended turn has nobody to answer it — denied with the question message,
// never surfaced, even when a rule would match the tool name.
func TestUnattendedTurn_AskUserQuestionDeclined(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	prompted := ""
	b := unattendedCCBackend(&buf, []string{"AskUserQuestion"}, &prompted)
	b.handleToolRequest(&PermissionRequest{
		RequestID: "req-q",
		Request: PermissionRequestPayload{
			ToolName: "AskUserQuestion", ToolUseID: "toolu_Q",
			Input: json.RawMessage(`{"questions":[{"question":"which?","options":[{"label":"a"}]}]}`),
		},
	})
	if prompted != "" {
		t.Errorf("question surfaced (%q) during an unattended turn", prompted)
	}
	inner := parseControlResponse(t, buf.String())["response"].(map[string]any)
	if inner["behavior"] != "deny" || inner["message"] != delegator.UnattendedQuestionMessage {
		t.Errorf("response = %v, want deny with UnattendedQuestionMessage", inner)
	}
}

// TestUnattendedTurn_ElicitationDeclined: an MCP elicitation is a question —
// declined at once during an unattended turn, never stored or prompted.
func TestUnattendedTurn_ElicitationDeclined(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	prompted := ""
	b := unattendedCCBackend(&buf, nil, &prompted)
	b.OnElicitationRequest(&ElicitationRequest{
		RequestID: "elic-u",
		Request: ElicitationRequestPayload{
			Subtype: "elicitation", McpServerName: "greeter", Message: "who are you?", Mode: "form",
			RequestedSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
		},
	})
	if prompted != "" {
		t.Errorf("elicitation surfaced (%q) during an unattended turn", prompted)
	}
	if b.outstanding.Has("elic-u") {
		t.Error("elicitation registered outstanding")
	}
	inner := parseControlResponse(t, buf.String())["response"].(map[string]any)
	if inner["action"] != "decline" {
		t.Errorf("action = %v, want decline", inner["action"])
	}
}

// TestUnattendedTurn_UserTurnStillPrompts is the other arm: the same backend
// with an ordinary turn prompts for an unruled call and answers nothing.
func TestUnattendedTurn_UserTurnStillPrompts(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	prompted := ""
	b := unattendedCCBackend(&buf, []string{"Bash:ls *"}, &prompted)
	b.turnEvents = &delegator.TurnEvents{Trigger: "telegram"}
	b.handleToolRequest(&PermissionRequest{
		RequestID: "req-user",
		Request:   PermissionRequestPayload{ToolName: "Bash", ToolUseID: "toolu_N", Input: json.RawMessage(`{"command":"rm -rf build"}`)},
	})
	if prompted != "req-user" {
		t.Errorf("prompted = %q, want req-user", prompted)
	}
	if buf.Len() != 0 {
		t.Errorf("answered behind the user's back: %s", buf.String())
	}
}

package ccstream

import (
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// toolIOHook feeds one PostToolUse hook response for toolUseID; agentID set
// makes it a subagent's.
func toolIOHook(t *testing.T, b *Backend, toolUseID, name, response, agentID string) {
	t.Helper()
	stdout, err := json.Marshal(hookScriptOutput{
		HookEvent: "PostToolUse", InstallID: b.hookInstallID, ToolUseID: toolUseID,
		ToolName: name, ToolResponse: response, AgentID: agentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(hookResponseEnvelope{HookEvent: "PostToolUse", Stdout: string(stdout), Outcome: "success"})
	if err != nil {
		t.Fatal(err)
	}
	b.handleHookResponse(env)
}

// TestToolIO_InputsAndOutputsReachSessionEvents proves tool_io (#3187 Q127)
// for claude-code, Partial: the main thread's tool input (OnToolStart) and
// result (OnToolEnd, from the PostToolUse hook) arrive whole, and so does a
// subagent's tool input (OnSubagentTool); a subagent's tool result does not
// arrive (its hook carries an agent_id and is dropped), which is the part
// the Spec's Partial reason names.
func TestToolIO_InputsAndOutputsReachSessionEvents(t *testing.T) {
	long := strings.Repeat("a long line the own-material index must see whole. ", 200)
	input := `{"command":"cat >> memory/x.md <<'EOF'\n` + long + `\nEOF"}`
	subInput := `{"file_path":"memory/y.md","content":"` + long + `"}`

	var starts, ends, subTools []string
	b := &Backend{hookInstallID: "install-io"}
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnToolStart:    func(_, _, in string) { starts = append(starts, in) },
		OnToolEnd:      func(_, _, out string, _ bool) { ends = append(ends, out) },
		OnSubagentTool: func(_, _, in string) { subTools = append(subTools, in) },
	})
	b.beginTurn(nil)

	b.OnAssistant(&AssistantMessage{Message: BetaMessage{Content: []ContentBlock{
		{Type: "tool_use", ID: "toolu_main", Name: "Bash", Input: json.RawMessage(input)},
	}}})
	toolIOHook(t, b, "toolu_main", "Bash", long, "")

	spawnTopLevel(t, b, "toolu_parent", "task_parent", true)
	subagentMsg(b, "toolu_parent", ContentBlock{Type: "tool_use", ID: "toolu_sub", Name: "Write", Input: json.RawMessage(subInput)})
	toolIOHook(t, b, "toolu_sub", "Write", "the subagent's result", "agent_sub")

	if !containsExactly(starts, input) {
		t.Errorf("main tool input not reported whole: %d starts", len(starts))
	}
	if !containsExactly(ends, long) {
		t.Errorf("main tool result not reported whole: %q", ends)
	}
	if !containsExactly(subTools, subInput) {
		t.Errorf("subagent tool input not reported whole: %d calls", len(subTools))
	}
	if containsExactly(ends, "the subagent's result") {
		t.Error("a subagent's tool result is reported: tool_io's Partial reason is stale, declare Yes")
	}
}

func containsExactly(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

package ccstream

import (
	"encoding/json"
	"testing"

	"foci/internal/delegator"
)

// A subagent's tool calls reach OnSubagentTool under its group key, as its
// activity for the app's Stop-view summary (#2960), and are never delivered as
// subagent text. The main thread's own tool calls do not reach it.
func TestSubagentToolUse_ReportedAsActivity(t *testing.T) {
	type call struct{ group, name, input string }
	var tools []call
	var texts []string
	b := &Backend{}
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnSubagentText: func(_, text string, _ int) { texts = append(texts, text) },
		OnSubagentTool: func(group, name, input string) { tools = append(tools, call{group, name, input}) },
	})
	b.beginTurn(nil)

	spawnTopLevel(t, b, "toolu_parent", "task_parent", true)
	subagentMsg(b, "toolu_parent",
		ContentBlock{Type: "text", Text: "Looking at the parser"},
		ContentBlock{Type: "tool_use", ID: "toolu_read", Name: "Read", Input: json.RawMessage(`{"file_path":"/x/parser.go"}`)},
	)

	if len(tools) != 1 {
		t.Fatalf("tool calls = %+v, want one (the subagent's Read only, not the main thread's Agent)", tools)
	}
	if want := (call{"toolu_parent", "Read", `{"file_path":"/x/parser.go"}`}); tools[0] != want {
		t.Errorf("tool call = %+v, want %+v", tools[0], want)
	}
	if len(texts) != 1 || texts[0] != "Looking at the parser" {
		t.Errorf("texts = %q, want only the text block", texts)
	}
}

// A grandchild's tool call is the activity of the depth-1 subagent the app
// lists, as its text is (#1554).
func TestSubagentToolUse_NestedAttributedToAncestor(t *testing.T) {
	var groups []string
	b := &Backend{}
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnSubagentTool: func(group, _, _ string) { groups = append(groups, group) },
	})
	b.beginTurn(nil)

	spawnTopLevel(t, b, "toolu_parent", "task_parent", true)
	subagentMsg(b, "toolu_parent", nestedAgentBlock("toolu_nested"))
	b.OnSystem("task_started", nestedTaskEvent(t, "task_started", "task_nested", "toolu_nested", ""))
	subagentMsg(b, "toolu_nested", ContentBlock{Type: "tool_use", ID: "toolu_g", Name: "Grep", Input: json.RawMessage(`{}`)})

	want := []string{"toolu_parent", "toolu_parent"} // the nested Agent call, then the grandchild's Grep
	if len(groups) != len(want) || groups[0] != want[0] || groups[1] != want[1] {
		t.Errorf("groups = %v, want %v", groups, want)
	}
}

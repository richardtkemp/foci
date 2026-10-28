package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
)

func testOpening() *session.BranchOpening {
	return &session.BranchOpening{
		CallMarker: "depth_fanout",
		ToolResult: `Fanned out into 2 leaves. This conversation is the leaf "A".`,
		AgentText:  "Which colour do you want?",
		Fallback:   "### Your opening message\nWhich colour do you want?",
	}
}

// An API branch taken inside a turn's depth_fanout call (#2617) holds the
// running turn (not on disk yet) through the fan-out call, a result for every
// call of that message (the fan-out's own text for it), and the question as
// assistant text; the orientation carries no fallback.
func TestForkSessionAPIOpening(t *testing.T) {
	store := session.NewStore(t.TempDir())
	const parent = "agent/c1"
	history := []provider.Message{
		{Role: "user", Content: provider.TextContent("hi")},
		{Role: "assistant", Content: provider.TextContent("hello")},
	}
	if err := store.For(parent).AppendAll(parent, history); err != nil {
		t.Fatal(err)
	}
	unsaved := []provider.Message{
		{Role: "user", Content: provider.TextContent("plan the shed")},
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "text", Text: "Splitting it up."},
			{Type: "tool_use", ID: "t_other", Name: "read_file", Input: json.RawMessage(`{}`)},
			{Type: "tool_use", ID: "t_fan", Name: "depth_fanout", Input: json.RawMessage(`{"leaves":[]}`)},
		}},
	}
	a := &Agent{Sessions: store}
	ctx := session.WithUnsavedTurn(context.Background(), unsaved)
	bk, ok, err := a.ForkSession(ctx, parent, session.BranchOptions{BranchType: "depth", OrientationTemplate: "LEAF", Opening: testOpening()})
	if err != nil || !ok {
		t.Fatalf("fork: ok=%v err=%v", ok, err)
	}
	got, err := store.LoadFull(bk)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("branch history has %d messages, want 2 parent + 2 unsaved + result + question: %+v", len(got), got)
	}
	if provider.TextOf(got[2].Content) != "plan the shed" {
		t.Errorf("the running turn's user message is missing: %+v", got[2])
	}
	res := got[4]
	if res.Role != "user" || len(res.Content) != 2 || res.Content[0].ToolUseID != "t_other" || res.Content[1].ToolUseID != "t_fan" {
		t.Fatalf("result message = %+v, want results for t_other and t_fan", res)
	}
	if !strings.Contains(res.Content[1].Content, "leaf \"A\"") || !strings.Contains(res.Content[0].Content, "belongs to the parent") {
		t.Errorf("results = %q / %q", res.Content[0].Content, res.Content[1].Content)
	}
	if q := got[5]; q.Role != "assistant" || provider.TextOf(q.Content) != "Which colour do you want?" {
		t.Errorf("last message = %+v, want the question as assistant text", q)
	}
	if o := store.ConsumeOrientation(bk, nil); o != "LEAF" {
		t.Errorf("orientation = %q, want the template alone (the opening is in the history)", o)
	}
}

// With no running turn to carry (an approved deep fan-out runs after its turn
// ended), the API branch gets no synthetic messages and the question rides on
// the first user message through the orientation instead.
func TestForkSessionAPIOpeningFallback(t *testing.T) {
	store := session.NewStore(t.TempDir())
	a := &Agent{Sessions: store}
	bk, ok, err := a.ForkSession(context.Background(), "agent/c1", session.BranchOptions{BranchType: "depth", OrientationTemplate: "LEAF", Opening: testOpening()})
	if err != nil || !ok {
		t.Fatalf("fork: ok=%v err=%v", ok, err)
	}
	if got, _ := store.LoadFull(bk); len(got) != 0 {
		t.Errorf("branch history = %+v, want none", got)
	}
	if o := store.ConsumeOrientation(bk, nil); o != "LEAF\n\n### Your opening message\nWhich colour do you want?" {
		t.Errorf("orientation = %q, want the template plus the fallback", o)
	}
}

func TestAPIOpeningNeedsTheMarkedCall(t *testing.T) {
	unsaved := []provider.Message{{Role: "assistant", Content: []provider.ContentBlock{{Type: "tool_use", ID: "x", Name: "read_file"}}}}
	if got := apiOpening(unsaved, testOpening()); got != nil {
		t.Errorf("opening without a depth_fanout call = %+v, want nil", got)
	}
}

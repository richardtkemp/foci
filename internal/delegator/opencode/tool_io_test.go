package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolIO_InputsAndOutputsReachSessionEvents proves tool_io (#3187 Q127)
// for opencode, Partial: a tool's input (OnToolStart, when it starts
// running) and its output (OnToolEnd, when it completes) arrive whole; a
// subagent's (child session's) tool reaches neither, which is the part the
// Spec's Partial reason names.
func TestToolIO_InputsAndOutputsReachSessionEvents(t *testing.T) {
	b := newHandlerTestBackend(t)
	c := b.captures()
	long := strings.Repeat("a long line the own-material index must see whole. ", 200)
	input, err := json.Marshal(map[string]string{"filePath": "memory/x.md", "content": long})
	if err != nil {
		t.Fatal(err)
	}

	b.onMessagePartUpdated(Part{Type: PartTool, ID: "p1", CallID: "call-1", Tool: "write",
		State: &ToolState{Status: ToolStateRunning, Input: input}}, "")
	b.onMessagePartUpdated(Part{Type: PartTool, ID: "p1", CallID: "call-1", Tool: "write",
		State: &ToolState{Status: ToolStateCompleted, Input: input, Output: long}}, "")

	if len(*c.toolStarts) != 1 || (*c.toolStarts)[0].detail != string(input) {
		t.Errorf("tool input not reported whole: %d starts", len(*c.toolStarts))
	}
	if len(*c.toolEnds) != 1 || (*c.toolEnds)[0].detail != long {
		t.Errorf("tool output not reported whole: %d ends", len(*c.toolEnds))
	}

	// A child session's tool part.
	props, err := json.Marshal(eventMessagePartUpdated{Part: Part{Type: PartTool, ID: "p2", CallID: "call-child", Tool: "write",
		State: &ToolState{Status: ToolStateCompleted, Input: input, Output: "the subagent's result"}}})
	if err != nil {
		t.Fatal(err)
	}
	b.handleEvent(rawEvent{Type: EventMessagePartUpdated, Properties: props, childCallID: "call-task"})
	if len(*c.toolStarts) != 1 || len(*c.toolEnds) != 1 {
		t.Error("a subagent's tool is reported: tool_io's Partial reason is stale")
	}
}

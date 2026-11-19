package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// TestToolIO_InputsAndOutputsReachSessionEvents proves tool_io (#3187 Q127)
// for codex, Partial: a command's input (OnToolStart) and its output
// (OnToolEnd) arrive whole; an MCP tool's arguments are cut and its result
// is not reported, which is part of what the Spec's Partial reason names.
func TestToolIO_InputsAndOutputsReachSessionEvents(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	starts, ends := map[string]string{}, map[string]string{}
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnToolStart: func(id, _, in string) { starts[id] = in },
		OnToolEnd:   func(id, _, out string, _ bool) { ends[id] = out },
	})
	long := strings.Repeat("a long line the own-material index must see whole. ", 200)
	cmd := "printf '%s\\n' '" + long + "' >> memory/x.md"
	q := func(s string) string {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	b.dispatch([]byte(`{"method":"item/started","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"c1","status":"inProgress","command":` + q(cmd) + `}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"commandExecution","id":"c1","status":"completed","command":` + q(cmd) + `,"aggregatedOutput":` + q(long) + `}}}`))
	if starts["c1"] != cmd {
		t.Errorf("command input not reported whole (%d of %d bytes)", len(starts["c1"]), len(cmd))
	}
	if ends["c1"] != long {
		t.Errorf("command output not reported whole (%d of %d bytes)", len(ends["c1"]), len(long))
	}

	args := `{"text":` + q(long) + `}`
	b.dispatch([]byte(`{"method":"item/started","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"mcpToolCall","id":"m1","status":"inProgress","server":"s","tool":"t","arguments":` + args + `}}}`))
	b.dispatch([]byte(`{"method":"item/completed","params":{"threadId":"th_1","turnId":"tu_1","item":{"type":"mcpToolCall","id":"m1","status":"completed","server":"s","tool":"t","arguments":` + args + `}}}`))
	if starts["m1"] == args || ends["m1"] != "" {
		t.Error("an MCP tool's input and output are reported whole: tool_io's Partial reason is stale")
	}
}

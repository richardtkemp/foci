package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"foci/internal/memsearch"
	"foci/internal/provider"
	"foci/internal/tools"
)

// ownLineIn reports whether the own-material index recorded line for
// sessionKey's family in its current window.
func ownLineIn(t *testing.T, x *memsearch.Index, sessionKey, line string) bool {
	t.Helper()
	x.Flush()
	n, err := x.Matches(x.Window(sessionKey, 3), []int64{memsearch.HashLine(line)})
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func openOwnIndex(t *testing.T) *memsearch.Index {
	t.Helper()
	x, err := memsearch.OpenIndex(filepath.Join(t.TempDir(), "own.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.Close() })
	return x
}

const (
	ownWritten = "- The reflection branch wrote this line into a memory file today."
	ownRead    = "- And this line came back in a tool result the session read."
)

// Delegated backends (claude-code, codex, opencode) feed the index through
// the session events every one of them emits: OnToolStart's input and
// OnToolEnd's output, under the key the events were bound to (a branch key
// counts for its root's family, #3185).
func TestOwnMemory_DelegatedToolEventsFeedIndex(t *testing.T) {
	t.Parallel()
	x := openOwnIndex(t)
	a := &Agent{OwnMemory: x}
	be := &mockBackendDT{}
	a.AttachDelivery(be, "test/c1/b1795000000")
	se := be.sessionEvents
	se.OnToolStart("t1", "Bash", `{"command":"cat >> memory/x.md <<'EOF'\n`+ownWritten+`\nEOF"}`)
	se.OnToolEnd("t1", "Bash", ownRead, false)
	if !ownLineIn(t, x, "test/c1", ownWritten) || !ownLineIn(t, x, "test/c1", ownRead) {
		t.Fatal("tool input or output not recorded for the root's family")
	}
	if ownLineIn(t, x, "test/c2", ownWritten) {
		t.Fatal("recorded for another family")
	}
}

// The API transport feeds the index from its own tool loop: the input and
// the result the model sees.
func TestOwnMemory_APIToolLoopFeedsIndex(t *testing.T) {
	t.Parallel()
	x := openOwnIndex(t)
	registry := tools.NewRegistry()
	registry.Register(&tools.Tool{
		Name: "write", Description: "w", Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult(ownRead), nil
		},
	})
	a := &Agent{Tools: registry, OwnMemory: x}
	in, _ := json.Marshal(map[string]string{"path": "memory/x.md", "content": ownWritten + "\n"})
	blocks := []provider.ContentBlock{{Type: "tool_use", ID: "tu1", Name: "write", Input: in}}
	if _, err := a.executeToolCalls(context.Background(), &TurnDetail{SessionKey: "test/c1"}, nil, "test/c1", "", blocks, nil); err != nil {
		t.Fatal(err)
	}
	if !ownLineIn(t, x, "test/c1", ownWritten) || !ownLineIn(t, x, "test/c1", ownRead) {
		t.Fatal("API tool input or result not recorded")
	}
}

// A compaction advances the family's window and prunes lines older than
// [jit_memory] dedupe_compactions.
func TestOwnMemory_CompactionAdvancesWindow(t *testing.T) {
	t.Parallel()
	x := openOwnIndex(t)
	a := &Agent{OwnMemory: x}
	x.Observe("test/c1", ownWritten)
	x.Flush()
	for i := 0; i < 3; i++ {
		a.noteJITCompaction("test/c1") // no live config: keep = 0, nothing pruned
	}
	if w := x.Window("test/c1", 3); w.Gen != 3 || w.MinGen != 1 {
		t.Fatalf("window = %+v", w)
	}
	if ownLineIn(t, x, "test/c1", ownWritten) {
		t.Fatal("a line from before the window is still own")
	}
}

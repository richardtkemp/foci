package ccstream

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// msgWithStop is one transcript record carrying usage and a stop_reason.
func msgWithStop(id, stop string, out, cw int) string {
	return fmt.Sprintf(
		`{"type":"assistant","isSidechain":true,"timestamp":%q,`+
			`"message":{"id":%q,"model":"claude-opus-5","stop_reason":%q,`+
			`"usage":{"output_tokens":%d,"cache_creation_input_tokens":%d}}}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano), id, stop, out, cw)
}

// TestSubagentTail_WaitsForTheTerminalRecordWrittenByAnotherProcess is the
// regression test for #1938: the tail lost exactly its LAST completed message.
//
// finalize() fires on CC's task_notification:completed, which arrives on CC's
// STDOUT STREAM — a different channel from CC's append to the transcript FILE.
// Nothing orders the two, so the old single post-stop drain could run before the
// final record was visible. Measured live twice, both off by exactly one:
// 12 records read as 11, 15 read as 14.
//
// THE SEPARATE PROCESS IS THE POINT. TestSubagentTail_FinalizeDrainsRemainder
// looks like it covers this path and cannot: its append is a synchronous
// in-process OpenFile/WriteString/Close that completes strictly BEFORE finalize
// is called, so Go's sequential execution guarantees visibility and it passes
// whether or not the race exists. Here the terminal record is written by a real
// child process AFTER finalize has already been entered, which is the only way
// to reproduce what CC actually does.
func TestSubagentTail_WaitsForTheTerminalRecordWrittenByAnotherProcess(t *testing.T) {
	withFastTail(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-term.jsonl")

	var mu sync.Mutex
	var terminalSeen bool
	var totalWrites int
	mgr := newSubagentTailManager(nil, func(_, _, id string, _ time.Time, complete bool, u TokenUsage) {
		mu.Lock()
		defer mu.Unlock()
		if complete {
			totalWrites += u.CacheCreationInputTokens
			if id == "m-final" {
				terminalSeen = true
			}
		}
	}, nil)

	// Message 1: stop_reason "tool_use" — the run continues after this.
	if err := os.WriteFile(path, []byte(msgWithStop("m-1", "tool_use", 95, 10630)), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.maybeStart("tool-term", path)
	waitFor(t, func() bool { return fileOpened(mgr, "tool-term") })

	// A REAL separate process appends the terminal record shortly AFTER we ask
	// the tail to finish — exactly CC's ordering.
	final := msgWithStop("m-final", "end_turn", 3, 1029)
	// Staged in a file rather than interpolated into the shell: Go's %q escaping
	// does not survive sh, and a mangled record fails the test for the wrong
	// reason (it reads as "the tail missed it" when the JSON never parsed).
	stage := filepath.Join(dir, "final.jsonl")
	if err := os.WriteFile(stage, []byte(final), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 0.15; cat "+stage+" >> "+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()

	mgr.finalize("tool-term")

	mu.Lock()
	defer mu.Unlock()
	if !terminalSeen {
		t.Errorf("the terminal end_turn record was never read — the tail closed before " +
			"the writing process flushed it (#1938)")
	}
	if totalWrites != 10630+1029 {
		t.Errorf("cache_write collected = %d, want %d (missing the terminal record's share)",
			totalWrites, 10630+1029)
	}
}

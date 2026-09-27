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
	// withFastTail's 1s window is shorter than spawning sh + sleep 0.15 takes on
	// a saturated machine; it expired first and the test failed under load. The
	// tail returns the moment the record lands, so a wide window costs nothing.
	subagentTailSettle = 30 * time.Second
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
	mgr.maybeStart("tool-term", path, time.Time{})
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

// userRecord is a transcript "user" record: a tool_result, a Stop hook's
// feedback, or a SendMessage prompt reactivating the subagent. Any of them means
// the conversation continued past whatever the assistant said before it.
const userRecord = `{"type":"user","isSidechain":true,"message":{"role":"user","content":"continue"}}` + "\n"

// TestSubagentTail_AnEarlierEndTurnDoesNotEndTheTail is the second half of
// #1938. The first fix ended the post-stop drain on "a terminal record has been
// read", but that flag was STICKY: set by ANY end_turn read during ordinary
// pre-stop tailing and never cleared. A run that wrote an end_turn and then
// carried on therefore returned on its first post-stop drain, which is the
// original race again. Two live shapes, both reproduced here:
//
//   - stop-hook handback: a text reply (end_turn), the Stop hook's feedback, a
//     SubagentHandback tool_use, its result, then the final text. Live:
//     agent-aed9b9fd… has 49 records with end_turn at 42-43 and 49; the tail
//     logged "closed … lines=48", and the file's mtime (22:46:40.762Z)
//     postdates CC's task_notification (22:46:40.542Z) by 220ms.
//   - reactivation: run 1's end_turn, then the SendMessage prompt that starts
//     run 2. A reactivation tail re-reads run 1 from byte 0, so it was ALWAYS
//     armed before run 2 wrote anything.
//
// The end of a run is not "an end_turn exists"; it is "the transcript currently
// ENDS at one": the last conversational record is a terminal assistant message.
//
// Sequencing: the history is drained BEFORE finalize (lines counted, not
// guessed), and the final record is appended only after finalize has been
// entered, and after it has RETURNED if the tail gives up early, so the pre-fix
// code fails deterministically rather than by losing a race. subagentTailSettle
// is set far above any real delay: the fixed tail returns the moment the record
// lands, so the wide window costs nothing and cannot flake under load.
func TestSubagentTail_AnEarlierEndTurnDoesNotEndTheTail(t *testing.T) {
	cases := []struct {
		name    string
		history []string
	}{
		{"stop-hook handback", []string{
			msgWithStop("m-reply", "end_turn", 586, 500),
			userRecord, // Stop hook: "call SubagentHandback"
			msgWithStop("m-handback", "tool_use", 470, 400),
			userRecord, // SubagentHandback's tool_result
		}},
		{"reactivation re-read from byte 0", []string{
			msgWithStop("m-run1", "end_turn", 50, 500),
			userRecord, // SendMessage prompt: run 2 begins
			msgWithStop("m-run2-tool", "tool_use", 30, 400),
			userRecord,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFastTail(t)
			subagentTailSettle = 30 * time.Second
			path := filepath.Join(t.TempDir(), "agent-sticky.jsonl")

			var mu sync.Mutex
			finalSeen := false
			mgr := newSubagentTailManager(nil, func(_, _, id string, _ time.Time, complete bool, _ TokenUsage) {
				mu.Lock()
				defer mu.Unlock()
				if complete && id == "m-final" {
					finalSeen = true
				}
			}, nil)

			var hist []byte
			for _, l := range tc.history {
				hist = append(hist, l...)
			}
			if err := os.WriteFile(path, hist, 0o644); err != nil {
				t.Fatal(err)
			}
			mgr.maybeStart("tool-sticky", path, time.Time{})
			waitForLines(t, mgr, "tool-sticky", int64(len(tc.history)))

			done := make(chan struct{})
			go func() { mgr.finalize("tool-sticky"); close(done) }()
			select {
			case <-done: // gave up at stop: the record below is already too late
			case <-time.After(200 * time.Millisecond):
			}
			appendLine(t, path, msgWithStop("m-final", "end_turn", 3, 1029))

			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("finalize did not return after the terminal record was written")
			}
			mu.Lock()
			defer mu.Unlock()
			if !finalSeen {
				t.Error("the run's final end_turn was never read: an EARLIER end_turn ended the tail at stop (#1938)")
			}
		})
	}
}

// TestSubagentTail_EndsPromptlyWhenTheFileIsAlreadyAtRest pins the other side:
// a transcript that already ends at its terminal record when stop arrives must
// not wait out the settle window. The window is 30s here, so a tail that waited
// for it would fail the 10s bound. The trailing attachment is not a
// conversational record, so it leaves the transcript at rest (one live
// transcript in ~200 ends that way).
func TestSubagentTail_EndsPromptlyWhenTheFileIsAlreadyAtRest(t *testing.T) {
	withFastTail(t)
	subagentTailSettle = 30 * time.Second
	path := filepath.Join(t.TempDir(), "agent-rest.jsonl")
	content := msgWithStop("m-1", "end_turn", 5, 5) + userRecord +
		msgWithStop("m-2", "tool_use", 5, 5) + userRecord +
		msgWithStop("m-3", "end_turn", 5, 5) +
		`{"type":"attachment","isSidechain":true}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := newSubagentTailManager(nil, nil, nil)
	mgr.maybeStart("tool-rest", path, time.Time{})
	waitForLines(t, mgr, "tool-rest", 6)

	done := make(chan struct{})
	go func() { mgr.finalize("tool-rest"); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("finalize waited on a transcript that already ended at its terminal record")
	}
}

// waitForLines blocks until the tail for key has read n lines. The bound is
// generous because it only matters when the test is already failing.
func waitForLines(t *testing.T, m *subagentTailManager, key string, n int64) {
	t.Helper()
	m.mu.Lock()
	tail := m.tails[key]
	m.mu.Unlock()
	if tail == nil {
		t.Fatalf("no tail running for %s", key)
	}
	deadline := time.Now().Add(20 * time.Second)
	for tail.lines.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("tail read %d lines, want %d", tail.lines.Load(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

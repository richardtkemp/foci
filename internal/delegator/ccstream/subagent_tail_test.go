package ccstream

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recorder collects delivered subagent text blocks under a mutex.
type tailRecorder struct {
	mu   sync.Mutex
	got  []string
	keys []string
}

func (r *tailRecorder) deliver(groupKey, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, groupKey)
	r.got = append(r.got, text)
}

func (r *tailRecorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// withFastTail shortens the poll interval for the duration of a test.
func withFastTail(t *testing.T) {
	t.Helper()
	origPoll, origWait := subagentTailPoll, subagentTailFileWait
	subagentTailPoll = 2 * time.Millisecond
	subagentTailFileWait = 2 * time.Second
	t.Cleanup(func() {
		subagentTailPoll = origPoll
		subagentTailFileWait = origWait
	})
}

func assistantLine(text string) string {
	return `{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within deadline")
}

// TestSubagentTail_ForegroundStreamsAppendedText verifies the tailer forwards
// each assistant text block as it is appended to the transcript, in order.
func TestSubagentTail_ForegroundStreamsAppendedText(t *testing.T) {
	withFastTail(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-x.jsonl")

	rec := &tailRecorder{}
	mgr := newSubagentTailManager(rec.deliver, nil)

	// Foreground start recorded, then task_started fires maybeStart.
	mgr.expectForeground("tool-1")
	// Create the file with the first line before the tail starts.
	if err := os.WriteFile(path, []byte(assistantLine("MSG-ONE")), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.maybeStart("tool-1", path)

	waitFor(t, func() bool { return len(rec.texts()) == 1 })

	// Append a tool_use line (ignored) and a second text message.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"tool_use"}]}}` + "\n")
	f.WriteString(assistantLine("MSG-TWO"))
	f.Close()

	waitFor(t, func() bool { return len(rec.texts()) == 2 })
	mgr.finalize("tool-1")

	got := rec.texts()
	want := []string{"MSG-ONE", "MSG-TWO"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("block %d: got %q want %q (all=%v)", i, got[i], want[i], got)
		}
	}
	for _, k := range rec.keys {
		if k != "tool-1" {
			t.Fatalf("group key: got %q want tool-1", k)
		}
	}
}

// TestSubagentTail_FinalizeDrainsRemainder verifies finalize catches text
// written just before completion (a line appended after the last poll).
func TestSubagentTail_FinalizeDrainsRemainder(t *testing.T) {
	withFastTail(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-y.jsonl")

	rec := &tailRecorder{}
	mgr := newSubagentTailManager(rec.deliver, nil)
	mgr.expectForeground("tool-2")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.maybeStart("tool-2", path)
	// Give the tailer time to open the file, then append + immediately finalize.
	waitFor(t, func() bool { return fileOpened(mgr, "tool-2") })
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(assistantLine("FINAL-MSG"))
	f.Close()
	mgr.finalize("tool-2") // must drain FINAL-MSG before returning

	got := rec.texts()
	if len(got) != 1 || got[0] != "FINAL-MSG" {
		t.Fatalf("finalize did not drain remainder: got %v", got)
	}
}

// fileOpened reports whether the tail goroutine for key is running (a proxy:
// the tail entry exists). Used only to sequence the finalize-drain test.
func fileOpened(m *subagentTailManager, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.tails[key]
	return ok
}

// TestSubagentTail_BackgroundNotTailed verifies maybeStart is a no-op when no
// foreground start was recorded (background subagents already stream text).
func TestSubagentTail_BackgroundNotTailed(t *testing.T) {
	withFastTail(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-z.jsonl")
	if err := os.WriteFile(path, []byte(assistantLine("SHOULD-NOT-APPEAR")), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &tailRecorder{}
	mgr := newSubagentTailManager(rec.deliver, nil)
	// No expectForeground → background path.
	mgr.maybeStart("tool-bg", path)

	time.Sleep(30 * time.Millisecond)
	if got := rec.texts(); len(got) != 0 {
		t.Fatalf("background subagent should not be tailed, got %v", got)
	}
	// No tail registered.
	if fileOpened(mgr, "tool-bg") {
		t.Fatal("a tail was started for a background subagent")
	}
}

// TestSubagentTail_DeliverLineFilters checks that only assistant text blocks are
// forwarded — input prompt (user), tool_use, tool_result and empty text skipped.
func TestSubagentTail_DeliverLineFilters(t *testing.T) {
	rec := &tailRecorder{}
	mgr := newSubagentTailManager(rec.deliver, nil)
	lines := []string{
		`{"type":"user","isSidechain":true,"message":{"content":[{"type":"text","text":"PROMPT"}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"tool_use"}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":""}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"REAL"}]}}`,
		`not json`,
		``,
	}
	for _, l := range lines {
		mgr.deliverLine("g", []byte(l))
	}
	got := rec.texts()
	if len(got) != 1 || got[0] != "REAL" {
		t.Fatalf("filter failed: got %v want [REAL]", got)
	}
}

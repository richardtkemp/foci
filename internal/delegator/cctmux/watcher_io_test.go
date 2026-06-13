package cctmux

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
)

// endTurnLine is a minimal assistant JSONL entry that completes a turn.
func endTurnLine(text string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}],"stop_reason":"end_turn"}}` + "\n"
}

// writeSessionFile creates a JSONL file with the given content and returns its path.
func writeSessionFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	return path
}

// appendLine appends a line to an existing file (simulating CC writing a new entry).
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// TestNewSessionWatcher_OffsetModes proves the three startOffset semantics:
// -1 tails from the current end of file, 0 reads from the beginning, and a
// positive value resumes from that recorded position.
func TestNewSessionWatcher_OffsetModes(t *testing.T) {
	content := "0123456789\n"
	cases := []struct {
		name        string
		startOffset int64
		wantOffset  int64
	}{
		{"tail from end", -1, int64(len(content))},
		{"from beginning", 0, 0},
		{"explicit resume offset", 5, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSessionFile(t, content)
			w, err := newSessionWatcher(path, tc.startOffset)
			if err != nil {
				t.Fatalf("newSessionWatcher: %v", err)
			}
			defer w.close()
			if w.offset != tc.wantOffset {
				t.Errorf("offset = %d, want %d", w.offset, tc.wantOffset)
			}
			if w.path != path {
				t.Errorf("path = %q, want %q", w.path, path)
			}
		})
	}
}

// TestNewSessionWatcher_MissingFile proves a watcher cannot be created for a
// nonexistent session file (fsnotify add fails).
func TestNewSessionWatcher_MissingFile(t *testing.T) {
	_, err := newSessionWatcher(filepath.Join(t.TempDir(), "absent.jsonl"), -1)
	if err == nil {
		t.Fatal("expected error for missing session file")
	}
}

// TestWatcher_RoutesToBackendSessionAndTurnEvents is the end-to-end revival
// proof: it wires a Backend exactly as the agent does — session-scoped delivery
// via AttachSessionEvents, per-turn bookkeeping installed as turnEvents — then
// drives the JSONL watcher (whose sink is the Backend) over a transcript with
// text, a tool call, its result, and an end_turn. It asserts the SessionEvents
// delivery callbacks fired with the right values AND TurnEvents.OnTurnComplete
// fired exactly once. This is the path that was dead post-#747 (AttachSessionEvents
// was a no-op and Inject read the nil inj.Handler).
func TestWatcher_RoutesToBackendSessionAndTurnEvents(t *testing.T) {
	b := &Backend{}

	var texts []string
	var toolStarts, toolEnds []string
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnText:      func(text string) { texts = append(texts, text) },
		OnToolStart: func(_, name, _ string) { toolStarts = append(toolStarts, name) },
		OnToolEnd:   func(_, name, _ string, _ bool) { toolEnds = append(toolEnds, name) },
	})

	completes := 0
	var gotResult *delegator.TurnResult
	b.turnMu.Lock()
	b.turnEvents = &delegator.TurnEvents{OnTurnComplete: func(r *delegator.TurnResult) {
		completes++
		gotResult = r
	}}
	b.turnMu.Unlock()

	// A representative single turn: assistant text + tool_use, the tool_result
	// on a user message, then an end_turn assistant message.
	content := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working on it"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file.txt","is_error":false}]}}` + "\n" +
		endTurnLine("done")

	path := writeSessionFile(t, content)
	w, err := newSessionWatcher(path, 0)
	if err != nil {
		t.Fatalf("newSessionWatcher: %v", err)
	}
	defer w.close()
	w.setEvents(b) // the watcher's sink is the Backend, as in startWatcher

	w.readNew(b)

	// Delivery routed through SessionEvents.
	if len(texts) != 2 || texts[0] != "working on it" || texts[1] != "done" {
		t.Errorf("OnText calls = %v, want [\"working on it\" \"done\"]", texts)
	}
	if len(toolStarts) != 1 || toolStarts[0] != "Bash" {
		t.Errorf("OnToolStart = %v, want [Bash]", toolStarts)
	}
	if len(toolEnds) != 1 || toolEnds[0] != "Bash" {
		t.Errorf("OnToolEnd = %v, want [Bash] (name correlated via tool_use id)", toolEnds)
	}

	// Completion routed through the per-turn TurnEvents, exactly once.
	if completes != 1 {
		t.Fatalf("OnTurnComplete fired %d times, want 1", completes)
	}
	if gotResult == nil || gotResult.Text != "done" || gotResult.ToolCalls != 1 {
		t.Errorf("TurnResult = %+v, want Text=done ToolCalls=1", gotResult)
	}

	// One-shot: turnEvents cleared after completion.
	b.turnMu.Lock()
	te := b.turnEvents
	b.turnMu.Unlock()
	if te != nil {
		t.Error("turnEvents should be nil after the turn completed")
	}
}

// TestReadNew_IncrementalReads proves readNew processes only lines after the
// stored offset and advances it, so repeated calls never re-deliver entries.
func TestReadNew_IncrementalReads(t *testing.T) {
	path := writeSessionFile(t, endTurnLine("first"))
	w, err := newSessionWatcher(path, 0)
	if err != nil {
		t.Fatalf("newSessionWatcher: %v", err)
	}
	defer w.close()

	var texts []string
	handler := &testEvents{
		OnText: func(text string) { texts = append(texts, text) },
	}

	w.readNew(handler)
	if len(texts) != 1 || texts[0] != "first" {
		t.Fatalf("texts after first read = %v, want [first]", texts)
	}

	// Re-reading without new content must deliver nothing.
	w.readNew(handler)
	if len(texts) != 1 {
		t.Fatalf("re-read delivered duplicates: %v", texts)
	}

	// Appended content is picked up from the stored offset.
	appendLine(t, path, endTurnLine("second"))
	w.readNew(handler)
	if len(texts) != 2 || texts[1] != "second" {
		t.Fatalf("texts after append = %v, want [first second]", texts)
	}
}

// TestReadNew_MissingFile proves readNew silently no-ops if the session file
// disappears (CC restart) instead of panicking or corrupting the offset.
func TestReadNew_MissingFile(t *testing.T) {
	path := writeSessionFile(t, "")
	w, err := newSessionWatcher(path, 0)
	if err != nil {
		t.Fatalf("newSessionWatcher: %v", err)
	}
	defer w.close()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Should not panic.
	w.readNew(&testEvents{})
	if w.offset != 0 {
		t.Errorf("offset = %d, should be untouched when file is gone", w.offset)
	}
}

// TestWatchLoop_DeliversAppendedEntries proves the fsnotify-driven loop: an
// entry appended after the loop starts is read and dispatched to the current
// handler without polling.
func TestWatchLoop_DeliversAppendedEntries(t *testing.T) {
	path := writeSessionFile(t, "")
	w, err := newSessionWatcher(path, -1)
	if err != nil {
		t.Fatalf("newSessionWatcher: %v", err)
	}
	defer w.close()

	turnDone := make(chan *delegator.TurnResult, 1)
	w.setEvents(&testEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { turnDone <- r },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loopDone := make(chan struct{})
	go func() {
		w.watchLoop(ctx)
		close(loopDone)
	}()

	appendLine(t, path, endTurnLine("live entry"))

	select {
	case r := <-turnDone:
		if r.Text != "live entry" {
			t.Errorf("turn text = %q, want %q", r.Text, "live entry")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchLoop did not deliver the appended entry")
	}

	cancel()
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLoop did not exit on context cancel")
	}
}

// TestWatchLoop_NoLossAcrossNilHandler proves entries written while no
// handler is installed are not lost: the loop leaves the offset untouched
// when the handler is nil, so once a handler is installed the buffered entry
// and any later entries are each delivered exactly once, in order.
func TestWatchLoop_NoLossAcrossNilHandler(t *testing.T) {
	path := writeSessionFile(t, "")
	w, err := newSessionWatcher(path, -1)
	if err != nil {
		t.Fatalf("newSessionWatcher: %v", err)
	}
	defer w.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.watchLoop(ctx)

	// First entry lands while no handler is installed.
	appendLine(t, path, endTurnLine("buffered"))

	texts := make(chan string, 4)
	w.setEvents(&testEvents{
		OnText: func(text string) { texts <- text },
	})

	// Second entry triggers an fsnotify event with the handler in place;
	// the read starts from the untouched offset so both entries arrive.
	appendLine(t, path, endTurnLine("live"))

	want := []string{"buffered", "live"}
	for _, expected := range want {
		select {
		case got := <-texts:
			if got != expected {
				t.Fatalf("text = %q, want %q", got, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", expected)
		}
	}
	select {
	case extra := <-texts:
		t.Fatalf("unexpected duplicate delivery: %q", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestWatchLoop_ExitsWhenWatcherClosed proves closing the fsnotify watcher
// terminates the loop (events channel closes) rather than leaking the goroutine.
func TestWatchLoop_ExitsWhenWatcherClosed(t *testing.T) {
	path := writeSessionFile(t, "")
	w, err := newSessionWatcher(path, -1)
	if err != nil {
		t.Fatalf("newSessionWatcher: %v", err)
	}

	loopDone := make(chan struct{})
	go func() {
		w.watchLoop(context.Background())
		close(loopDone)
	}()

	w.close()

	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLoop did not exit after watcher close")
	}
}

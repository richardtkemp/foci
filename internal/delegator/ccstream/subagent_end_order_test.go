package ccstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestTaskNotification_SubagentTextPrecedesSubagentEnd pins the ORDER the app
// relies on (#1169 issue 2): every SubagentText of a foreground subagent's run,
// including its FINAL message, is delivered before that run's SubagentEnd.
//
// A foreground subagent's text reaches foci only through its transcript tail,
// and CC writes the final message to the transcript at about the moment it
// emits task_notification. The handler therefore drains the tail
// (subagentTails().finalize, which blocks until the tail has read the
// transcript to its terminal record) BEFORE it fires OnSubagentEnd. Fire the end
// first and the final text lands in a chit already marked complete.
//
// Driven through the real handlers (OnAssistant, OnSystem) rather than the tail
// manager, so it fails if the handler reorders the two calls, which the
// tail-manager tests cannot see. Verified red by swapping finalize and
// OnSubagentEnd in the task_notification case.
//
// Variants, all asserting the same order:
//   - idle tail: the poll interval is effectively infinite, so the final line
//     can ONLY be read by the drain finalize triggers. This makes the wrong
//     order fail deterministically rather than depending on scheduling.
//   - polling tail: the production shape, a tail polling in a tight loop while
//     the final line is appended.
//   - late write: the final line lands on the transcript AFTER the
//     task_notification (the two travel on different channels and are not
//     ordered, #1938); the end must still wait for it.
//
// Timing is load-tolerant: the only waits are generous deadlines, and the
// settle window is long because a correctly-ordered run returns as soon as the
// transcript reaches its terminal record, so the window costs nothing on the
// passing path.
func TestTaskNotification_SubagentTextPrecedesSubagentEnd(t *testing.T) {
	for _, tc := range []struct {
		name      string
		poll      time.Duration
		lateWrite bool
	}{
		{"idle tail", time.Hour, false},
		{"polling tail", time.Millisecond, false},
		{"late write", time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Package globals: not parallel.
			origPoll, origWait, origSettle := subagentTailPoll, subagentTailFileWait, subagentTailSettle
			subagentTailPoll = tc.poll
			subagentTailFileWait = 30 * time.Second
			subagentTailSettle = 30 * time.Second
			t.Cleanup(func() {
				subagentTailPoll, subagentTailFileWait, subagentTailSettle = origPoll, origWait, origSettle
			})
			t.Setenv("HOME", t.TempDir())

			const (
				toolUseID = "toolu_order_agent"
				taskID    = "a1169000000000001"
			)
			b := &Backend{workDir: "/home/foci/clutch"}
			b.sessionID = "11690000-0000-0000-0000-000000000000"
			t.Cleanup(func() { b.subagentTails().stopAll() })

			// One ordered log of everything the session sees for the group.
			var (
				mu     sync.Mutex
				events []string
			)
			record := func(ev string) {
				mu.Lock()
				events = append(events, ev)
				mu.Unlock()
			}
			snapshot := func() []string {
				mu.Lock()
				defer mu.Unlock()
				return append([]string(nil), events...)
			}
			b.AttachSessionEvents(&delegator.SessionEvents{
				OnSubagentStart: func(g, _, _ string, _ int) { record("start:" + g) },
				OnSubagentText:  func(g, text string, _ int) { record("text:" + g + ":" + text) },
				OnSubagentEnd:   func(g string, _ int) { record("end:" + g) },
			})
			b.beginTurn(&delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) {}})

			path := b.subagentFilePath(taskID, ".jsonl")
			if path == "" {
				t.Fatal("subagentFilePath returned empty: the test cannot exercise the tail")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			// The run's progress so far: text written before completion.
			var early []string
			var body string
			for i := 1; i <= 3; i++ {
				text := fmt.Sprintf("PROGRESS-%d", i)
				early = append(early, text)
				body += assistantLine(text)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}

			// A FOREGROUND Agent: its text reaches foci only via the tail.
			b.OnAssistant(&AssistantMessage{Message: BetaMessage{Content: []ContentBlock{{
				Type: "tool_use", ID: toolUseID, Name: "Agent",
				Input: json.RawMessage(`{"description":"order","prompt":"go","run_in_background":false}`),
			}}}})
			sys := func(ev TaskEvent) {
				raw, _ := json.Marshal(ev)
				b.OnSystem(ev.Subtype, raw)
			}
			sys(TaskEvent{Type: "system", Subtype: "task_started", TaskID: taskID, ToolUseID: toolUseID, TaskType: "local_agent"})

			// Let the tail deliver the early text, so the final message is
			// genuinely the last thing still unread when the notification lands.
			deadline := time.Now().Add(30 * time.Second)
			for len(snapshot()) < 1+len(early) {
				if time.Now().After(deadline) {
					t.Fatalf("tail did not deliver the early text: events = %v", snapshot())
				}
				time.Sleep(time.Millisecond)
			}

			const final = "FINAL-ANSWER"
			finalLine := `{"type":"assistant","isSidechain":true,"message":{"id":"msg_final","stop_reason":"end_turn",` +
				`"content":[{"type":"text","text":"` + final + `"}]}}` + "\n"
			appendFinal := func() {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Error(err)
					return
				}
				defer f.Close()
				if _, err := f.WriteString(finalLine); err != nil {
					t.Error(err)
				}
			}
			var wg sync.WaitGroup
			if tc.lateWrite {
				wg.Add(1)
				go func() {
					defer wg.Done()
					time.Sleep(50 * time.Millisecond)
					appendFinal()
				}()
			} else {
				appendFinal()
			}
			sys(TaskEvent{Type: "system", Subtype: "task_notification", TaskID: taskID, ToolUseID: toolUseID, Status: "completed"})
			wg.Wait()

			// OnSubagentEnd fires synchronously inside OnSystem, so the end is
			// recorded by now.
			want := []string{"start:" + toolUseID}
			for _, text := range early {
				want = append(want, "text:"+toolUseID+":"+text)
			}
			want = append(want, "text:"+toolUseID+":"+final, "end:"+toolUseID)

			got := snapshot()
			if len(got) != len(want) {
				// Catch a straggler delivered after OnSystem returned, so the
				// failure message shows the full sequence.
				time.Sleep(100 * time.Millisecond)
				got = snapshot()
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("session events out of order (every SubagentText must precede SubagentEnd):\n got  %v\n want %v", got, want)
			}
		})
	}
}

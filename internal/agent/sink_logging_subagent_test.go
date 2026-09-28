package agent

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/convo"
	"foci/internal/delegator"
	"foci/internal/turnevent"
)

// initTestConvo opens a per-agent conversation DB for agentID and returns its
// path. convo is package-global, so tests using it must not run in parallel.
func initTestConvo(t *testing.T, agentID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), agentID+".db")
	if err := convo.InitPerAgent([]string{agentID}, func(string) string { return path }); err != nil {
		t.Fatalf("InitPerAgent: %v", err)
	}
	t.Cleanup(convo.Close)
	return path
}

// convoRows reads the sent rows of the conversation DB as
// "content_type|group|run|turn_kind|text" lines, in insert order.
func convoRows(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open conversation db: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT content_type, COALESCE(subagent_group, ''), COALESCE(subagent_run, 0),
		COALESCE(turn_kind, ''), text FROM messages WHERE direction = 'sent' ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ct, group, kind, text string
		var run int
		if err := rows.Scan(&ct, &group, &run, &kind, &text); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%d|%s|%s", ct, group, run, kind, text))
	}
	return out
}

func assertRows(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("conversation rows =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestLoggingSink_RecordsSubagentOutput pins #2060's core: the conversation
// logging wrapper records a subagent's start prompt (with its label), its
// follow-up prompts and its text blocks, tagged with group and run, under the
// parent session key — so memory search can find what a subagent found after
// the backend's own transcript is cleaned up.
func TestLoggingSink_RecordsSubagentOutput(t *testing.T) {
	path := initTestConvo(t, "test")
	a := &Agent{}
	s := newLoggingSink(turnevent.NopSink{}, a, 7, &TurnMetadata{}, "test/c1")
	ctx := context.Background()

	s.Emit(ctx, turnevent.TextBlock{Text: "dispatching", Phase: turnevent.PhaseIntermediate})
	s.Emit(ctx, turnevent.SubagentStart{GroupKey: "toolu_A", Label: "probe", Prompt: "find the bug", RunIndex: 1})
	s.Emit(ctx, turnevent.SubagentText{GroupKey: "toolu_A", Text: "found it in foo.go", RunIndex: 1})
	s.Emit(ctx, turnevent.SubagentPrompt{GroupKey: "toolu_A", Prompt: "also check bar.go", RunIndex: 1})
	s.Emit(ctx, turnevent.SubagentStart{GroupKey: "toolu_B", Label: "codex-agent", RunIndex: 2}) // codex: no prompt
	s.Emit(ctx, turnevent.SubagentEnd{GroupKey: "toolu_A", RunIndex: 1})                         // not text: not recorded
	s.Emit(ctx, turnevent.SubagentText{GroupKey: "toolu_A", RunIndex: 1})                        // empty: not recorded

	assertRows(t, convoRows(t, path), []string{
		"text||0||dispatching",
		"subagent_prompt|toolu_A|1||[subagent: probe]\nfind the bug",
		"subagent|toolu_A|1||found it in foo.go",
		"subagent_prompt|toolu_A|1||also check bar.go",
		"subagent_prompt|toolu_B|2||[subagent: codex-agent]",
	})
}

// TestRecordingSystemSink covers which non-delivered turns get recorded
// (Dick's #2060 rulings): reflection, session-end memory, background,
// consolidation and /branch are recorded and tagged with their kind; keepalive,
// compaction memory and the batch utility purposes are not; and a sink that
// already logs is not wrapped again (no double rows).
func TestRecordingSystemSink(t *testing.T) {
	for _, trigger := range []string{"reflection", "session_end_memory", "background", "consolidation", "branch"} {
		t.Run("recorded/"+trigger, func(t *testing.T) {
			path := initTestConvo(t, "test")
			a := &Agent{}
			ts := &TurnState{SessionKey: "test/c1", Trigger: trigger, Meta: &TurnMetadata{}}
			s := a.recordingSystemSink(turnevent.NopSink{}, ts)
			if s.DeliversToPlatform() {
				t.Error("recording wrapper made a non-delivered turn deliver")
			}
			s.Emit(context.Background(), turnevent.TextBlock{Text: "thought", Phase: turnevent.PhaseIntermediate})
			s.Emit(context.Background(), turnevent.SubagentText{GroupKey: "toolu_A", Text: "sub", RunIndex: 1})
			assertRows(t, convoRows(t, path), []string{
				"text||0|" + trigger + "|thought",
				"subagent|toolu_A|1|" + trigger + "|sub",
			})
		})
	}
	for _, trigger := range []string{"keepalive", "compaction_memory", "nudge_extraction", "summary", "prompt_diff", "user", ""} {
		t.Run("unrecorded/"+trigger, func(t *testing.T) {
			path := initTestConvo(t, "test")
			a := &Agent{}
			ts := &TurnState{SessionKey: "test/c1", Trigger: trigger, Meta: &TurnMetadata{}}
			inner := turnevent.Sink(turnevent.NopSink{})
			if s := a.recordingSystemSink(inner, ts); s != inner {
				t.Fatalf("trigger %q: sink was wrapped (%T), want it returned unchanged", trigger, s)
			}
			if rows := convoRows(t, path); len(rows) != 0 {
				t.Errorf("rows = %v, want none", rows)
			}
		})
	}
	t.Run("already-logging", func(t *testing.T) {
		path := initTestConvo(t, "test")
		a := &Agent{}
		ts := &TurnState{SessionKey: "test/c1", Trigger: "branch", Meta: &TurnMetadata{}}
		logged := fnSink(func(context.Context, turnevent.Event) {})
		inner := a.WrapConversationLogging(logged, "test/c1")
		s := a.recordingSystemSink(inner, ts)
		if s != inner {
			t.Fatalf("an already-logging sink was wrapped again (%T)", s)
		}
		s.Emit(context.Background(), turnevent.TextBlock{Text: "once", Phase: turnevent.PhaseIntermediate})
		if rows := convoRows(t, path); len(rows) != 1 {
			t.Errorf("rows = %v, want exactly one", rows)
		}
	})
}

// waitRegistered blocks until a turn has registered a sink on sk's router —
// the orchestrator's Phase 3.5, which runs after dispatch. Syncs on the state,
// not a sleep; the deadline is only a hang guard.
func waitRegistered(t *testing.T, a *Agent, sk string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a.sessionRouter(sk).current.Load() != nil {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("no sink registered on %s's router", sk)
	return false
}

// TestSystemTurn_RecordsReflectionOutput drives a real delegated turn through
// HandleMessage (no sink on ctx, as the periodic runner does) and has the
// backend emit text and subagent output after dispatch, the way a live
// backend does. A reflection turn's output must be recorded tagged
// "reflection"; before #2060 its NopSink suppressed the recording along with
// the delivery. A keepalive turn stays unrecorded.
func TestSystemTurn_RecordsReflectionOutput(t *testing.T) {
	for _, c := range []struct {
		trigger string
		want    []string
	}{
		{"reflection", []string{
			"text||0|reflection|noted a lesson",
			"subagent_prompt|toolu_R|1|reflection|[subagent: dig]\ncheck the logs",
			"subagent|toolu_R|1|reflection|logs are clean",
		}},
		{"keepalive", nil},
	} {
		t.Run(c.trigger, func(t *testing.T) {
			path := initTestConvo(t, "helen")
			be := &batchTurnBackend{}
			be.sessionFile = "/tmp/reflect.jsonl"
			sk := "helen/c42"
			var a *Agent
			be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
				be.mu.Lock()
				se := be.sessionEvents
				be.mu.Unlock()
				go func() {
					defer h.OnTurnComplete(&delegator.TurnResult{Text: "noted a lesson", Model: "claude-sonnet-4-5"})
					if !waitRegistered(t, a, sk) {
						return
					}
					se.OnText("noted a lesson")
					se.OnSubagentStart("toolu_R", "dig", "check the logs", 1)
					se.OnSubagentText("toolu_R", "logs are clean", 1)
				}()
				return nil, nil
			}
			a = newBatchTestAgent(t, be)

			if err := a.HandleMessage(WithTrigger(context.Background(), c.trigger), sk, []string{"reflect"}, nil); err != nil {
				t.Fatalf("HandleMessage: %v", err)
			}
			assertRows(t, convoRows(t, path), c.want)
		})
	}
}

// TestRunBatchTurn_RecordsConsolidation: a consolidation batch is recorded
// from its first event, tagged "consolidation", and exactly once (the batch
// wrapper and the orchestrator's registration must not both log). A nudge
// extraction batch is not recorded.
func TestRunBatchTurn_RecordsConsolidation(t *testing.T) {
	for _, c := range []struct {
		purpose string
		want    []string
	}{
		{delegator.BatchPurposeConsolidation, []string{"text||0|consolidation|merged 3 notes"}},
		{delegator.BatchPurposeNudgeExtraction, nil},
	} {
		t.Run(c.purpose, func(t *testing.T) {
			path := initTestConvo(t, "helen")
			be := &batchTurnBackend{}
			be.sessionFile = "/tmp/batch.jsonl"
			be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
				h.OnText("merged 3 notes")
				h.OnTurnComplete(&delegator.TurnResult{Text: "merged 3 notes", Model: "claude-sonnet-4-5"})
				return nil, nil
			}
			a := newBatchTestAgent(t, be)
			if _, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{
				Prompt: "consolidate", OwnerSessionKey: "helen/c42", Purpose: c.purpose,
			}); err != nil {
				t.Fatalf("RunBatch: %v", err)
			}
			assertRows(t, convoRows(t, path), c.want)
		})
	}
}

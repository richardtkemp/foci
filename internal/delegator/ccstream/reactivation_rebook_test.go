package ccstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// The #2057 live reproduction, 2026-09-27 on 644f8b21e (clutch): a background
// haiku subagent ran once (run 1, booked as row 49952, output 747), foci was
// restarted by a deploy, and a SendMessage resumed it. The resume rehydrated the
// run from meta.json and started a run-2 tail, which read the transcript from
// byte 0 (lines=58). The fixture is that transcript reduced to the fields the
// tail reads (type, isSidechain, timestamp, message id/model/usage/stop_reason,
// content kinds and a prompt's first line); its line count and every usage
// figure are the live ones.
const (
	rebookFixture = "testdata/reactivation_2057_agent-a3a7eb579bc975fa4.jsonl"
	rebookTaskID  = "a3a7eb579bc975fa4"
	rebookAgentTU = "toolu_0149hLbkdxVeSRhhoce2dqGg" // the Agent spawn == group key
	rebookSendTU  = "toolu_01EDXa6RmMck7i4DCZAF1WBc" // the SendMessage that resumed it
	rebookModel   = "claude-haiku-4-5-20251001"
	// rebookRun2Prompt is the reactivation's own prompt record in the fixture.
	rebookRun2Prompt = "2026-09-27T18:03:04.419Z"

	rebookRun1Out = 198 + 128 + 169 + 29 + 223 // 747, row 49952
	rebookRun2Out = 118 + 29 + 269 + 45 + 67   // 528, the transcript's run 2
)

// writeRebookTranscript writes the fixture under HOME, with every timestamp
// moved by the same offset so the run-2 prompt lands at run2At. The live gaps
// are kept: run 1 sits 12.5 minutes before it, as it did.
func writeRebookTranscript(t *testing.T, b *Backend, run2At time.Time) {
	t.Helper()
	src, err := os.ReadFile(rebookFixture)
	if err != nil {
		t.Fatal(err)
	}
	anchor, _ := time.Parse(time.RFC3339Nano, rebookRun2Prompt)
	shift := run2At.Sub(anchor)

	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		if ts, ok := rec["timestamp"].(string); ok {
			at, err := time.Parse(time.RFC3339Nano, ts)
			if err != nil {
				t.Fatal(err)
			}
			rec["timestamp"] = at.Add(shift).UTC().Format(time.RFC3339Nano)
		}
		line, _ := json.Marshal(rec)
		out.Write(line)
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	path := b.subagentTranscriptPath(rebookTaskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := `{"agentType":"general-purpose","description":"#2057 repro subagent run 1","toolUseId":"` +
		rebookAgentTU + `","spawnDepth":1,"requestShape":"background","model":"haiku"}`
	if err := os.WriteFile(b.subagentFilePath(rebookTaskID, ".meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rebookRun drives a post-restart reactivation through the real handlers: a
// fresh backend (optionally after one result, as the new CC process may have
// produced), the transcript and meta.json written by write, then the resume's
// task_started and task_notification and a result pricing it. Returns the
// result and the debug log.
func rebookRun(t *testing.T, priorResult bool, write func(b *Backend, now time.Time)) (*delegator.TurnResult, string) {
	t.Helper()
	withFastTail(t)
	logs := captureDebugLog(t)
	t.Setenv("HOME", t.TempDir())

	b := &Backend{workDir: "/home/foci/clutch"}
	b.sessionID = "96438086-09ea-4ec3-a515-c49564b7f96d"
	t.Cleanup(b.subagentTails().stopAll)
	b.mu.Lock()
	b.lastModel = "claude-opus-5"
	b.mu.Unlock()

	var result *delegator.TurnResult
	complete := func(r *delegator.TurnResult) { result = r }
	opus := ModelUsage{InputTokens: 20, OutputTokens: 200, CostUSD: 0.02}
	if priorResult {
		b.beginTurn(&delegator.TurnEvents{TurnID: "turn-first", OnTurnComplete: complete})
		b.OnResult(&ResultMessage{Subtype: "success", Result: "ok", ModelUsage: map[string]ModelUsage{
			"claude-opus-5": {InputTokens: 10, OutputTokens: 100, CostUSD: 0.01},
		}})
		opus = ModelUsage{InputTokens: 30, OutputTokens: 300, CostUSD: 0.03} // cumulative
	}

	write(b, time.Now())

	result = reactivateAndPrice(t, b, map[string]ModelUsage{
		"claude-opus-5": opus,
		rebookModel:     {InputTokens: 50, OutputTokens: 1000, CacheReadInputTokens: 100000, CacheCreationInputTokens: 20000, CostUSD: 0.03},
	})
	return result, logs.String()
}

// reactivateAndPrice runs the resume's turn through the real handlers: the
// SendMessage's task_started and task_notification for the subagent, then a
// result reporting modelUsage. Returns that turn's result.
func reactivateAndPrice(t *testing.T, b *Backend, modelUsage map[string]ModelUsage) *delegator.TurnResult {
	t.Helper()
	var result *delegator.TurnResult
	b.beginTurn(&delegator.TurnEvents{TurnID: "turn-resume", OnTurnComplete: func(r *delegator.TurnResult) { result = r }})
	sys := func(ev TaskEvent) {
		raw, _ := json.Marshal(ev)
		b.OnSystem(ev.Subtype, raw)
	}
	sys(TaskEvent{Type: "system", Subtype: "task_started", TaskID: rebookTaskID, ToolUseID: rebookSendTU, TaskType: "local_agent"})
	sys(TaskEvent{Type: "system", Subtype: "task_notification", TaskID: rebookTaskID, ToolUseID: rebookSendTU, Status: "completed"})
	b.OnResult(&ResultMessage{Subtype: "success", Result: "ok", SessionID: b.SessionID(), ModelUsage: modelUsage})
	if result == nil || result.Usage == nil {
		t.Fatal("no turn result")
	}
	return result
}

// checkRebookShare asserts one share, for the subagent, of wantOut output, and
// no #1918 corrections.
func checkRebookShare(t *testing.T, result *delegator.TurnResult, wantOut int, why string) {
	t.Helper()
	if n := len(result.Usage.Subagents); n != 1 {
		t.Fatalf("Subagents = %+v, want exactly one share", result.Usage.Subagents)
	}
	sc := result.Usage.Subagents[0]
	if sc.AgentID != rebookAgentTU {
		t.Errorf("share agent = %s, want %s", sc.AgentID, rebookAgentTU)
	}
	if sc.Counts.Output != wantOut {
		t.Errorf("share output = %d, want %d: %s", sc.Counts.Output, wantOut, why)
	}
	if len(result.Usage.Corrections) != 0 {
		var moved int
		for _, c := range result.Usage.Corrections {
			moved += c.Counts.Output
		}
		t.Errorf("%d cost corrections (output %d) — an earlier run's messages were treated as late spend absorbed by a parent turn (#1918), but that run was booked on its own row before the restart",
			len(result.Usage.Corrections), moved)
	}
}

func checkLog(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q\n--- log ---\n%s", want, out)
		}
	}
}

// writeSynthTranscript writes a transcript built from recs, whose timestamps
// are offsets from now, plus the meta.json that identifies the subagent.
func writeSynthTranscript(t *testing.T, b *Backend, now time.Time, recs []synthRec) {
	t.Helper()
	var out bytes.Buffer
	for _, r := range recs {
		rec := map[string]any{"type": r.kind, "isSidechain": true,
			"timestamp": now.Add(r.off).UTC().Format(time.RFC3339Nano)}
		if r.kind == "user" {
			rec["message"] = map[string]any{"content": "prompt"}
		} else {
			msg := map[string]any{"id": r.id, "model": rebookModel,
				"usage": map[string]any{"input_tokens": 1, "output_tokens": r.out}, "content": []any{}}
			if r.stop != "" {
				msg["stop_reason"] = r.stop
			}
			rec["message"] = msg
		}
		line, _ := json.Marshal(rec)
		out.Write(line)
		out.WriteByte('\n')
	}
	path := b.subagentTranscriptPath(rebookTaskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := `{"description":"synthetic","toolUseId":"` + rebookAgentTU + `","spawnDepth":1}`
	if err := os.WriteFile(b.subagentFilePath(rebookTaskID, ".meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

type synthRec struct {
	kind string // "user" (a prompt) or "assistant"
	off  time.Duration
	id   string
	stop string
	out  int
}

func prompt(off time.Duration) synthRec { return synthRec{kind: "user", off: off} }
func reply(off time.Duration, id, stop string, out int) synthRec {
	return synthRec{kind: "assistant", off: off, id: id, stop: stop, out: out}
}

// TestReactivationAfterRestart_BooksOnlyItsOwnRun is #2057, replayed from the
// live transcript. A reactivation tail reads from byte 0; after a restart the
// accumulator has no memory of run 1, so run 1 was booked a second time. Which
// way depends on whether the new process had produced a result before the
// resume:
//
//   - after (today's reproduction): run 1's messages predate lastResultAt, so
//     they became #1918 late-spend corrections against the parent turn — live,
//     one applied (223 output moved onto the new row, which read 751) and four
//     skipped with a WARN;
//   - before (the 2026-09-22 case): lastResultAt is zero, nothing is late, and
//     the new row carried all of run 1 as well as run 2.
//
// Either way the new run must book exactly run 2 (528 output) and move nothing.
// Run 1 includes a Stop-hook bounce (an end_turn, a prompt, another end_turn),
// so the boundary must be the prompt after run 1's LAST at-rest record.
func TestReactivationAfterRestart_BooksOnlyItsOwnRun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		priorResult bool
	}{
		{"resume after the new process's first result", true},
		{"resume before the new process's first result", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, logs := rebookRun(t, tc.priorResult, func(b *Backend, now time.Time) {
				// The run-2 prompt is written a moment after the resume's
				// task_started is received, as CC may; run 1 sits 12.5 minutes
				// earlier, before the prior result.
				writeRebookTranscript(t, b, now.Add(500*time.Millisecond))
			})
			checkRebookShare(t, result, rebookRun2Out,
				"run 2 alone; run 1's 747 was already booked before the restart")
			checkLog(t, logs,
				"subagent_rehydrate task_id="+rebookTaskID,
				"subagent tail: run start group="+rebookAgentTU,
				"subagent tail: closed group="+rebookAgentTU+" lines=58 usage=9 completed=7 terminal=true before_run=11")
		})
	}
}

// TestReactivationAfterRestart_StopHookBounceJustBeforeResume: run 1 was sent
// back by a Stop hook moments before the resume, so its bounce prompt is close
// in TIME to the resume's own. The boundary is placed by ORDER — the first
// prompt after run 1's last at-rest record — so run 1's post-bounce reply is
// not booked again.
func TestReactivationAfterRestart_StopHookBounceJustBeforeResume(t *testing.T) {
	result, logs := rebookRun(t, false, func(b *Backend, now time.Time) {
		writeSynthTranscript(t, b, now, []synthRec{
			prompt(-60 * time.Second),
			reply(-3*time.Second, "m1", "end_turn", 10),
			prompt(-2500 * time.Millisecond), // the Stop hook sends run 1 back
			reply(-2*time.Second, "m2", "end_turn", 20),
			prompt(-100 * time.Millisecond), // the resume's prompt, before task_started is read
			reply(time.Second, "m3", "tool_use", 40),
			reply(2*time.Second, "m4", "end_turn", 80),
		})
	})
	checkRebookShare(t, result, 120, "run 2 is m3+m4; m1 and m2 (run 1, either side of its Stop-hook bounce) were booked before the restart")
	checkLog(t, logs, "before_run=2")
}

// TestReactivationAfterRestart_TaskStartedReceivedLate: foci read the resume's
// task_started seconds after run 2 had begun writing. Its first records predate
// the receive time but follow its prompt, so they are run 2's and are booked.
func TestReactivationAfterRestart_TaskStartedReceivedLate(t *testing.T) {
	result, logs := rebookRun(t, false, func(b *Backend, now time.Time) {
		writeSynthTranscript(t, b, now, []synthRec{
			prompt(-10 * time.Minute),
			reply(-9*time.Minute, "m1", "end_turn", 10),
			prompt(-8 * time.Second),
			reply(-7*time.Second, "m2", "tool_use", 40),
			reply(-4*time.Second, "m3", "tool_use", 50),
			reply(time.Second, "m4", "end_turn", 80),
		})
	})
	checkRebookShare(t, result, 170, "run 2 is m2+m3+m4, including the two records written before task_started was received")
	checkLog(t, logs, "before_run=1")
}

// TestReactivationAfterRestart_NoAtRestRecordFallsBack: with no at-rest record
// before task_started the boundary cannot be placed, so the tail books the
// whole transcript as it did before #2057, and says so.
func TestReactivationAfterRestart_NoAtRestRecordFallsBack(t *testing.T) {
	result, logs := rebookRun(t, false, func(b *Backend, now time.Time) {
		writeSynthTranscript(t, b, now, []synthRec{
			prompt(-60 * time.Second),
			reply(-50*time.Second, "m1", "tool_use", 10),
			prompt(-100 * time.Millisecond),
			reply(time.Second, "m2", "end_turn", 20),
		})
	})
	checkRebookShare(t, result, 30, "fallback: the whole transcript")
	checkLog(t, logs, "subagent tail: run start NOT found group="+rebookAgentTU+": no at-rest record before task_started")
}

// The fixture's cache traffic per run, summed over its unique message ids (the
// accumulator books each id once): read + write.
const (
	rebookRun1CacheRead  = 12660 + 14375 + 14559 + 14789 // + 0 on the first message
	rebookRun1CacheWrite = 12660 + 1715 + 184 + 230 + 22
	rebookRun2CacheRead  = 15085 + 15253 + 15325 + 15657
	rebookRun2CacheWrite = 15085 + 168 + 72 + 332 + 22
)

// TestReactivationAfterCCRelaunch_BooksOnlyItsOwnRun is the #2087 shape: no
// foci restart, but a CC PROCESS relaunch inside a running foci. After the
// 2026-09-27 compaction, foci closed CC and started `claude --resume` in a new
// Backend, and six SendMessage resumes of earlier subagents arrived before that
// process's first result. The new Backend knew nothing of their earlier runs,
// so on a binary without #2057's gate each reactivation tail booked them again:
// 15.2M cache tokens, $10.88 on one turn.
//
// This runs the new Backend through the real Start with --resume (so the
// baseline is seeded from the old process's cost-state record, which already
// holds run 1), with CC's init streamed by a stub, then the reactivation and
// the first result. CC's ModelUsage holds run 1 in the baseline and adds only
// run 2, so the #2013 fresh-process guard is also a witness: re-booking run 1
// puts the seen work 71,194 cache tokens above the delta and fires it.
func TestReactivationAfterCCRelaunch_BooksOnlyItsOwnRun(t *testing.T) {
	withFastTail(t)
	logs := captureDebugLog(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	const sid = "96438086-09ea-4ec3-a515-c49564b7f96d"
	workDir := filepath.Join(home, "clutch")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The old process's shutdown record: the main thread's history, and the
	// subagent's run 1, which that process had already booked.
	opusBase := ModelUsage{InputTokens: 1000, OutputTokens: 5000, CacheReadInputTokens: 1_000_000, CacheCreationInputTokens: 50_000, CostUSD: 10}
	haikuBase := ModelUsage{InputTokens: 54, OutputTokens: rebookRun1Out,
		CacheReadInputTokens: rebookRun1CacheRead, CacheCreationInputTokens: rebookRun1CacheWrite, CostUSD: 0.02}
	state, _ := json.Marshal(map[string]any{"type": "cost-state", "sessionId": sid,
		"modelUsage": map[string]ModelUsage{"claude-opus-5": opusBase, rebookModel: haikuBase}})
	mainPath, err := ccTranscriptPath(workDir, sid)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, mainPath, string(state))

	// The relaunched CC: reports init, then idles until foci closes stdin.
	stub := filepath.Join(t.TempDir(), "claude-stub")
	script := "#!/bin/sh\nprintf '%s\\n' '" +
		`{"type":"system","subtype":"init","session_id":"` + sid + `","model":"claude-opus-5","claude_code_version":"2.1.280"}` +
		"'\nexec cat >/dev/null\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	be, err := newFromConfig(map[string]any{"binary": stub})
	if err != nil {
		t.Fatal(err)
	}
	b := be.(*Backend)
	guard := &delegator.ExpectationGuard{}
	b.expect = guard
	if err := b.Start(context.Background(), delegator.StartOptions{WorkDir: workDir, AgentID: "clutch", ResumeSessionID: sid}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	writeRebookTranscript(t, b, time.Now().Add(500*time.Millisecond))

	// ModelUsage is the restored baseline plus this process's work: the main
	// thread's output (no cache, so the main thread adds nothing the guard
	// weighs) and the subagent's run 2.
	opus := opusBase
	opus.OutputTokens += 200
	haiku := haikuBase
	haiku.InputTokens += 46
	haiku.OutputTokens += rebookRun2Out
	haiku.CacheReadInputTokens += rebookRun2CacheRead
	haiku.CacheCreationInputTokens += rebookRun2CacheWrite
	result := reactivateAndPrice(t, b, map[string]ModelUsage{"claude-opus-5": opus, rebookModel: haiku})

	checkRebookShare(t, result, rebookRun2Out,
		"run 2 alone; run 1's 747 was booked by the CC process that compaction closed")
	if n := guard.Count(expectBackend, invFreshProcessUsage); n != 0 {
		t.Errorf("fresh-process guard fired %d time(s): foci saw more subagent work than ModelUsage added over the baseline, "+
			"so run 1 was booked again", n)
	}
	out := logs.String()
	checkLog(t, out,
		"--resume "+sid,
		"resume baseline: 2 model(s)",
		"subagent_rehydrate task_id="+rebookTaskID,
		"subagent tail: run start group="+rebookAgentTU,
		"before_run=11")
	// Premise: the guard judged this result. It does so only on a process's
	// first result, and stands down after a compaction in the same process.
	b.mu.Lock()
	seen := b.resultSeen
	b.mu.Unlock()
	if !seen {
		t.Fatal("the result never reached the fresh-process check")
	}
}

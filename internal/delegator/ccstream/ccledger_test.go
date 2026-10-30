package ccstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/clock"
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/modelinfo"
)

// withLiveLedger opens a scratch ledger as the process's live one. Not for
// parallel tests: the live ledger is process-wide.
func withLiveLedger(t *testing.T) *accounting.Ledger {
	t.Helper()
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })
	return l
}

// onLedgerLine sets m's ledger hook to hand fn each assistant line, parsed as
// the adapter parses it.
func onLedgerLine(m *subagentTailManager, fn func(group string, l *ccLine)) *subagentTailManager {
	m.ledgerLine = func(group string, raw []byte) {
		if l, _ := parseCCRecord(raw); l != nil {
			fn(group, l)
		}
	}
	return m
}

// TestLedgerWiring drives the Backend's own hooks: the stream names a
// main-thread call, CC appends its line to the main transcript, a result
// arrives, the turn completes, and the process exits. The call lands in the
// LIVE ledger on its turn and reaches the observers (api.jsonl, the trace
// generation) — and by the time OnTurnComplete fires it is already booked, so
// the agent layer's turn total holds it. Not parallel: it sets the process's
// live ledger.
func TestLedgerWiring(t *testing.T) {
	live := withLiveLedger(t)
	var observed []string
	accounting.BookedHook = func(b accounting.Booking) { observed = append(observed, b.Call.Key) }
	t.Cleanup(func() { accounting.BookedHook = nil })

	b := newTestBackend(&bytes.Buffer{})
	lg := newCCLedger(b, "cap/c1", "cap", nil)
	b.ledger.Store(lg)
	transcript := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"id":"old","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":9}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(transcript)
	lg.startMainTail(transcript, fi.Size()) // a resumed process: the old line is an earlier process's

	var atComplete []accounting.CallRow
	b.turnMu.Lock()
	b.beginTurnLocked(&delegator.TurnEvents{TurnID: "cap/c1@1", OnTurnComplete: func(*delegator.TurnResult) {
		atComplete, _ = live.Calls(time.Time{})
	}})
	b.stateEventsSeen = true
	b.turnMu.Unlock()
	stop := "end_turn"
	b.OnAssistant(&AssistantMessage{Type: "assistant", Message: BetaMessage{ID: "msg_1", Model: "claude-opus-5",
		StopReason: &stop, Usage: TokenUsage{InputTokens: 10, OutputTokens: 20}}})
	b.OnResult(&ResultMessage{Type: "result", ModelUsage: map[string]ModelUsage{"claude-opus-5": {InputTokens: 10, OutputTokens: 20, CostUSD: 0.01}}})
	// CC writes the line just after the result (P0-b: within ~2ms).
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","timestamp":"2026-09-29T10:00:00Z","message":{"id":"msg_1","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n")
	_ = f.Close()
	b.completeTurn("idle")
	if len(atComplete) != 1 || atComplete[0].TurnID != "cap/c1@1" {
		t.Fatalf("calls when OnTurnComplete fired = %+v, want msg_1 already booked (the turn's flush)", atComplete)
	}
	lg.close()

	rows, err := live.Calls(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TurnID != "cap/c1@1" || rows[0].Count(modelinfo.ClassOutput) != 20 {
		t.Fatalf("live calls = %+v, want msg_1 on cap/c1@1", rows)
	}
	if !slices.Contains(observed, "msg_1") {
		t.Errorf("observers saw %v, want msg_1: a live booking reaches api.jsonl and the trace", observed)
	}
}

// TestLedgerFlushPassesWithoutAMainTail: with no main transcript being tailed
// (its path unknown), a named call's line can never come, so a turn's flush
// must not wait out its bound. Not parallel: it sets the bound.
func TestLedgerFlushPassesWithoutAMainTail(t *testing.T) {
	orig := ccBarrierBound
	ccBarrierBound = time.Minute // far past the deadline below: waiting it out fails
	t.Cleanup(func() { ccBarrierBound = orig })
	b := newTestBackend(&bytes.Buffer{})
	lg := newCCLedger(b, "cap/c1", "cap", nil)
	t.Cleanup(lg.close)
	lg.enqueue(ccEvent{kind: ccNamed, id: "msg_x", turn: "T1"})
	done := make(chan struct{})
	go func() { lg.flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("flush waited for a line no tail can read")
	}
}

// TestLedgerFlushPassesWhenOnlyARetractedCallIsUnseen is #2255 on the flush
// barrier: a named call a refusal fallback retracted is not one a turn's
// completion waits for, so the barrier passes at once even with a main tail
// running and the retracted line certain never to come. Not parallel: it
// sets the bound.
func TestLedgerFlushPassesWhenOnlyARetractedCallIsUnseen(t *testing.T) {
	orig := ccBarrierBound
	ccBarrierBound = time.Minute // far past the deadline below: waiting it out fails
	t.Cleanup(func() { ccBarrierBound = orig })
	b := newTestBackend(&bytes.Buffer{})
	lg := newCCLedger(b, "cap/c1", "cap", nil)
	t.Cleanup(lg.close)
	lg.enqueue(ccEvent{kind: ccSessionFile, id: "transcript.jsonl"}) // a main tail is running
	lg.enqueue(ccEvent{kind: ccNamed, id: "m1", uuid: "u1", turn: "T1"})
	lg.enqueue(ccEvent{kind: ccRefusalEv, refusal: &ccRefusal{model: opus, fallback: "claude-opus-4-8",
		retracted: []string{"u1"}}})
	done := make(chan struct{})
	go func() { lg.flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("flush waited for a line a refusal fallback retracted")
	}
}

// TestWorkflowAgentsBookOnInvokingTurn is #2130 defect 1. A Workflow
// run's agents write their transcripts under subagents/workflows/<run id>/,
// not subagents/, and the stream announces the whole run with one
// task_started. Their calls must be booked from those transcripts, each on
// the turn that invoked the Workflow (the #2111 ruling: subagent spend is
// tied to its invoking turn), with their observed 5m cache writes, even when
// an agent's transcript appears after that turn has ended. A result that
// arrives mid-run must not book the run's spend as a turn-less remainder, and
// the run's journal.jsonl is not an agent transcript.
func TestWorkflowAgentsBookOnInvokingTurn(t *testing.T) {
	live := withLiveLedger(t)

	b := newTestBackend(&bytes.Buffer{})
	b.hookInstallID = "install-a"
	lg := newCCLedger(b, "cap/c1", "cap", nil)
	b.ledger.Store(lg)
	b.turnMu.Lock()
	b.turnRowID = "cap/c1@1"
	b.turnMu.Unlock()

	dir := filepath.Join(t.TempDir(), "subagents", "workflows", "wf_0bd537f1-cdd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	agentLine := func(id, model string, out, w5m int) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":"%s","message":{"id":%q,"model":%q,"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":0}}}}`+"\n",
			time.Now().UTC().Format(time.RFC3339Nano), id, model, out, w5m, w5m)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("agent-a1.jsonl", agentLine("msg_a1", "claude-sonnet-5", 40, 1000))
	write("agent-a1.meta.json", `{"agentType":"workflow-subagent"}`)
	write("journal.jsonl", agentLine("msg_journal", "claude-sonnet-5", 1, 1)) // not an agent: never booked

	// The Workflow tool resolves at launch; its PostToolUse carries the run.
	resp, _ := json.Marshal(map[string]string{"status": "async_launched", "taskId": "wd6wda9q7",
		"taskType": "local_workflow", "runId": "wf_0bd537f1-cdd", "transcriptDir": dir})
	stdout, _ := json.Marshal(hookScriptOutput{HookEvent: "PostToolUse", InstallID: "install-a",
		ToolUseID: "toolu_wf", ToolName: "Workflow", ToolResponse: string(resp)})
	env, _ := json.Marshal(hookResponseEnvelope{HookEvent: "PostToolUse", Stdout: string(stdout)})
	b.handleHookResponse(env)
	started, _ := json.Marshal(TaskEvent{Subtype: "task_started", TaskID: "wd6wda9q7", ToolUseID: "toolu_wf", TaskType: "local_workflow"})
	b.OnSystem("task_started", started)

	// The invoking turn ends while the run carries on; a result lands mid-run.
	b.turnMu.Lock()
	b.turnRowID = "cap/c1@2"
	b.turnMu.Unlock()
	sonnet := ModelUsage{InputTokens: 3, OutputTokens: 40, CacheCreationInputTokens: 1000}
	b.OnResult(&ResultMessage{Type: "result", ModelUsage: map[string]ModelUsage{"claude-sonnet-5": sonnet}})
	write("agent-a2.jsonl", agentLine("msg_a2", "claude-opus-5", 70, 2000))

	done, _ := json.Marshal(TaskEvent{Subtype: "task_notification", Status: "completed", TaskID: "wd6wda9q7", ToolUseID: "toolu_wf"})
	b.OnSystem("task_notification", done)
	b.OnResult(&ResultMessage{Type: "result", ModelUsage: map[string]ModelUsage{"claude-sonnet-5": sonnet,
		"claude-opus-5": {InputTokens: 3, OutputTokens: 70, CacheCreationInputTokens: 2000}}})
	b.subagentTails().stopAll()
	lg.close()

	rows, err := live.Calls(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want the two agents' calls and nothing else (no remainder, no journal line)", len(rows))
	}
	want := map[string]int{"claude-sonnet-5": 1000, "claude-opus-5": 2000}
	for _, r := range rows {
		if r.Kind != accounting.KindCall {
			t.Errorf("%s row kind=%s turn=%q: the run's spend fell into a remainder", r.Model, r.Kind, r.TurnID)
			continue
		}
		if r.TurnID != "cap/c1@1" || r.Actor == "" || r.Count(modelinfo.ClassCacheWrite5m) != want[r.Model] {
			t.Errorf("%s = turn %q actor %q 5m %d, want the invoking turn cap/c1@1, a workflow actor, %d 5m writes",
				r.Model, r.TurnID, r.Actor, r.Count(modelinfo.ClassCacheWrite5m), want[r.Model])
		}
	}
}

// armingClock is a clock.Fake that reports the duration of every timer armed
// on it, so a test knows flush is already waiting before it moves time.
type armingClock struct {
	*clock.Fake
	armed chan time.Duration
}

func (c *armingClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	tm := c.Fake.AfterFunc(d, f)
	c.armed <- d
	return tm
}

// stalledFlush is one flush against an adapter blocked mid-turn: the adapter
// goroutine is parked on its next clock read until release, and flush's own
// wait runs on clk, so the test moves time instead of sitting through it
// (#2538: the two tests below used to take 3s and 6s of real time).
type stalledFlush struct {
	clk     *armingClock
	wait    time.Duration // what flush armed its give-up timer for
	done    chan struct{} // closed when flush returns
	release func()        // unparks the adapter
	mu      sync.Mutex
	warn    bool
}

// startStalledFlush starts flush against a stalled adapter and returns once
// flush has armed its timer. Not parallel: the warn hook is process-global.
func startStalledFlush(t *testing.T) *stalledFlush {
	t.Helper()
	p := &stalledFlush{
		clk:  &armingClock{Fake: clock.NewFake(), armed: make(chan time.Duration, 1)},
		done: make(chan struct{}),
	}
	log.SetWarnHook(func(_ log.Level, _, msg string) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if msg == "ledger: adapter did not pass a turn's barrier in time" {
			p.warn = true
		}
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	b := newTestBackend(&bytes.Buffer{})
	parked := make(chan struct{}) // closed once the adapter is blocked
	gate := make(chan struct{})
	var releaseOnce sync.Once
	p.release = func() { releaseOnce.Do(func() { close(gate) }) }
	var stalled atomic.Bool
	lg := &ccLedger{
		book:     newCCBook(accounting.Live(), b.logger(), "cap/c1", "cap", time.Now(), nil),
		clk:      p.clk,
		events:   make(chan ccEvent, 8192),
		done:     make(chan struct{}),
		tailStop: make(chan struct{}),
		tailDone: make(chan struct{}),
		tailPoke: make(chan struct{}, 1),
	}
	lg.book.now = func() time.Time {
		if stalled.CompareAndSwap(false, true) {
			close(parked)
			<-gate
		}
		return time.Now()
	}
	go lg.run()
	t.Cleanup(lg.close)
	t.Cleanup(p.release) // LIFO: unpark the adapter before closing it

	// Park the adapter before the barrier is queued, so flush can only end
	// on the timer or after release, never by the adapter answering first.
	select {
	case <-parked:
	case <-time.After(10 * time.Second): // hang guard only
		t.Fatal("adapter never read its clock: cannot stall it")
	}
	go func() { defer close(p.done); lg.flush() }()
	select {
	case p.wait = <-p.clk.armed:
	case <-time.After(10 * time.Second): // hang guard only
		t.Fatal("flush never armed its give-up timer")
	}
	return p
}

func (p *stalledFlush) returned() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *stalledFlush) awaitReturn(t *testing.T) (warned bool) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second): // hang guard only
		t.Fatal("flush did not return")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.warn
}

// TestLedgerFlushToleratesA3sAdapterStall is #2500: an adapter that answers
// the barrier 3s late is still waited for, with no WARN.
func TestLedgerFlushToleratesA3sAdapterStall(t *testing.T) {
	p := startStalledFlush(t)
	if p.wait != 5*time.Second {
		t.Errorf("flush waits %v for the adapter, want 5s (#2500: 2s was too tight)", p.wait)
	}
	p.clk.Advance(3 * time.Second)
	if p.returned() {
		t.Fatal("flush returned while the adapter was still stalled, before it could have answered")
	}
	p.release()
	if p.awaitReturn(t) {
		t.Error("flush WARNed; a 3s stall is inside the 5s wait")
	}
}

// TestLedgerFlushGivesUpAfter5sAdapterStall is the fail-arm of #2500: an
// adapter still stalled when the 5s wait runs out makes flush give up with
// the WARN, without waiting for the adapter.
func TestLedgerFlushGivesUpAfter5sAdapterStall(t *testing.T) {
	p := startStalledFlush(t)
	if p.wait != 5*time.Second {
		t.Errorf("flush gives up after %v, want 5s", p.wait)
	}
	p.clk.Advance(5 * time.Second) // the adapter stays parked throughout
	if !p.awaitReturn(t) {
		t.Error("flush returned with no WARN; the adapter never passed the barrier")
	}
}

// TestLedgerFlushPassesWhenTheTranscriptRetractsACall is #2492 through the
// real main tail: CC 2.1.307+ retries a reply that had stop_reason tool_use but
// no parsable tool call. The failed attempt was streamed (so named) but never
// reaches the transcript; the retry prompt CC writes instead lists its frame
// uuid in retractedMessageUuids. The tail reads that record, so a turn's flush
// does not wait for the attempt. Not parallel: it sets the bound.
func TestLedgerFlushPassesWhenTheTranscriptRetractsACall(t *testing.T) {
	orig := ccBarrierBound
	ccBarrierBound = time.Minute // far past the deadline below: waiting it out fails
	t.Cleanup(func() { ccBarrierBound = orig })
	b := newTestBackend(&bytes.Buffer{})
	lg := newCCLedger(b, "cap/c1", "cap", nil)
	t.Cleanup(lg.close)
	transcript := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lg.startMainTail(transcript, 0)
	lg.enqueue(ccEvent{kind: ccNamed, id: "msg_bad", uuid: "u-bad", turn: "T1"})
	lg.enqueue(ccEvent{kind: ccNamed, id: "msg_good", uuid: "u-good", turn: "T1"})
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"user","isMeta":true,"uuid":"u-meta","retractedMessageUuids":["u-bad"],"message":{"role":"user","content":[{"type":"text","text":"The previous response failed to produce a valid tool call. Please retry the tool call now."}]}}` + "\n" +
		`{"type":"assistant","uuid":"u-good","timestamp":"2026-09-29T10:00:00Z","message":{"id":"msg_good","model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n")
	_ = f.Close()
	lg.poke()
	done := make(chan struct{})
	go func() { lg.flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("flush waited for a call the transcript retracted")
	}
}

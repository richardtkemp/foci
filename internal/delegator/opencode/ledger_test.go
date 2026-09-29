package opencode

import (
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
	"foci/internal/sqlite"
)

// liveLedger opens a ledger as the live one for the test and returns its path
// for direct reads, and the alarms it raises. (The recorded model, glm-5.3,
// is priced by a live config override, not the committed table, so booking it
// here alarms invModelNotInTable.) Not for parallel tests.
func liveLedger(t *testing.T) (*[]accounting.Alarm, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	var alarms []accounting.Alarm
	l, _, err := accounting.Open(path, accounting.Options{OnAlarm: func(a accounting.Alarm) {
		alarms = append(alarms, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })
	return &alarms, path
}

// ledgerBackend is a handler-test Backend on foci session "arnix/c1" with an
// open turn "arnix/c1@1", recorded in the ledger as the agent layer records it.
func ledgerBackend(t *testing.T) *Backend {
	t.Helper()
	b := newHandlerTestBackend(t)
	b.agentID = "arnix"
	b.startOpts.SessionKey = "arnix/c1"
	b.beginTurn(&delegator.TurnEvents{TurnID: "arnix/c1@1"})
	if err := accounting.RecordTurn(accounting.Turn{TurnID: "arnix/c1@1", Session: "arnix/c1", AgentID: "arnix",
		Backend: accounting.BackendOpencode, Source: accounting.SourceUser, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return b
}

// recordedMessage is a real opencode message.updated event (recorded from an
// arnix session on glm-5.3, 2026-09-29): 305 reasoning tokens beside 447 of
// output, and tokens.total equal to the sum of the five counts.
func recordedMessage(t *testing.T) (rawEvent, Message) {
	t.Helper()
	data, err := os.ReadFile("testdata/message_updated_reasoning.json")
	if err != nil {
		t.Fatal(err)
	}
	var ev rawEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	var p eventMessageUpdated
	if err := json.Unmarshal(ev.Properties, &p); err != nil {
		t.Fatal(err)
	}
	return ev, p.Info
}

type bookedCall struct {
	key, turn, actor, kind string
	tokens                 map[string]int
	cost                   sql.NullFloat64
}

func readBooked(t *testing.T, path string) []bookedCall {
	t.Helper()
	db, err := sqlite.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT c.id, c.call_key, c.turn_id, c.actor, c.kind, cc.cost_usd
		FROM api_calls c JOIN call_costs cc ON cc.id = c.id ORDER BY c.id`)
	if err != nil {
		t.Fatal(err)
	}
	var out []bookedCall
	var ids []int64
	for rows.Next() {
		var id int64
		var c bookedCall
		if err := rows.Scan(&id, &c.key, &c.turn, &c.actor, &c.kind, &c.cost); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		out = append(out, c)
	}
	_ = rows.Close()
	for i, id := range ids {
		out[i].tokens = map[string]int{}
		tr, err := db.Query(`SELECT class, count FROM call_tokens WHERE call_id = ?`, id)
		if err != nil {
			t.Fatal(err)
		}
		for tr.Next() {
			var c string
			var n int
			_ = tr.Scan(&c, &n)
			out[i].tokens[c] = n
		}
		_ = tr.Close()
	}
	return out
}

// TestMessageIsOneCallBookedWhenComplete is the opencode adapter (T17): a
// recorded message.updated books exactly one call — keyed by the message id,
// on the open turn, its counts in disjoint classes that add up to opencode's
// own total — once time.completed is set. A streaming update before that
// books nothing, and a replay books nothing more. opencode's own figure is a
// per-call backend report.
func TestMessageIsOneCallBookedWhenComplete(t *testing.T) {
	_, path := liveLedger(t)
	b := ledgerBackend(t)
	ev, msg := recordedMessage(t)

	streaming := msg
	streaming.Time.Completed = 0
	b.onMessageUpdated(streaming)
	if got := readBooked(t, path); len(got) != 0 {
		t.Fatalf("a streaming update booked %+v", got)
	}
	b.handleEvent(ev)
	b.handleEvent(ev)

	got := readBooked(t, path)
	if len(got) != 1 {
		t.Fatalf("calls = %d, want one per message", len(got))
	}
	c := got[0]
	if c.key != msg.ID || c.turn != "arnix/c1@1" || c.actor != "" || c.kind != accounting.KindCall {
		t.Errorf("call = %+v, want message %s on the open turn", c, msg.ID)
	}
	want := map[string]int{"input": 2248, "output": 447, "reasoning": 305, "cache_read": 46528}
	sum := 0
	for class, n := range want {
		if c.tokens[class] != n {
			t.Errorf("%s = %d, want %d", class, c.tokens[class], n)
		}
		sum += c.tokens[class]
	}
	if sum != 49528 {
		t.Errorf("classes sum to %d, want opencode's tokens.total 49528 — they must be disjoint", sum)
	}
	db, err := sqlite.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	var grain string
	if err := db.QueryRow(`SELECT COUNT(*), MAX(grain) FROM backend_reports WHERE scope_key = ?`, msg.ID).Scan(&n, &grain); err != nil ||
		n != 1 || grain != accounting.GrainPerCall {
		t.Errorf("reports = %d %q (%v), want one per-call report", n, grain, err)
	}
}

// TestReasoningIsBilledAtTheOutputRate is T25: opencode bills reasoning ON
// TOP of output, at the output rate (#2112 P0-d), so the ledger's price of a
// message equals opencode's own figure for it. Before the switch foci dropped
// reasoning and under-priced every reasoning model. (The recorded messages
// are on a zero-cost coding plan, so the model here is a priced one and the
// reported cost is opencode's formula over the recorded counts.)
func TestReasoningIsBilledAtTheOutputRate(t *testing.T) {
	alarms, path := liveLedger(t)
	b := ledgerBackend(t)
	_, msg := recordedMessage(t)
	msg.ModelID, msg.ProviderID = "claude-opus-5", "anthropic"
	at := time.UnixMilli(msg.Time.Completed)
	rm, ok := modelinfo.ResolveRateModel(msg.ModelID, at)
	if !ok {
		t.Fatal("claude-opus-5 has no rates")
	}
	r, _ := modelinfo.RatesAsOf(rm, at)
	tk := msg.Tokens
	msg.Cost = (float64(tk.Input)*r[modelinfo.ClassInput] + float64(tk.Output+tk.Reasoning)*r[modelinfo.ClassOutput] +
		float64(tk.Cache.Read)*r[modelinfo.ClassCacheRead]) / 1e6

	b.onMessageUpdated(msg)

	got := readBooked(t, path)
	if len(got) != 1 || !got[0].cost.Valid {
		t.Fatalf("calls = %+v, want one priced call", got)
	}
	if d := got[0].cost.Float64 - msg.Cost; math.Abs(d) > 1e-12 {
		t.Errorf("ledger price $%.9f, opencode's $%.9f: off by $%.9f — reasoning is billed at the output rate",
			got[0].cost.Float64, msg.Cost, d)
	}
	if len(*alarms) != 0 {
		t.Errorf("alarms: %+v", *alarms)
	}
}

// TestSubagentMessagesAreItsCalls: a child session's completed message,
// rerouted to the parent, is booked as the subagent's call (actor = the task
// tool's callID) on the parent's open turn, and never touches the parent's
// context fill. The subscriber reroutes a child's message.updated even when no
// task callID names it, as the unnamed subagent.
func TestSubagentMessagesAreItsCalls(t *testing.T) {
	_, path := liveLedger(t)
	b := ledgerBackend(t)
	ev, msg := recordedMessage(t)
	ev.childCallID = "call_task_1"
	b.handleEvent(ev)

	got := readBooked(t, path)
	if len(got) != 1 || got[0].actor != "call_task_1" || got[0].turn != "arnix/c1@1" || got[0].key != msg.ID {
		t.Fatalf("calls = %+v, want the subagent's call on the parent's turn", got)
	}
	b.mu.Lock()
	fill := b.lastUsage
	b.mu.Unlock()
	if fill != nil {
		t.Errorf("a subagent's message set the parent's context fill: %+v", fill)
	}

	s := &Server{sessions: map[string]*Backend{"ses_parent": {sessionID: "ses_parent", events: make(chan rawEvent, 1)}},
		childToParent: map[string]string{"ses_child": "ses_parent"}}
	props, _ := json.Marshal(eventMessageUpdated{Info: Message{ID: "m1", SessionID: "ses_child", Role: "assistant"}})
	s.route(rawEvent{Type: EventMessageUpdated, Properties: props})
	select {
	case routed := <-s.sessions["ses_parent"].events:
		if routed.childCallID != accounting.UnnamedSubagent {
			t.Errorf("routed childCallID = %q, want %q", routed.childCallID, accounting.UnnamedSubagent)
		}
	default:
		t.Error("a child's message.updated was not routed to its parent")
	}
}

// TestCompactionMessageTurns: a compaction's summary message is booked as a
// compaction call — on the open turn when opencode compacts inside one, and on
// a compaction turn of its own, closed at once, when it runs between turns (an
// operator's /compact).
func TestCompactionMessageTurns(t *testing.T) {
	_, path := liveLedger(t)
	b := ledgerBackend(t)
	_, msg := recordedMessage(t)
	msg.Mode, msg.Summary = "compaction", true
	b.onMessageUpdated(msg)

	b.turnMu.Lock()
	b.turnActive, b.turnEvents = false, nil
	b.turnMu.Unlock()
	idle := msg
	idle.ID = "msg_idle_compaction"
	b.onMessageUpdated(idle)

	got := readBooked(t, path)
	if len(got) != 2 || got[0].kind != accounting.KindCompaction || got[1].kind != accounting.KindCompaction {
		t.Fatalf("calls = %+v, want two compaction calls", got)
	}
	if got[0].turn != "arnix/c1@1" {
		t.Errorf("in-turn compaction on %q, want the open turn", got[0].turn)
	}
	db, err := sqlite.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var source string
	var closed sql.NullString
	if err := db.QueryRow(`SELECT source, activity_closed_at FROM turns WHERE turn_id = ?`, got[1].turn).Scan(&source, &closed); err != nil ||
		source != accounting.SourceCompaction || !closed.Valid {
		t.Errorf("idle compaction's turn %q: source %q closed %v (%v), want a closed compaction turn", got[1].turn, source, closed, err)
	}
}

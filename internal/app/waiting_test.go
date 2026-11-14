package app

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/clock"
	"foci/internal/config"
	"foci/internal/fap"
)

// Tests for the waiting-on-other value (#3112, waiting.go) and the seq on the
// message wake push.

// fakeWaitInputs stands in for an agent's stay-busy inputs.
type fakeWaitInputs struct {
	mu         sync.Mutex
	threshold  time.Duration
	asyncSince map[string]time.Time
	held       map[string]bool
}

func newFakeWaitInputs() *fakeWaitInputs {
	return &fakeWaitInputs{threshold: time.Hour, asyncSince: map[string]time.Time{}, held: map[string]bool{}}
}

func (f *fakeWaitInputs) StayBusyWaitThreshold() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.threshold
}

func (f *fakeWaitInputs) AsyncPendingSince(sk string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.asyncSince[sk]
	return t, ok
}

func (f *fakeWaitInputs) UsageHeldInput(sk string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[sk]
}

// waitFixture is a hub on a fake clock with fake agent inputs and a fake wake
// source for agent "ag".
type waitFixture struct {
	h     *Hub
	clk   *clock.Fake
	in    *fakeWaitInputs
	wakes func() []fap.PendingReminder
	mu    sync.Mutex
}

func newWaitFixture(t *testing.T) *waitFixture {
	t.Helper()
	f := &waitFixture{h: newTestHub(), clk: clock.NewFake(), in: newFakeWaitInputs()}
	f.h.waitClock = f.clk
	f.h.waitInputsFor = func(string) waitInputs { return f.in }
	registerBareAgent(f.h, "ag")
	reminderMu.Lock()
	reminderSources["ag"] = func(string) []fap.PendingReminder {
		f.mu.Lock()
		fn := f.wakes
		f.mu.Unlock()
		if fn == nil {
			return []fap.PendingReminder{}
		}
		return fn()
	}
	reminderMu.Unlock()
	t.Cleanup(func() {
		reminderMu.Lock()
		delete(reminderSources, "ag")
		reminderMu.Unlock()
	})
	return f
}

func (f *waitFixture) setWakes(fn func() []fap.PendingReminder) {
	f.mu.Lock()
	f.wakes = fn
	f.mu.Unlock()
}

// bind registers a conversation of agent "ag" with one connected client that
// advertised featureWaitingOnOther (unless old), wired as ensureBinding wires
// it.
func (f *waitFixture) bind(convID string, old bool) (*convBinding, *wsClient) {
	c := fakeClientForHub(f.h)
	b := withIdent(&convBinding{convID: convID, clients: map[*wsClient]struct{}{c: {}}, seen: map[string]struct{}{}}, "ag", "ag/"+convID)
	if !old {
		b.features = featureSet([]string{featureWaitingOnOther})
	}
	b.onActivityChange = f.h.refreshWaiting
	f.h.convs[convID] = b
	f.h.bySession[b.key()] = b
	return b, c
}

// waitingValues returns the value of every WaitingOnOther frame queued on c.
func waitingValues(t *testing.T, c *wsClient) []bool {
	t.Helper()
	var out []bool
	for _, d := range drain(t, c) {
		if d.t != fap.TypeWaitingOnOther {
			continue
		}
		v, ok := d.d["waitingOnOther"].(bool)
		if !ok {
			t.Fatalf("waitingOnOther frame without a bool: %v", d.d)
		}
		out = append(out, v)
	}
	return out
}

func rosterValue(t *testing.T, h *Hub, convID string) (bool, bool) {
	t.Helper()
	raw, err := json.Marshal(h.agentRoster())
	if err != nil {
		t.Fatal(err)
	}
	var agents []struct {
		Conversations []map[string]any `json:"conversations"`
	}
	if err := json.Unmarshal(raw, &agents); err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		for _, c := range a.Conversations {
			if c["id"] == convID {
				v, present := c["waitingOnOther"].(bool)
				return v, present
			}
		}
	}
	t.Fatalf("conversation %s not in roster", convID)
	return false, false
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// H3: each row alone turns the value TRUE, and FALSE once it ends; the client
// receives exactly [true, false].
func TestWaitingOnOther_EachRowAlone(t *testing.T) {
	type row struct {
		name  string
		start func(f *waitFixture, b *convBinding)
		end   func(f *waitFixture, b *convBinding)
	}
	ms := func(f *waitFixture, d time.Duration) int64 { return f.clk.Now().Add(d).UnixMilli() }
	rows := []row{
		{"turn in flight",
			func(_ *waitFixture, b *convBinding) { b.setTurnActivity(fap.ActivityKindThinking) },
			func(_ *waitFixture, b *convBinding) { b.setTurnActivity(fap.ActivityKindIdle) }},
		{"warming",
			func(_ *waitFixture, b *convBinding) { b.setTurnActivity(fap.ActivityKindWarming) },
			func(_ *waitFixture, b *convBinding) { b.setTurnActivity(fap.ActivityKindIdle) }},
		{"rate-limit retry",
			func(_ *waitFixture, b *convBinding) { b.setRetrying("retrying in 30s") },
			func(_ *waitFixture, b *convBinding) { b.setTurnActivity(fap.ActivityKindIdle) }},
		{"Agent-tool subagent",
			func(_ *waitFixture, b *convBinding) {
				b.setSubagents("research", []fap.RunningSubagent{{ID: "toolu_a", Description: "research", Kind: "agent"}})
			},
			func(_ *waitFixture, b *convBinding) { b.setSubagents("", nil) }},
		{"background spawn",
			func(_ *waitFixture, b *convBinding) {
				b.addSpawn(fap.RunningSubagent{ID: "sp1", Description: "spawn", Kind: "agent", SubagentType: "spawn"}, "")
			},
			func(_ *waitFixture, b *convBinding) { b.removeSpawn("sp1") }},
		{"waiting for another agent",
			func(_ *waitFixture, b *convBinding) { b.setWaitingDetail("scout") },
			func(_ *waitFixture, b *convBinding) { b.setWaitingDetail("") }},
		{"5-minute-old background command",
			func(f *waitFixture, b *convBinding) {
				b.setSubagents("make", []fap.RunningSubagent{{ID: "cmd1", Description: "make", Kind: "command", Command: "make", StartedMs: ms(f, -5*time.Minute)}})
			},
			func(_ *waitFixture, b *convBinding) { b.setSubagents("", nil) }},
		{"async result started 5 minutes ago",
			func(f *waitFixture, b *convBinding) {
				f.in.mu.Lock()
				f.in.asyncSince[b.key()] = f.clk.Now().Add(-5 * time.Minute)
				f.in.mu.Unlock()
				f.h.refreshWaiting(b)
			},
			func(f *waitFixture, b *convBinding) {
				f.in.mu.Lock()
				delete(f.in.asyncSince, b.key())
				f.in.mu.Unlock()
				f.h.refreshWaiting(b)
			}},
		{"wake due in 30 minutes",
			func(f *waitFixture, _ *convBinding) {
				due := ms(f, 30*time.Minute)
				f.setWakes(func() []fap.PendingReminder {
					return []fap.PendingReminder{{ID: 1, Text: "check", DueAtMs: due, State: "pending"}}
				})
				f.h.pushReminders("ag")
			},
			func(f *waitFixture, _ *convBinding) {
				f.setWakes(nil)
				f.h.pushReminders("ag")
			}},
		{"message held by the usage hold",
			func(f *waitFixture, b *convBinding) {
				f.in.mu.Lock()
				f.in.held[b.key()] = true
				f.in.mu.Unlock()
				f.h.refreshWaiting(b)
			},
			func(f *waitFixture, b *convBinding) {
				f.in.mu.Lock()
				delete(f.in.held, b.key())
				f.in.mu.Unlock()
				f.h.refreshWaiting(b)
			}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newWaitFixture(t)
			b, c := f.bind("c1", false)
			if v, present := rosterValue(t, f.h, "c1"); !present || v {
				t.Fatalf("idle roster waitingOnOther = %v (present %v), want an explicit false", v, present)
			}
			r.start(f, b)
			if got := waitingValues(t, c); !equalBools(got, []bool{true}) {
				t.Fatalf("after start: frames %v, want [true]", got)
			}
			r.end(f, b)
			if got := waitingValues(t, c); !equalBools(got, []bool{false}) {
				t.Fatalf("after end: frames %v, want [false]", got)
			}
		})
	}
}

// H3 depth row: a depth root is TRUE while one of its leaves has a turn in
// flight, and FALSE once the leaf ends (Q38).
func TestWaitingOnOther_DepthRootFollowsLeaf(t *testing.T) {
	f := newWaitFixture(t)
	_, rootClient := f.bind("root", false)
	leaf, _ := f.bind("leaf", false)
	leaf.depthRoot = "root"

	leaf.setTurnActivity(fap.ActivityKindThinking)
	if got := waitingValues(t, rootClient); !equalBools(got, []bool{true}) {
		t.Fatalf("root frames while the leaf works = %v, want [true]", got)
	}
	if v, _ := rosterValue(t, f.h, "root"); !v {
		t.Fatal("roster: root must be TRUE while its leaf works")
	}
	leaf.setTurnActivity(fap.ActivityKindIdle)
	if got := waitingValues(t, rootClient); !equalBools(got, []bool{false}) {
		t.Fatalf("root frames after the leaf ends = %v, want [false]", got)
	}
}

// H4: threshold edges at 1h on the fake clock; spawns and subagents count at
// any age.
func TestWaitingOnOther_ThresholdEdges(t *testing.T) {
	cases := []struct {
		name string
		set  func(f *waitFixture, b *convBinding)
		want bool
	}{
		{"command 59 min old", func(f *waitFixture, b *convBinding) {
			b.setSubagents("x", []fap.RunningSubagent{{ID: "c", Kind: "command", StartedMs: f.clk.Now().Add(-59 * time.Minute).UnixMilli()}})
		}, true},
		{"command 61 min old", func(f *waitFixture, b *convBinding) {
			b.setSubagents("x", []fap.RunningSubagent{{ID: "c", Kind: "command", StartedMs: f.clk.Now().Add(-61 * time.Minute).UnixMilli()}})
		}, false},
		{"async 59 min old", func(f *waitFixture, b *convBinding) {
			f.in.asyncSince[b.key()] = f.clk.Now().Add(-59 * time.Minute)
		}, true},
		{"async 61 min old", func(f *waitFixture, b *convBinding) {
			f.in.asyncSince[b.key()] = f.clk.Now().Add(-61 * time.Minute)
		}, false},
		{"wake due in 59 min", func(f *waitFixture, _ *convBinding) {
			due := f.clk.Now().Add(59 * time.Minute).UnixMilli()
			f.setWakes(func() []fap.PendingReminder { return []fap.PendingReminder{{ID: 1, DueAtMs: due, State: "pending"}} })
		}, true},
		{"wake due in 61 min", func(f *waitFixture, _ *convBinding) {
			due := f.clk.Now().Add(61 * time.Minute).UnixMilli()
			f.setWakes(func() []fap.PendingReminder { return []fap.PendingReminder{{ID: 1, DueAtMs: due, State: "pending"}} })
		}, false},
		{"held wake due in 59 min", func(f *waitFixture, _ *convBinding) {
			due := f.clk.Now().Add(59 * time.Minute).UnixMilli()
			f.setWakes(func() []fap.PendingReminder { return []fap.PendingReminder{{ID: 1, DueAtMs: due, State: "held"}} })
		}, true},
		{"spawn running 3 hours", func(f *waitFixture, b *convBinding) {
			b.addSpawn(fap.RunningSubagent{ID: "sp", Kind: "agent", SubagentType: "spawn", StartedMs: f.clk.Now().Add(-3 * time.Hour).UnixMilli()}, "")
		}, true},
		{"subagent running 3 hours", func(f *waitFixture, b *convBinding) {
			b.setSubagents("x", []fap.RunningSubagent{{ID: "a", Kind: "agent", StartedMs: f.clk.Now().Add(-3 * time.Hour).UnixMilli()}})
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.clk.Advance(24 * time.Hour) // keep every start time after the epoch
			b, _ := f.bind("c1", false)
			tc.set(f, b)
			if got := f.h.waitingOnOther(b, f.clk.Now()); got != tc.want {
				t.Fatalf("waitingOnOther = %v, want %v", got, tc.want)
			}
		})
	}
}

// H5: nothing else counts. An idle chat with unread messages, an open ask and
// a last agent message ending in "?" is waiting for the user.
func TestWaitingOnOther_UnreadAskQuestionMarkStayFalse(t *testing.T) {
	f := newWaitFixture(t)
	b, c := f.bind("c1", false)
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m1", Role: "agent", Text: "Shall I deploy it?"})
	b.send(fap.Interactive{ConversationID: "c1", PromptID: "p1", Text: "Deploy?", Choices: []fap.Choice{{Label: "Yes", Data: "y"}}})
	b.setTurnActivity(fap.ActivityKindThinking)
	b.setTurnActivity(fap.ActivityKindIdle)
	f.h.refreshWaitingAll()
	if got := waitingValues(t, c); !equalBools(got, []bool{true, false}) {
		t.Fatalf("frames = %v, want [true false] (the turn, then waiting for the user)", got)
	}
	if v, _ := rosterValue(t, f.h, "c1"); v {
		t.Fatal("roster value must be FALSE for an idle chat with unread, an ask and a question")
	}
}

// H6: a change caused only by time passing reaches a connected client within
// one simulated minute.
func TestWaitingOnOther_TimeBasedChangeIsSent(t *testing.T) {
	t.Run("command ages past the threshold", func(t *testing.T) {
		f := newWaitFixture(t)
		f.clk.Advance(24 * time.Hour)
		b, c := f.bind("c1", false)
		started := f.clk.Now().Add(-50 * time.Minute)
		b.setSubagents("make", []fap.RunningSubagent{{ID: "cmd", Kind: "command", StartedMs: started.UnixMilli()}})
		if got := waitingValues(t, c); !equalBools(got, []bool{true}) {
			t.Fatalf("frames = %v, want [true]", got)
		}
		f.h.startWaitingSweep()
		crossed := started.Add(time.Hour)
		var at time.Time
		for i := 0; i < 22 && at.IsZero(); i++ { // 11 minutes in 30 s steps
			f.clk.Advance(30 * time.Second)
			if got := waitingValues(t, c); len(got) > 0 {
				if !equalBools(got, []bool{false}) {
					t.Fatalf("frames = %v, want [false]", got)
				}
				at = f.clk.Now()
			}
		}
		if at.IsZero() {
			t.Fatal("no change sent within 11 simulated minutes")
		}
		if late := at.Sub(crossed); late > time.Minute {
			t.Fatalf("change sent %s after the threshold passed, want within 1m", late)
		}
	})
	t.Run("wake comes within the threshold", func(t *testing.T) {
		f := newWaitFixture(t)
		b, c := f.bind("c1", false)
		due := f.clk.Now().Add(70 * time.Minute)
		f.setWakes(func() []fap.PendingReminder {
			return []fap.PendingReminder{{ID: 1, DueAtMs: due.UnixMilli(), State: "pending"}}
		})
		f.h.refreshWaiting(b)
		if got := waitingValues(t, c); len(got) != 0 {
			t.Fatalf("a wake 70 min away sent %v, want nothing (FALSE)", got)
		}
		f.h.startWaitingSweep()
		crossed := due.Add(-time.Hour)
		var at time.Time
		for i := 0; i < 22 && at.IsZero(); i++ {
			f.clk.Advance(30 * time.Second)
			if got := waitingValues(t, c); len(got) > 0 {
				if !equalBools(got, []bool{true}) {
					t.Fatalf("frames = %v, want [true]", got)
				}
				at = f.clk.Now()
			}
		}
		if at.IsZero() {
			t.Fatal("no change sent within 11 simulated minutes")
		}
		if late := at.Sub(crossed); late > time.Minute {
			t.Fatalf("change sent %s after the wake came within the threshold, want within 1m", late)
		}
	})
}

// H7: a client connecting later gets the current value at hello: TRUE for a
// working chat, FALSE for an idle one.
func TestWaitingOnOther_HelloCarriesCurrentValue(t *testing.T) {
	f := newWaitFixture(t)
	busy, _ := f.bind("busy", false)
	f.bind("idle", false)
	busy.setTurnActivity(fap.ActivityKindThinking)
	if v, present := rosterValue(t, f.h, "busy"); !present || !v {
		t.Fatalf("busy chat roster = %v (present %v), want true", v, present)
	}
	if v, present := rosterValue(t, f.h, "idle"); !present || v {
		t.Fatalf("idle chat roster = %v (present %v), want an explicit false", v, present)
	}
}

// H8: the threshold is per agent, read live. A 15-minute-old command is FALSE
// for an agent at 10m and TRUE for one at the global 1h; changing the live
// value flips the second, and the sweep sends the change with no restart.
func TestWaitingOnOther_PerAgentThresholdLive(t *testing.T) {
	h := newTestHub()
	clk := clock.NewFake()
	clk.Advance(24 * time.Hour)
	h.waitClock = clk

	global := &config.Config{}
	global.Behavior.StayBusyWaitThreshold = config.Ptr("1h")
	snap := func(agentVal string) *config.ResolvedAgentConfig {
		acfg := config.AgentConfig{ID: "x"}
		if agentVal != "" {
			acfg.Behavior.StayBusyWaitThreshold = config.Ptr(agentVal)
		}
		return config.Resolve(global, acfg)
	}
	var mu sync.Mutex
	shortSnap, longSnap := snap("10m"), snap("")
	short := &agent.Agent{LiveConfigFn: func() *config.ResolvedAgentConfig { return shortSnap }}
	long := &agent.Agent{LiveConfigFn: func() *config.ResolvedAgentConfig { mu.Lock(); defer mu.Unlock(); return longSnap }}
	for id, ag := range map[string]*agent.Agent{"short": short, "long": long} {
		h.agents[id] = &appConn{hub: h, agentID: id, agentRef: ag}
		h.agentOrder = append(h.agentOrder, id)
	}

	started := clk.Now().Add(-15 * time.Minute).UnixMilli()
	mk := func(agentID string) (*convBinding, *wsClient) {
		c := fakeClientForHub(h)
		b := withIdent(&convBinding{convID: agentID + "-c", clients: map[*wsClient]struct{}{c: {}}, seen: map[string]struct{}{}}, agentID, agentID+"/c")
		b.features = featureSet([]string{featureWaitingOnOther})
		b.onActivityChange = h.refreshWaiting
		h.convs[b.convID] = b
		h.bySession[b.key()] = b
		b.setSubagents("make", []fap.RunningSubagent{{ID: "cmd", Kind: "command", StartedMs: started}})
		return b, c
	}
	_, shortC := mk("short")
	_, longC := mk("long")
	if got := waitingValues(t, shortC); len(got) != 0 {
		t.Fatalf("agent at 10m: frames %v, want none (a 15-min command is FALSE)", got)
	}
	if got := waitingValues(t, longC); !equalBools(got, []bool{true}) {
		t.Fatalf("agent at the global 1h: frames %v, want [true]", got)
	}

	mu.Lock()
	longSnap = snap("10m")
	mu.Unlock()
	h.startWaitingSweep()
	clk.Advance(waitingSweepInterval)
	if got := waitingValues(t, longC); !equalBools(got, []bool{false}) {
		t.Fatalf("after the live change to 10m: frames %v, want [false]", got)
	}
}

// H9 (characterisation): the Activity frame for a chat whose only work is a
// 3-hour-old background command still lists the command, and activityIdle is
// false: the threshold is a stay-busy rule, not a display rule.
func TestWaitingOnOther_ActivityFrameUnchangedForOldCommand(t *testing.T) {
	f := newWaitFixture(t)
	f.clk.Advance(24 * time.Hour)
	b, c := f.bind("c1", false)
	started := f.clk.Now().Add(-3 * time.Hour).UnixMilli()
	b.setSubagents("make", []fap.RunningSubagent{{ID: "cmd", Description: "make", Kind: "command", Command: "make test", StartedMs: started}})
	var acts []map[string]any
	for _, d := range drain(t, c) {
		if d.t == fap.TypeActivity {
			acts = append(acts, d.d)
		}
	}
	if len(acts) != 1 {
		t.Fatalf("activity frames = %v, want one", acts)
	}
	got, _ := json.Marshal(acts[0])
	want := `{"conversationId":"c1","detail":"make","kind":"subagents","subagents":[{"command":"make test","description":"make","id":"cmd","kind":"command","startedMs":` + jsonInt(started) + `}]}`
	if string(got) != want {
		t.Fatalf("activity frame = %s\nwant           %s", got, want)
	}
	if b.activityIdle() {
		t.Fatal("activityIdle must stay false while an old command runs")
	}
	if f.h.waitingOnOther(b, f.clk.Now()) {
		t.Fatal("waitingOnOther must be FALSE for a 3-hour-old command alone")
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// H10: a client whose hello did not advertise the capability (every client
// before #3112) receives no WaitingOnOther frame, and the roster it gets
// decodes with the fap types.
func TestWaitingOnOther_OldClientGetsNoNewFrame(t *testing.T) {
	f := newWaitFixture(t)
	b, c := f.bind("c1", true)
	b.setTurnActivity(fap.ActivityKindThinking)
	b.setTurnActivity(fap.ActivityKindIdle)
	for _, d := range drain(t, c) {
		if d.t == fap.TypeWaitingOnOther {
			t.Fatalf("an old client received %v", d)
		}
	}
	raw, err := json.Marshal(fap.HelloServer{Version: fap.ProtocolVersion, Agents: f.h.agentRoster()})
	if err != nil {
		t.Fatal(err)
	}
	var back fap.HelloServer
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("roster does not decode: %v", err)
	}
}

// H1: the message wake carries the seq of the frame that caused it. A second
// message inside the coalescing window sends no push; after the window the
// next push carries its own seq; the user's own message sends none.
func TestMessageWake_CarriesSeq(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	tk := newPushTokens()
	tk.set("phone", "tok-phone")
	h.tokens = tk
	p, rec := newRecordingPusher(t, tk)
	h.pusher = p
	b := withIdent(&convBinding{convID: "c1", seen: map[string]struct{}{}, notifyOffline: h.pushNotify}, "ag", "ag/c1")
	h.convs["c1"] = b
	h.bySession[b.key()] = b

	b.seq = 6
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m7", Role: "agent", Text: "first"})
	rec.waitFor(t, "tok-phone", 1)
	got := rec.pushes()[0].Data
	want := map[string]string{"type": "message", "conversationId": "c1", "preview": "first", "agentId": "ag",
		"agentName": got["agentName"], "sessionKey": "ag/c1", "sessionTitle": got["sessionTitle"], "seq": "7"}
	if len(got) != len(want) {
		t.Fatalf("push data = %v, want keys %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("push data[%q] = %q, want %q (data %v)", k, got[k], v, got)
		}
	}

	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m8", Role: "agent", Text: "second"}) // seq 8, inside the window
	time.Sleep(50 * time.Millisecond)
	if n := rec.count("tok-phone"); n != 1 {
		t.Fatalf("pushes after a message inside the window = %d, want 1", n)
	}

	p.mu.Lock()
	for k := range p.lastPush {
		p.lastPush[k] = time.Now().Add(-2 * time.Hour) // the window has passed
	}
	p.mu.Unlock()
	b.seq = 11
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "m12", Role: "agent", Text: "later"})
	rec.waitFor(t, "tok-phone", 2)
	if s := rec.pushes()[1].Data["seq"]; s != "12" {
		t.Fatalf("push after the window: seq = %q, want 12", s)
	}

	p.mu.Lock()
	for k := range p.lastPush {
		p.lastPush[k] = time.Now().Add(-2 * time.Hour)
	}
	p.mu.Unlock()
	b.send(fap.ServerMessage{ConversationID: "c1", MessageID: "u1", Role: "user", Text: "mine"})
	time.Sleep(50 * time.Millisecond)
	if n := rec.count("tok-phone"); n != 2 {
		t.Fatalf("pushes after the user's own message = %d, want 2", n)
	}
}

// H2 (characterisation): the read and askgw wake payloads are unchanged.
func TestWakePayloads_ReadAndAskgwUnchanged(t *testing.T) {
	read := readWakeData("c1", "m9")
	if len(read) != 3 || read["type"] != "read" || read["conversationId"] != "c1" || read["messageId"] != "m9" {
		t.Fatalf("read wake data = %v", read)
	}
	android := readWakeAndroid("c1")
	if len(android) != 2 || android["priority"] != "normal" || android["collapse_key"] != "read:c1" {
		t.Fatalf("read wake android = %v", android)
	}
	ask := askgwWakeData("p1", "Deploy?")
	if len(ask) != 3 || ask["type"] != "askgw" || ask["promptId"] != "p1" || ask["preview"] != "Deploy?" {
		t.Fatalf("askgw wake data = %v", ask)
	}
}

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
)

// holdSpawnAgent is a spawn agent with a scriptable usage hold: its first
// turn blocks until its context ends (a step in flight when the hold
// starts); later turns finish at once.
type holdSpawnAgent struct {
	mu      sync.Mutex
	calls   [][]string
	turns   map[*context.CancelCauseFunc]*bool
	holding bool
	lift    chan struct{}
	paused  map[string]bool
	warm    bool
	started chan struct{}
}

func newHoldSpawnAgent(warm bool) *holdSpawnAgent {
	return &holdSpawnAgent{turns: map[*context.CancelCauseFunc]*bool{}, paused: map[string]bool{}, warm: warm, started: make(chan struct{}, 4)}
}

func (h *holdSpawnAgent) HandleMessage(ctx context.Context, _ string, texts []string, _ []platform.Attachment) error {
	h.mu.Lock()
	h.calls = append(h.calls, texts)
	first := len(h.calls) == 1
	h.mu.Unlock()
	h.started <- struct{}{}
	if first {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (h *holdSpawnAgent) UsageHoldTurn(ctx context.Context, _, _ string) (context.Context, func() bool) {
	c, cancel := context.WithCancelCause(ctx)
	hit := false
	h.mu.Lock()
	h.turns[&cancel] = &hit
	h.mu.Unlock()
	return c, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.turns, &cancel)
		cancel(nil)
		return hit
	}
}

func (h *holdSpawnAgent) startHold() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.holding = true
	h.lift = make(chan struct{})
	for cancel, hit := range h.turns {
		*hit = true
		(*cancel)(context.Canceled)
	}
}

func (h *holdSpawnAgent) liftHold() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.holding = false
	close(h.lift)
}

func (h *holdSpawnAgent) UsageHoldActive() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.holding }

func (h *holdSpawnAgent) WaitUsageLift(ctx context.Context) bool {
	h.mu.Lock()
	lift, holding := h.lift, h.holding
	h.mu.Unlock()
	if !holding {
		return true
	}
	select {
	case <-lift:
		return true
	case <-ctx.Done():
		return false
	}
}

func (h *holdSpawnAgent) UsageHoldPause(key, _ string, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paused[key] = on
}

func (h *holdSpawnAgent) CacheExpiry(_ string, at time.Time) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.warm {
		return at.Add(time.Hour)
	}
	return time.Time{}
}

func (h *holdSpawnAgent) Calls() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string(nil), h.calls...)
}

func (h *holdSpawnAgent) anyPaused() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, on := range h.paused {
		if on {
			return true
		}
	}
	return false
}

// startHoldSpawn starts one background clone spawn on ag and
// returns its tracker and result channel.
func startHoldSpawn(t *testing.T, ag *holdSpawnAgent) (*SpawnTracker, chan string) {
	t.Helper()
	results := make(chan string, 4)
	notifier := NewAsyncNotifier(func(_, msg, _, _ string, _ *AgentOrigin) { results <- msg })
	tracker := NewSpawnTracker()
	deps := SpawnDeps{Sessions: &mockSessionBrancher{}, AgentID: "test", FallbackModel: "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic", MaxInherit: 2, Notifier: notifier, Tracker: tracker}
	tool := NewSpawner(deps, func() SpawnAgent { return ag }).SpawnTool()
	params, _ := json.Marshal(map[string]any{"name": "held", "prompt": "long task", "context": "clone"})
	if _, err := tool.Execute(WithSessionKey(context.Background(), "test/imain"), params); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	select {
	case <-ag.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the spawn's first step never started")
	}
	return tracker, results
}

// TestSpawn_UsageHoldPausesAndResumes is holdout H9: a background spawn
// running when the hold starts is stopped and paused; its parent gets no
// [SPAWN RESULT] and sees it running; a hold does not
// time it out; at lift its branch gets the resume line, and it then reports
// ONE [SPAWN RESULT].
func TestSpawn_UsageHoldPausesAndResumes(t *testing.T) {
	t.Parallel()
	ag := newHoldSpawnAgent(true)
	tracker, results := startHoldSpawn(t, ag)

	ag.startHold()
	select {
	case msg := <-results:
		t.Fatalf("a [SPAWN RESULT] arrived during the hold: %q", msg)
	case <-time.After(1500 * time.Millisecond): // the hold must not finish the spawn
	}
	if running := tracker.Running("test/imain"); len(running) != 1 {
		t.Fatalf("running spawns during the hold = %d, want the paused one", len(running))
	}
	if !ag.anyPaused() {
		t.Error("the paused spawn is not in the paused (warm) set")
	}

	ag.liftHold()
	select {
	case msg := <-results:
		if !strings.Contains(msg, "[SPAWN RESULT]") || !strings.Contains(msg, "completed") {
			t.Errorf("result = %q, want a completed [SPAWN RESULT]", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no [SPAWN RESULT] after the lift")
	}
	calls := ag.Calls()
	if len(calls) != 2 || len(calls[1]) == 0 || calls[1][0] != UsageResumeLine {
		t.Fatalf("turns = %q, want the first step then the resume line", calls)
	}
	select {
	case msg := <-results:
		t.Errorf("a second [SPAWN RESULT]: %q", msg)
	case <-time.After(200 * time.Millisecond):
	}
	if ag.anyPaused() {
		t.Error("the spawn is still in the paused set after the lift")
	}
}

// TestSpawn_UsageHoldColdNotResumed is the paused-spawn-gone-cold ruling
// (Dick 2026-11-02): at lift a paused spawn whose cache is cold gets no
// resume turn and ends with one [SPAWN RESULT] that says why.
func TestSpawn_UsageHoldColdNotResumed(t *testing.T) {
	t.Parallel()
	ag := newHoldSpawnAgent(false)
	tracker, results := startHoldSpawn(t, ag)
	ag.startHold()
	time.Sleep(100 * time.Millisecond)
	ag.liftHold()
	select {
	case msg := <-results:
		if !strings.Contains(msg, "[SPAWN RESULT]") || !strings.Contains(msg, "not resumed") || !strings.Contains(msg, "cold") {
			t.Errorf("result = %q, want a stopped-by-the-hold, gone-cold [SPAWN RESULT]", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no [SPAWN RESULT] for the cold paused spawn")
	}
	if calls := ag.Calls(); len(calls) != 1 {
		t.Errorf("turns = %q, want no resume turn", calls)
	}
	if running := tracker.Running("test/imain"); len(running) != 0 {
		t.Errorf("still running after its stop: %+v", running)
	}
}

// heldSpawnAgent is a warm cacheSpawnAgent whose account may hold.
type heldSpawnAgent struct {
	*cacheSpawnAgent
	mu      sync.Mutex
	refusal string
}

func (a *heldSpawnAgent) UsageHoldRefusal() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refusal
}
func (a *heldSpawnAgent) hold(msg string) { a.mu.Lock(); a.refusal = msg; a.mu.Unlock() }

// TestSpawnSend_RefusedDuringUsageHold is the spawn_send half of holdout H8
// and worked example K: during a hold, spawn_send to a WARM finished spawn
// (one the cold rule would resume) is refused with the hold message and
// starts no turn; after the lift the same send resumes it.
func TestSpawnSend_RefusedDuringUsageHold(t *testing.T) {
	t.Parallel()
	agent := &heldSpawnAgent{cacheSpawnAgent: &cacheSpawnAgent{scriptedSpawnAgent: newScriptedSpawnAgent(0),
		expiry: func(now time.Time) time.Time { return now.Add(50 * time.Minute) }}}
	h := newFollowupHarness(agent)
	id := h.start(t, "find the bug")
	waitStarted(t, agent.scriptedSpawnAgent, 0)
	h.result(t)

	agent.hold("the usage hold is on (only keepalives and reminders run) until Tue 3 Nov 04:00.")
	got := h.sendRefused(t, id, "now fix it")
	if !strings.Contains(got, "NOT delivered") || !strings.Contains(got, "usage hold") {
		t.Errorf("send during a hold = %q, want NOT delivered naming the usage hold", got)
	}
	time.Sleep(100 * time.Millisecond)
	if calls, _ := agent.turns(); len(calls) != 1 {
		t.Errorf("turns = %d, want 1 (no turn during the hold)", len(calls))
	}

	agent.hold("")
	if got := h.send(t, id, "now fix it"); !strings.Contains(got, "resumed") {
		t.Fatalf("send after the lift = %q, want it resumed", got)
	}
	waitStarted(t, agent.scriptedSpawnAgent, 1)
}

// TestSpawn_RefusedDuringUsageHold: a new spawn is refused during a hold with
// the hold message and starts no turn (requirement 2).
func TestSpawn_RefusedDuringUsageHold(t *testing.T) {
	t.Parallel()
	agent := &heldSpawnAgent{cacheSpawnAgent: &cacheSpawnAgent{scriptedSpawnAgent: newScriptedSpawnAgent(0),
		expiry: func(now time.Time) time.Time { return now.Add(time.Hour) }}}
	agent.hold("the usage hold is on (only keepalives and reminders run) until Tue 3 Nov 04:00.")
	tool := NewSpawner(SpawnDeps{Sessions: &mockSessionBrancher{}, AgentID: "test", MaxInherit: 2,
		Notifier: NewAsyncNotifier(func(string, string, string, string, *AgentOrigin) {}), Tracker: NewSpawnTracker()},
		func() SpawnAgent { return agent }).SpawnTool()
	params, _ := json.Marshal(map[string]any{"name": "nope", "prompt": "task", "context": "clone"})
	res, err := tool.Execute(WithSessionKey(context.Background(), "test/imain"), params)
	if err == nil {
		t.Fatalf("spawn during a hold = %q, want a refusal as Execute's error (#2849)", res.Text)
	}
	if got := err.Error(); !strings.Contains(got, "NOT started") || !strings.Contains(got, "usage hold") || !strings.Contains(got, "04:00") {
		t.Errorf("spawn during a hold = %q, want NOT started naming the hold and its lift", got)
	}
	if calls, _ := agent.turns(); len(calls) != 0 {
		t.Errorf("turns = %d, want none", len(calls))
	}
}

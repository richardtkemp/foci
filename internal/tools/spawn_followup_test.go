package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/turnevent"
)

// scriptedSpawnAgent records every turn a spawn runs. Turn i waits for
// gates[i] (when there is one) or its context's end; with drainSteer set, turn
// 0 instead waits until its turnevent.Steerer yields text, as an API turn
// drains steers at a tool boundary. Each turn emits a text block, a tool call
// and a final answer naming its index and inputs.
type scriptedSpawnAgent struct {
	mu         sync.Mutex
	calls      [][]string
	keys       []string
	gates      []chan struct{}
	started    chan int
	drainSteer bool
}

func newScriptedSpawnAgent(gates int) *scriptedSpawnAgent {
	a := &scriptedSpawnAgent{started: make(chan int, 8)}
	for i := 0; i < gates; i++ {
		a.gates = append(a.gates, make(chan struct{}))
	}
	return a
}

func (a *scriptedSpawnAgent) HandleMessage(ctx context.Context, sk string, texts []string, _ []platform.Attachment) error {
	a.mu.Lock()
	idx := len(a.calls)
	a.calls = append(a.calls, texts)
	a.keys = append(a.keys, sk)
	var gate chan struct{}
	if idx < len(a.gates) {
		gate = a.gates[idx]
	}
	a.mu.Unlock()
	a.started <- idx
	turnevent.Emit(ctx, turnevent.TextBlock{Text: fmt.Sprintf("working %d", idx), Phase: turnevent.PhaseIntermediate})
	turnevent.Emit(ctx, turnevent.ToolCall{Name: "Bash", Args: json.RawMessage(`{"command": "sleep 600"}`)})
	var steers []string
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if a.drainSteer && idx == 0 {
		st := turnevent.SteererFromContext(ctx)
		for len(steers) == 0 {
			steers = st.PendingSteers()
			if len(steers) == 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
	}
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: fmt.Sprintf("result %d: %s %s", idx, strings.Join(texts, "|"), strings.Join(steers, "|"))})
	return nil
}

func (a *scriptedSpawnAgent) turns() ([][]string, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]string(nil), a.calls...), append([]string(nil), a.keys...)
}

// steeringSpawnAgent adds SpawnSteerer: a delegated backend folding text into
// the running turn.
type steeringSpawnAgent struct {
	*scriptedSpawnAgent
	ok      bool
	steered chan [2]string
}

func (a *steeringSpawnAgent) SteerTurn(_ context.Context, sk, text string) bool {
	a.steered <- [2]string{sk, text}
	return a.ok
}

type followupHarness struct {
	spawner   *Spawner
	tracker   *SpawnTracker
	delivered chan string
	ctx       context.Context
}

func newFollowupHarness(agent SpawnAgent) *followupHarness {
	h := &followupHarness{tracker: NewSpawnTracker(), delivered: make(chan string, 8)}
	notifier := NewAsyncNotifier(func(sk, msg, _, _ string, _ *AgentOrigin) {
		h.delivered <- sk + "\n" + msg
	})
	h.spawner = NewSpawner(SpawnDeps{
		Sessions:   &mockSessionBrancher{},
		AgentID:    "test",
		MaxInherit: 2,
		Notifier:   notifier,
		Tracker:    h.tracker,
	}, func() SpawnAgent { return agent })
	h.ctx = WithSessionKey(context.Background(), "test/imain")
	return h
}

var spawnIDRe = regexp.MustCompile(`Spawn: (spawn-[0-9a-f]+)`)

func (h *followupHarness) start(t *testing.T, prompt string) string {
	t.Helper()
	params, _ := json.Marshal(map[string]string{"prompt": prompt, "context": "clone"})
	res, err := h.spawner.SpawnTool().Execute(h.ctx, params)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	m := spawnIDRe.FindStringSubmatch(res.Text)
	if m == nil {
		t.Fatalf("spawn result names no spawn id: %q", res.Text)
	}
	return m[1]
}

func (h *followupHarness) send(t *testing.T, id, message string) string {
	t.Helper()
	params, _ := json.Marshal(map[string]string{"id": id, "message": message})
	res, err := h.spawner.SendTool().Execute(h.ctx, params)
	if err != nil {
		t.Fatalf("spawn_send: %v", err)
	}
	return res.Text
}

func (h *followupHarness) status(t *testing.T, id string) string {
	t.Helper()
	params, _ := json.Marshal(map[string]string{"id": id})
	res, err := h.spawner.StatusTool().Execute(h.ctx, params)
	if err != nil {
		t.Fatalf("spawn_status: %v", err)
	}
	return res.Text
}

func (h *followupHarness) result(t *testing.T) string {
	t.Helper()
	select {
	case msg := <-h.delivered:
		if !strings.HasPrefix(msg, "test/imain\n") {
			t.Errorf("result delivered to %q, want the parent session", strings.SplitN(msg, "\n", 2)[0])
		}
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("no [SPAWN RESULT] delivered")
		return ""
	}
}

func waitStarted(t *testing.T, a *scriptedSpawnAgent, want int) {
	t.Helper()
	select {
	case got := <-a.started:
		if got != want {
			t.Fatalf("turn %d started, want turn %d", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("turn %d never started", want)
	}
}

const branch = "test/imain/b1000000000"

func TestSpawnSend_QueuedMessageRunsAsFollowUpTurn(t *testing.T) {
	// A backend that cannot fold the message into the running turn gets it as a
	// follow-up turn on the same branch, and the parent still hears ONE result:
	// the last turn's (#2523).
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	h := newFollowupHarness(agent)
	id := h.start(t, "watch the build")
	waitStarted(t, agent, 0)

	if got := h.send(t, id, "also tail the log"); !strings.Contains(got, "Queued") {
		t.Fatalf("send to a running spawn = %q, want it queued", got)
	}
	close(agent.gates[0])
	waitStarted(t, agent, 1)
	msg := h.result(t)
	if !strings.Contains(msg, "completed") || !strings.Contains(msg, "result 1: "+spawnMessagePrefix+"also tail the log") {
		t.Errorf("result = %q, want the follow-up turn's answer", msg)
	}
	calls, keys := agent.turns()
	if len(calls) != 2 || keys[0] != branch || keys[1] != branch {
		t.Errorf("turns = %v on %v, want two on the spawn's branch", calls, keys)
	}
	if r := h.tracker.Running("test/imain"); r != nil {
		t.Errorf("still running after its result: %+v", r)
	}
}

func TestSpawnSend_SteersRunningTurn(t *testing.T) {
	// A backend that can steer (Claude Code) gets the message folded into the
	// running turn: no extra turn runs.
	t.Parallel()
	agent := &steeringSpawnAgent{scriptedSpawnAgent: newScriptedSpawnAgent(1), ok: true, steered: make(chan [2]string, 1)}
	h := newFollowupHarness(agent)
	id := h.start(t, "watch the build")
	waitStarted(t, agent.scriptedSpawnAgent, 0)

	if got := h.send(t, id, "stop at the first error"); !strings.Contains(got, "Delivered") {
		t.Fatalf("send = %q, want it delivered into the running turn", got)
	}
	st := <-agent.steered
	if st[0] != branch || st[1] != spawnMessagePrefix+"stop at the first error" {
		t.Errorf("steered %q into %q, want the prefixed message into %q", st[1], st[0], branch)
	}
	close(agent.gates[0])
	if msg := h.result(t); !strings.Contains(msg, "result 0: watch the build") {
		t.Errorf("result = %q, want the one turn's answer", msg)
	}
	if calls, _ := agent.turns(); len(calls) != 1 {
		t.Errorf("turns = %v, want 1: a steered message must not run a turn of its own", calls)
	}
}

func TestSpawnSend_DeclinedSteerIsQueued(t *testing.T) {
	// SteerTurn false (no turn in flight yet) falls back to the queue.
	t.Parallel()
	agent := &steeringSpawnAgent{scriptedSpawnAgent: newScriptedSpawnAgent(1), ok: false, steered: make(chan [2]string, 1)}
	h := newFollowupHarness(agent)
	id := h.start(t, "watch")
	waitStarted(t, agent.scriptedSpawnAgent, 0)
	if got := h.send(t, id, "more"); !strings.Contains(got, "Queued") {
		t.Fatalf("send = %q, want queued after a declined steer", got)
	}
	close(agent.gates[0])
	waitStarted(t, agent.scriptedSpawnAgent, 1)
	h.result(t)
}

func TestSpawnSend_APITurnDrainsMessageAtToolBoundary(t *testing.T) {
	// An API spawn's turn takes a queued message through its Steerer at the
	// next tool boundary; it is not run again as a follow-up turn.
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	agent.drainSteer = true
	h := newFollowupHarness(agent)
	id := h.start(t, "research")
	waitStarted(t, agent, 0)
	h.send(t, id, "focus on the parser")
	if msg := h.result(t); !strings.Contains(msg, "focus on the parser") {
		t.Errorf("result = %q, want the drained message in the turn", msg)
	}
	if calls, _ := agent.turns(); len(calls) != 1 {
		t.Errorf("turns = %v, want 1", calls)
	}
}

func TestSpawnSend_ResumesFinishedSpawnAndStatusShowsActivity(t *testing.T) {
	// A finished spawn resumes on its branch with the message and reports a
	// second result; spawn_status lists it and shows its recent activity.
	t.Parallel()
	agent := newScriptedSpawnAgent(0)
	h := newFollowupHarness(agent)
	id := h.start(t, "find the bug")
	waitStarted(t, agent, 0)
	if msg := h.result(t); !strings.Contains(msg, "result 0: find the bug") || !strings.Contains(msg, id) {
		t.Fatalf("first result = %q", msg)
	}

	listParams, _ := json.Marshal(map[string]string{})
	list, err := h.spawner.StatusTool().Execute(h.ctx, listParams)
	if err != nil || !strings.Contains(list.Text, id) || !strings.Contains(list.Text, "completed") {
		t.Errorf("status list = %q (err %v), want the spawn listed as completed", list.Text, err)
	}

	if got := h.send(t, id, "now fix it"); !strings.Contains(got, "resumed") {
		t.Fatalf("send to a finished spawn = %q, want it resumed", got)
	}
	waitStarted(t, agent, 1)
	if msg := h.result(t); !strings.Contains(msg, "result 1: "+spawnMessagePrefix+"now fix it") {
		t.Errorf("second result = %q, want the resumed turn's answer", msg)
	}
	if _, keys := agent.turns(); len(keys) != 2 || keys[1] != branch {
		t.Errorf("turn keys = %v, want the resume on %q", keys, branch)
	}

	// The branch key works as the id too.
	st := h.status(t, branch)
	for _, want := range []string{"Spawn " + id + ": completed", "prompt: find the bug", "text: working 0",
		"tool: Bash {\"command\": \"sleep 600\"}", "(resumed with) now fix it", "text: result 1:"} {
		if !strings.Contains(st, want) {
			t.Errorf("status lacks %q:\n%s", want, st)
		}
	}
	// The answer a backend already logged as a text block is not logged twice.
	if n := strings.Count(st, "result 0: find the bug"); n > 1 {
		t.Errorf("first answer logged %d times:\n%s", n, st)
	}
}

func TestSpawnSend_StoppedSpawnTakesNoFollowUp(t *testing.T) {
	// /stop ends the spawn even with a message waiting; the result says the
	// message was never delivered.
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	h := newFollowupHarness(agent)
	id := h.start(t, "watch")
	waitStarted(t, agent, 0)
	h.send(t, id, "and then this")
	if n := h.tracker.Stop("test/imain"); n != 1 {
		t.Fatalf("Stop = %d", n)
	}
	msg := h.result(t)
	if !strings.Contains(msg, "stopped by the user") || !strings.Contains(msg, "1 message(s)") {
		t.Errorf("result = %q, want stopped with one undelivered message", msg)
	}
	if calls, _ := agent.turns(); len(calls) != 1 {
		t.Errorf("turns = %v, want no follow-up after a stop", calls)
	}
	if st := h.status(t, id); !strings.Contains(st, "stopped") {
		t.Errorf("status = %q, want stopped", st)
	}
}

func TestSpawn_NestedFromSpawnBranchRefused(t *testing.T) {
	// A delegated spawn's tool calls carry its branch key and no inherit
	// marker; it still may not spawn a clone of its own.
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	h := newFollowupHarness(agent)
	h.start(t, "watch")
	waitStarted(t, agent, 0)
	defer close(agent.gates[0])

	params, _ := json.Marshal(map[string]string{"prompt": "nested", "context": "clone"})
	_, err := h.spawner.SpawnTool().Execute(WithSessionKey(context.Background(), branch), params)
	if err == nil || !strings.Contains(err.Error(), "nested") {
		t.Errorf("spawn from a spawn's branch: err = %v, want nested refusal", err)
	}
}

func TestSpawnSend_UnknownSpawn(t *testing.T) {
	t.Parallel()
	h := newFollowupHarness(newScriptedSpawnAgent(0))
	params, _ := json.Marshal(map[string]string{"id": "spawn-nope", "message": "hi"})
	if _, err := h.spawner.SendTool().Execute(h.ctx, params); err == nil || !strings.Contains(err.Error(), "no spawn") {
		t.Errorf("err = %v, want no-spawn error", err)
	}
	// Another session's spawn is not this session's to steer.
	agent := newScriptedSpawnAgent(0)
	h2 := newFollowupHarness(agent)
	id := h2.start(t, "x")
	h2.result(t)
	params, _ = json.Marshal(map[string]string{"id": id, "message": "hi"})
	if _, err := h2.spawner.SendTool().Execute(WithSessionKey(context.Background(), "test/iother"), params); err == nil {
		t.Error("another session reached this session's spawn")
	}
}

func TestSpawnTracker_PrunesOldFinished(t *testing.T) {
	t.Parallel()
	tr := NewSpawnTracker()
	for i := 0; i < maxFinishedSpawns+5; i++ {
		id := fmt.Sprintf("spawn-%02d", i)
		tr.add("p", RunningSpawn{ID: id, BranchKey: "p/b" + id, Started: time.Now()}, func() {}, 0)
		if _, done, _, _, _ := tr.finishOrNext("p", id, false); !done {
			t.Fatal("finish with nothing pending must be done")
		}
	}
	if n := len(tr.List("p")); n != maxFinishedSpawns {
		t.Errorf("kept %d finished spawns, want %d", n, maxFinishedSpawns)
	}
	if _, ok := tr.Lookup("p", "spawn-00", 0); ok {
		t.Error("oldest finished spawn not pruned")
	}
}

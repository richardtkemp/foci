package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/session"
)

// send_to_spawner (#2662, Dick 2026-10-30 12:18): every spawn can message the
// session that spawned it while it runs.

// injected is one AsyncNotifier delivery.
type injected struct {
	session, msg, replyTo string
	origin                *AgentOrigin
}

func recordingNotifier() (*AsyncNotifier, chan injected) {
	ch := make(chan injected, 16)
	return NewAsyncNotifier(func(sk, msg, replyTo, _ string, origin *AgentOrigin) {
		ch <- injected{sk, msg, replyTo, origin}
	}), ch
}

// busyAgent is a spawn agent that says whether a turn runs on a session.
type busyAgent struct {
	mu   sync.Mutex
	busy bool
}

func (a *busyAgent) HandleMessage(context.Context, string, []string, []platform.Attachment) error {
	return nil
}

func (a *busyAgent) IsTurnInFlight(string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy
}

// messagingAgent runs each turn by calling send_to_spawner (through tool) on
// its own session, then answering.
type messagingAgent struct {
	tool   func() *Tool
	text   string
	mu     sync.Mutex
	result []string // each call's tool result or error
	done   chan struct{}
}

func (a *messagingAgent) HandleMessage(ctx context.Context, sk string, _ []string, _ []platform.Attachment) error {
	res, err := a.tool().Execute(WithSessionKey(ctx, sk), json.RawMessage(`{"message":"`+a.text+`"}`))
	a.mu.Lock()
	if err != nil {
		a.result = append(a.result, "error: "+err.Error())
	} else {
		a.result = append(a.result, res.Text)
	}
	a.mu.Unlock()
	if a.done != nil {
		a.done <- struct{}{}
	}
	return nil
}

func (a *messagingAgent) results() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.result...)
}

func sendToSpawner(s *Spawner, ctx context.Context, msg string) (ToolResult, error) {
	params, _ := json.Marshal(map[string]string{"message": msg})
	return s.SendToSpawnerTool().Execute(ctx, params)
}

// A background spawn's message reaches its spawner's session at once, as a
// turn of its own, before its result; the spawner is told to answer with
// spawn_send, and its reply goes to its own chat (replyTo "").
func TestSendToSpawnerFromBackgroundSpawn(t *testing.T) {
	t.Parallel()
	notifier, got := recordingNotifier()
	var s *Spawner
	agent := &messagingAgent{tool: func() *Tool { return s.SendToSpawnerTool() }, text: "halfway: found the bug"}
	cb := &characterBranches{}
	s = NewSpawner(SpawnDeps{AgentID: "msgbg", MaxInherit: 1, Notifier: notifier, Tracker: NewSpawnTracker(), CharacterBranch: cb.create},
		func() SpawnAgent { return agent })
	ctx := WithSessionKey(context.Background(), "msgbg/imain")
	if _, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"job","background":true}`)); err != nil {
		t.Fatal(err)
	}
	first := recv(t, got, "the spawn's message")
	if first.session != "msgbg/imain" || first.replyTo != "" {
		t.Fatalf("message went to %q replyTo %q, want the spawner's session, reply to its own chat", first.session, first.replyTo)
	}
	id := s.deps.Tracker.List("msgbg/imain")[0].ID
	for _, want := range []string{"MESSAGE FROM YOUR SPAWN " + id, "halfway: found the bug", "spawn_send " + id, "NOT from your human"} {
		if !strings.Contains(first.msg, want) {
			t.Errorf("message lacks %q:\n%s", want, first.msg)
		}
	}
	if first.origin == nil || first.origin.SessionKey != "msgbg/imain/bchar" || first.origin.Text != "halfway: found the bug" {
		t.Errorf("origin = %+v, want the spawn's branch and raw text (the app's chit)", first.origin)
	}
	result := recv(t, got, "the [SPAWN RESULT]")
	if !strings.Contains(result.msg, "[SPAWN RESULT]") || strings.Contains(result.msg, "[MESSAGES FROM YOUR SPAWN]") {
		t.Errorf("result = %q, want a plain [SPAWN RESULT] (the message already went)", result.msg)
	}
	if r := agent.results(); len(r) != 1 || !strings.HasPrefix(r[0], "Sent to your spawner (msgbg/imain)") {
		t.Errorf("tool said %q", r)
	}
}

// A cross-agent background spawn's message goes to the CALLER's session,
// never the target's, and says which agent the spawn runs as.
func TestSendToSpawnerFromCrossAgentBackgroundSpawn(t *testing.T) {
	t.Parallel()
	notifier, got := recordingNotifier()
	tracker := NewSpawnTracker()
	caller := NewSpawner(SpawnDeps{AgentID: "msgx-caller", MaxInherit: 1, Notifier: notifier, Tracker: tracker},
		func() SpawnAgent { return newCtxSpawnAgent() })
	var target *Spawner
	agent := &messagingAgent{tool: func() *Tool { return target.SendToSpawnerTool() }, text: "from helen"}
	cb := &characterBranches{}
	target = NewSpawner(SpawnDeps{AgentID: "msgx-target", MaxInherit: 1, Tracker: tracker, CharacterBranch: cb.create},
		func() SpawnAgent { return agent })
	registerTestSpawner(t, caller)
	registerTestSpawner(t, target)
	ctx := WithSessionKey(context.Background(), "msgx-caller/imain")
	if _, err := caller.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"job","background":true,"agent":"msgx-target"}`)); err != nil {
		t.Fatal(err)
	}
	first := recv(t, got, "the spawn's message")
	if first.session != "msgx-caller/imain" {
		t.Fatalf("message went to %q, want the caller's session", first.session)
	}
	if !strings.Contains(first.msg, "running as msgx-target") || !strings.Contains(first.msg, "from helen") {
		t.Errorf("message = %q", first.msg)
	}
	if first.origin == nil || first.origin.SessionKey != session.CrossAgentSpawnRoot("msgx-target")+"/bchar" {
		t.Errorf("origin = %+v, want the target's spawn branch", first.origin)
	}
	recv(t, got, "the [SPAWN RESULT]")
}

// A direct spawn whose spawner's turn is running (its call waits on the
// spawn) keeps the message: it heads the result, and nothing is injected.
// This is the delegated path: the batch's tool calls carry only its session
// key, mapped while it runs.
func TestSendToSpawnerFromDirectSpawnWhileCallerWaits(t *testing.T) {
	t.Parallel()
	notifier, got := recordingNotifier()
	runner := newBlockingBatchRunner()
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Tracker = NewSpawnTracker()
	s.deps.Notifier = notifier
	caller := &busyAgent{busy: true}
	s.agentFn = func() SpawnAgent { return caller }
	ctx := WithSessionKey(context.Background(), "test/c7")
	call := runSpawnAsync(s, ctx, `{"prompt":"job"}`)
	req := waitBatch(t, runner)
	if req.OnSession == nil {
		t.Fatal("a direct batch has no OnSession hook, so its tool calls cannot find their spawn")
	}
	batchKey := "test/c7/b99"
	req.OnSession(batchKey)
	bctx := WithSessionKey(context.Background(), batchKey)
	for _, m := range []string{"first note", "second note"} {
		res, err := sendToSpawner(s, bctx, m)
		if err != nil || !strings.HasPrefix(res.Text, "Kept: your spawner is waiting for your result") {
			t.Fatalf("send_to_spawner = %q, %v; want kept", res.Text, err)
		}
	}
	select {
	case m := <-got:
		t.Fatalf("injected %+v while the spawner's call waits", m)
	default:
	}
	close(runner.release)
	r := waitCall(t, call)
	if r.err != nil {
		t.Fatal(r.err)
	}
	id := s.deps.Tracker.List("test/c7")[0].ID
	want := "[MESSAGES FROM YOUR SPAWN] Spawn " + id + " sent you 2 message(s) with send_to_spawner while it ran:"
	if !strings.HasPrefix(r.res.Text, want) || !strings.HasSuffix(r.res.Text, "[ITS RESULT]\n\nreleased") ||
		strings.Index(r.res.Text, "first note") > strings.Index(r.res.Text, "second note") {
		t.Fatalf("result = %q, want the two messages, in order, heading the result", r.res.Text)
	}
	// After the spawn, its batch key is no spawn's.
	if _, err := sendToSpawner(s, bctx, "late"); err == nil || !strings.Contains(err.Error(), "not a running spawn") {
		t.Errorf("after the spawn: err = %v, want refused", err)
	}
}

// A direct spawn whose spawner has no turn running (nothing in its session
// waits on the call: a Claude Code agent that ran it in a background shell)
// sends the message at once, and says it takes no spawn_send.
func TestSendToSpawnerFromDirectSpawnCallerIdle(t *testing.T) {
	t.Parallel()
	notifier, got := recordingNotifier()
	s := NewSpawner(SpawnDeps{AgentID: "msgidle", MaxInherit: 1, Notifier: notifier, Tracker: NewSpawnTracker()},
		func() SpawnAgent { return &busyAgent{busy: false} })
	ctx := WithSessionKey(context.Background(), "msgidle/c1")
	res, err := s.runDirect(ctx, s, spawnModeRaw, "p", "", time.Minute, func(runCtx context.Context) (ToolResult, error) {
		// An API one-shot's tool calls run on the caller's session key,
		// with the spawn in their ctx.
		if r, err := sendToSpawner(s, runCtx, "early news"); err != nil || !strings.HasPrefix(r.Text, "Sent to your spawner") {
			t.Errorf("send_to_spawner = %q, %v; want sent", r.Text, err)
		}
		return TextResult("done"), nil
	})
	if err != nil || res.Text != "done" {
		t.Fatalf("result = %q, %v; want just the answer (the message already went)", res.Text, err)
	}
	m := recv(t, got, "the message")
	if m.session != "msgidle/c1" || !strings.Contains(m.msg, "early news") || !strings.Contains(m.msg, "takes no spawn_send") {
		t.Errorf("injected %+v", m)
	}
	// The spawner's own session (not a spawn) has no spawner.
	if _, err := sendToSpawner(s, ctx, "x"); err == nil || !strings.Contains(err.Error(), "not a running spawn") {
		t.Errorf("from a plain session: err = %v", err)
	}
}

// A direct spawn's kept messages head its error too (failed or stopped),
// and a message after it ended is refused.
func TestSendToSpawnerDirectFailedAndEnded(t *testing.T) {
	t.Parallel()
	s := NewSpawner(SpawnDeps{AgentID: "msgfail", MaxInherit: 1, Tracker: NewSpawnTracker()},
		func() SpawnAgent { return &busyAgent{busy: true} })
	ctx := WithSessionKey(context.Background(), "msgfail/c1")
	var linked context.Context
	_, err := s.runDirect(ctx, s, spawnModeRaw, "p", "", time.Minute, func(runCtx context.Context) (ToolResult, error) {
		linked = runCtx
		if _, err := sendToSpawner(s, runCtx, "before the crash"); err != nil {
			t.Error(err)
		}
		return ToolResult{}, context.DeadlineExceeded
	})
	if err == nil || !strings.Contains(err.Error(), "before the crash") || !strings.Contains(err.Error(), "[ITS RESULT]") {
		t.Fatalf("err = %v, want the kept message heading the error", err)
	}
	if _, err := sendToSpawner(s, linked, "too late"); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Errorf("after the end: err = %v, want refused", err)
	}
}

// While a depth fan-in holds the route (#2624), a background spawn's message
// waits and heads its [SPAWN RESULT].
func TestSendToSpawnerHeldRoute(t *testing.T) {
	t.Parallel()
	notifier, got := recordingNotifier()
	var s *Spawner
	agent := &messagingAgent{tool: func() *Tool { return s.SendToSpawnerTool() }, text: "held note"}
	cb := &characterBranches{}
	var mu sync.Mutex
	hold := true
	s = NewSpawner(SpawnDeps{AgentID: "msghold", MaxInherit: 1, Notifier: notifier, Tracker: NewSpawnTracker(), CharacterBranch: cb.create,
		Route: func(p string) SpawnRoute {
			mu.Lock()
			defer mu.Unlock()
			return SpawnRoute{Owner: p, Hold: hold}
		}},
		func() SpawnAgent { return agent })
	ctx := WithSessionKey(context.Background(), "msghold/imain")
	if _, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"job","background":true}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(s.deps.Tracker.heldResults("msghold")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the result was never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	hold = false
	mu.Unlock()
	s.ReleaseHeld()
	m := recv(t, got, "the released result")
	if !strings.Contains(m.msg, "]\n[MESSAGES FROM YOUR SPAWN]") || !strings.Contains(m.msg, "held note") || !strings.Contains(m.msg, "[ITS RESULT]\n\n[SPAWN RESULT]") {
		t.Fatalf("result = %q, want the held message heading it", m.msg)
	}
	if r := agent.results(); len(r) != 1 || !strings.HasPrefix(r[0], "Kept:") {
		t.Errorf("tool said %q, want kept", r)
	}
}

// Every spawn kind whose session can reach foci's tools has send_to_spawner:
// raw and character API one-shots, explore (API and a delegated read-only
// batch's bridge) and a delegated character batch's bridge. The spawn family
// stays out of all of them.
func TestSendToSpawnerInEverySpawnToolSet(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	s := NewSpawner(SpawnDeps{AgentID: "msgsets", Tracker: NewSpawnTracker()}, func() SpawnAgent { return nil })
	reg.Register(s.SendToSpawnerTool())
	reg.Register(s.SpawnTool())
	reg.Register(&Tool{Name: "read"})
	_, raw := spawnIsolatedToolSet(reg, spawnRawBlacklist, nil, t.TempDir(), 0o600)
	_, char := spawnToolSet(reg, spawnCharacterBlacklist)
	_, explore := spawnExploreToolSet(reg)
	for name, set := range map[string]map[string]*Tool{"raw": raw, "character": char, "explore": explore} {
		if set["send_to_spawner"] == nil {
			t.Errorf("%s spawn lacks send_to_spawner", name)
		}
		if set["spawn"] != nil {
			t.Errorf("%s spawn has spawn", name)
		}
	}
	for _, access := range []delegator.BatchAccess{delegator.BatchAccessParent, delegator.BatchAccessReadOnly} {
		sub := RegistryForAccess(reg, access)
		if sub == nil || sub.Get("send_to_spawner") == nil {
			t.Errorf("batch access %s lacks send_to_spawner", access)
		}
	}
}

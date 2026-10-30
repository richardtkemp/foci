package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/session"
)

// registerTestSpawner registers s under its agent id for the test's life.
func registerTestSpawner(t *testing.T, s *Spawner) {
	t.Helper()
	RegisterSpawner(s.deps.AgentID, s)
	t.Cleanup(func() {
		spawnersMu.Lock()
		delete(spawners, s.deps.AgentID)
		spawnersMu.Unlock()
	})
}

// blockingBatchRunner runs a batch until its ctx ends or release is closed.
type blockingBatchRunner struct {
	started chan delegator.BatchRequest
	release chan struct{}
	mu      sync.Mutex
	ctxErr  error
}

func newBlockingBatchRunner() *blockingBatchRunner {
	return &blockingBatchRunner{started: make(chan delegator.BatchRequest, 4), release: make(chan struct{})}
}

func (r *blockingBatchRunner) RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error) {
	r.started <- req
	select {
	case <-ctx.Done():
		r.mu.Lock()
		r.ctxErr = ctx.Err()
		r.mu.Unlock()
		return "", ctx.Err()
	case <-r.release:
		return "released", nil
	}
}

func waitBatch(t *testing.T, r *blockingBatchRunner) delegator.BatchRequest {
	t.Helper()
	select {
	case req := <-r.started:
		return req
	case <-time.After(10 * time.Second):
		t.Fatal("the batch never started")
		return delegator.BatchRequest{}
	}
}

type spawnCall struct {
	res ToolResult
	err error
}

func runSpawnAsync(s *Spawner, ctx context.Context, params string) chan spawnCall {
	out := make(chan spawnCall, 1)
	go func() {
		res, err := s.SpawnTool().Execute(ctx, json.RawMessage(params))
		out <- spawnCall{res, err}
	}()
	return out
}

func waitCall(t *testing.T, c chan spawnCall) spawnCall {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the spawn call never returned")
		return spawnCall{}
	}
}

// #2656 req 2: a direct (synchronous) spawn is listed by spawn_status while
// it runs, and spawn_stop stops it: its call returns as stopped.
func TestDirectSpawnListedAndStoppable(t *testing.T) {
	t.Parallel()
	runner := newBlockingBatchRunner()
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Tracker = NewSpawnTracker()
	ctx := WithSessionKey(context.Background(), "test/c1")
	call := runSpawnAsync(s, ctx, `{"prompt":"long job"}`)
	waitBatch(t, runner)

	list, err := s.StatusTool().Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.Text, "running") || !strings.Contains(list.Text, "character, direct") || !strings.Contains(list.Text, "long job") {
		t.Fatalf("spawn_status = %q, want the running direct character spawn", list.Text)
	}
	snaps := s.deps.Tracker.List("test/c1")
	if len(snaps) != 1 {
		t.Fatalf("tracked %d spawns, want 1", len(snaps))
	}
	if _, err := s.SendTool().Execute(ctx, json.RawMessage(`{"id":"`+snaps[0].ID+`","message":"hi"}`)); err == nil || !strings.Contains(err.Error(), "no messages") {
		t.Errorf("spawn_send to a direct spawn: err = %v, want refused", err)
	}
	out, err := s.StopTool().Execute(ctx, json.RawMessage(`{"id":"`+snaps[0].ID+`"}`))
	if err != nil || !strings.Contains(out.Text, "Stopping spawn") {
		t.Fatalf("spawn_stop = %q, %v", out.Text, err)
	}
	r := waitCall(t, call)
	if r.err == nil || !strings.Contains(r.err.Error(), "stopped by its parent agent") {
		t.Fatalf("stopped spawn's call = %q, %v; want stopped", r.res.Text, r.err)
	}
	if sn := s.deps.Tracker.List("test/c1"); sn[0].Running || sn[0].Outcome != "stopped" {
		t.Errorf("after stop: %+v, want finished as stopped", sn[0])
	}
	// /stop reaches direct spawns too (it stops every spawn of the session).
	call = runSpawnAsync(s, ctx, `{"prompt":"another"}`)
	waitBatch(t, runner)
	if n := s.deps.Tracker.Stop("test/c1"); n != 1 {
		t.Fatalf("/stop stopped %d, want 1", n)
	}
	if r := waitCall(t, call); r.err == nil || !strings.Contains(r.err.Error(), "stopped by the user") {
		t.Fatalf("after /stop: %v, want stopped by the user", r.err)
	}
}

// #2656 req 1: a direct spawn whose call ends (its caller is gone) stops;
// it does not work on.
func TestDirectSpawnStopsWithItsCall(t *testing.T) {
	t.Parallel()
	runner := newBlockingBatchRunner()
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Tracker = NewSpawnTracker()
	ctx, cancel := context.WithCancel(WithSessionKey(context.Background(), "test/c1"))
	call := runSpawnAsync(s, ctx, `{"prompt":"p","timeout":3600}`)
	waitBatch(t, runner)
	cancel() // what the exec bridge does when foci-call dies
	if r := waitCall(t, call); r.err == nil {
		t.Fatal("the spawn's call returned no error after its ctx ended")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if !errors.Is(runner.ctxErr, context.Canceled) {
		t.Fatalf("batch ctx err = %v, want it cancelled with the call", runner.ctxErr)
	}
}

// spawn_stop all stops every running spawn of the caller.
func TestSpawnStopAll(t *testing.T) {
	t.Parallel()
	runner := newBlockingBatchRunner()
	s := newDelegatedSpawner(runner, allBatchCaps)
	s.deps.Tracker = NewSpawnTracker()
	ctx := WithSessionKey(context.Background(), "test/c1")
	a := runSpawnAsync(s, ctx, `{"prompt":"a"}`)
	waitBatch(t, runner)
	b := runSpawnAsync(s, ctx, `{"prompt":"b"}`)
	waitBatch(t, runner)
	out, err := s.StopTool().Execute(ctx, json.RawMessage(`{"id":"all"}`))
	if err != nil || !strings.Contains(out.Text, "Stopping 2") {
		t.Fatalf("spawn_stop all = %q, %v", out.Text, err)
	}
	for _, c := range []chan spawnCall{a, b} {
		if r := waitCall(t, c); r.err == nil {
			t.Error("a spawn kept running after spawn_stop all")
		}
	}
}

// recv waits for one value on c, failing the test after a hang guard.
func recv[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// --- cross-agent (#2662 part 1) ---

func newCrossPair(t *testing.T, name string) (caller *Spawner, callerRunner *recordingBatchRunner, target *Spawner, targetRunner *blockingBatchRunner) {
	t.Helper()
	callerRunner = &recordingBatchRunner{}
	caller = newDelegatedSpawner(callerRunner, allBatchCaps)
	caller.deps.AgentID = name + "-caller"
	caller.deps.Tracker = NewSpawnTracker()
	targetRunner = newBlockingBatchRunner()
	target = newDelegatedSpawner(targetRunner, allBatchCaps)
	target.deps.AgentID = name + "-target"
	target.deps.Tracker = caller.deps.Tracker
	registerTestSpawner(t, caller)
	registerTestSpawner(t, target)
	return caller, callerRunner, target, targetRunner
}

// A direct character spawn with agent=<id> runs as that agent: on its batch
// runner, with its own composed prompt, under its cross-agent root (none of
// its chats), tracked by the caller, its result returned to the caller.
func TestCrossAgentDirectSpawnRunsAsTarget(t *testing.T) {
	t.Parallel()
	caller, callerRunner, target, targetRunner := newCrossPair(t, "xdirect")
	ctx := WithSessionKey(context.Background(), caller.deps.AgentID+"/c1")
	call := runSpawnAsync(caller, ctx, `{"prompt":"do it for me","agent":"`+target.deps.AgentID+`"}`)
	req := waitBatch(t, targetRunner)
	if req.OwnerSessionKey != session.CrossAgentSpawnRoot(target.deps.AgentID) || !req.AgentPrompt || req.Access != delegator.BatchAccessParent {
		t.Fatalf("target batch = %+v, want a character batch under the target's cross-agent root", req)
	}
	snaps := caller.deps.Tracker.List(caller.deps.AgentID + "/c1")
	if len(snaps) != 1 || snaps[0].Agent != target.deps.AgentID || !snaps[0].Running {
		t.Fatalf("caller tracks %+v, want one running spawn as the target", snaps)
	}
	// The batch's session maps to the caller while it runs, so its
	// send_to_chat reaches the caller's chat (Dick 2026-10-30 12:06).
	if req.OnSession == nil {
		t.Fatal("the cross-agent batch has no OnSession hook")
	}
	batchKey := session.CrossAgentSpawnRoot(target.deps.AgentID) + "/b1"
	req.OnSession(batchKey)
	if got, ok := crossAgentChatSession(context.Background(), batchKey); !ok || got != caller.deps.AgentID+"/c1" {
		t.Fatalf("batch's chat session = %q, %v; want the caller's", got, ok)
	}
	close(targetRunner.release)
	r := waitCall(t, call)
	if r.err != nil || r.res.Text != "released" {
		t.Fatalf("cross-agent spawn = %q, %v; want the target's answer", r.res.Text, r.err)
	}
	if got, ok := crossAgentChatSession(context.Background(), batchKey); ok {
		t.Errorf("after the spawn its batch still maps to %q", got)
	}
	if len(callerRunner.reqs) != 0 {
		t.Errorf("the caller's own runner ran %d batches, want none", len(callerRunner.reqs))
	}
}

// Dick 2026-10-30: naming an agent implies character mode, and an explicit
// other mode with it is refused; an
// unknown agent is a clear error; a spawn cannot start one.
func TestCrossAgentSpawnRefusals(t *testing.T) {
	t.Parallel()
	caller, _, target, _ := newCrossPair(t, "xrefuse")
	ctx := WithSessionKey(context.Background(), caller.deps.AgentID+"/c1")
	for _, mode := range []string{"raw", "explore", "clone"} {
		_, err := caller.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p","context":"`+mode+`","agent":"`+target.deps.AgentID+`"}`))
		if err == nil || !strings.Contains(err.Error(), "always runs in character mode") {
			t.Errorf("%s as another agent: err = %v, want refused", mode, err)
		}
	}
	_, err := caller.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p","agent":"nobody-xrefuse"}`))
	if err == nil || !strings.Contains(err.Error(), `unknown agent "nobody-xrefuse"`) || !strings.Contains(err.Error(), target.deps.AgentID) {
		t.Errorf("unknown agent: err = %v, want a clear error naming the agents", err)
	}
	for name, c := range map[string]context.Context{
		"inherit":     WithSpawnInherit(ctx),
		"character":   WithSpawnCharacter(ctx),
		"cross-agent": WithSessionKey(context.Background(), session.CrossAgentSpawnRoot(caller.deps.AgentID)+"/b1"),
	} {
		if _, err := caller.SpawnTool().Execute(c, json.RawMessage(`{"prompt":"p","agent":"`+target.deps.AgentID+`"}`)); err == nil || !strings.Contains(err.Error(), "cannot start a spawn of another agent") {
			t.Errorf("from a %s spawn: err = %v, want refused", name, err)
		}
	}
	// Naming yourself is an ordinary spawn.
	if _, err := caller.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p","context":"raw","agent":"`+caller.deps.AgentID+`"}`)); err != nil {
		t.Errorf("agent = own id: %v, want an ordinary spawn", err)
	}
}

// A background cross-agent character spawn: the TARGET's agent runs its
// turns on a fresh branch of the target's cross-agent root; the caller's
// session gets the [SPAWN RESULT]; the record keeps the target, so
// spawn_send resumes it on the target's agent.
func TestCrossAgentBackgroundSpawn(t *testing.T) {
	t.Parallel()
	callerAgent, targetAgent := newCtxSpawnAgent(), newCtxSpawnAgent()
	h, _ := newCharacterHarness(callerAgent)
	h.spawner.deps.AgentID = "xbg-caller"
	h.ctx = WithSessionKey(context.Background(), "xbg-caller/imain")
	cb := &characterBranches{}
	target := NewSpawner(SpawnDeps{AgentID: "xbg-target", MaxInherit: 1, CharacterBranch: cb.create, Tracker: h.tracker},
		func() SpawnAgent { return targetAgent })
	registerTestSpawner(t, h.spawner)
	registerTestSpawner(t, target)

	res, err := h.spawner.SpawnTool().Execute(h.ctx, json.RawMessage(`{"prompt":"long task","background":true,"agent":"xbg-target"}`))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if !strings.Contains(res.Text, "Character spawn as xbg-target started") {
		t.Fatalf("spawn result = %q", res.Text)
	}
	root := session.CrossAgentSpawnRoot("xbg-target")
	if len(cb.created) != 1 || cb.created[0] != root+"|" {
		t.Fatalf("target CharacterBranch calls = %v, want one off %s", cb.created, root)
	}
	msg := recv(t, h.delivered, "the [SPAWN RESULT]")
	if !strings.HasPrefix(msg, "xbg-caller/imain\n") || !strings.Contains(msg, "[SPAWN RESULT]") {
		t.Fatalf("delivered %q, want the [SPAWN RESULT] in the caller's session", msg)
	}
	targetAgent.mu.Lock()
	if len(targetAgent.keys) != 1 || targetAgent.keys[0] != root+"/bchar" {
		t.Fatalf("target ran %v, want one run on its cross-agent branch", targetAgent.keys)
	}
	targetAgent.mu.Unlock()
	callerAgent.mu.Lock()
	if len(callerAgent.keys) != 0 {
		t.Fatalf("the caller's agent ran %v, want nothing", callerAgent.keys)
	}
	callerAgent.mu.Unlock()

	snaps := h.tracker.List("xbg-caller/imain")
	if len(snaps) != 1 || snaps[0].Agent != "xbg-target" {
		t.Fatalf("tracked %+v, want the spawn as xbg-target", snaps)
	}
	if rec := h.tracker.spawns["xbg-caller/imain"][snaps[0].ID].info; rec.Agent != "xbg-target" {
		t.Fatalf("tracked info %+v", rec)
	}
	params, _ := json.Marshal(map[string]string{"id": snaps[0].ID, "message": "more"})
	if _, err := h.spawner.SendTool().Execute(h.ctx, params); err != nil {
		t.Fatal(err)
	}
	recv(t, h.delivered, "the resumed run's [SPAWN RESULT]")
	targetAgent.mu.Lock()
	defer targetAgent.mu.Unlock()
	if len(targetAgent.keys) != 2 {
		t.Fatalf("resume ran on %v, want the target's agent again", targetAgent.keys)
	}
	// The target resolves the branch's owner to the caller (prompts go there).
	if owner, ok := target.BranchOwner(root + "/bchar"); !ok || owner != "xbg-caller/imain" {
		t.Errorf("target BranchOwner = %q, %v; want the caller's session", owner, ok)
	}
	// And so does the branch's send_to_chat.
	if got, ok := crossAgentChatSession(context.Background(), root+"/bchar"); !ok || got != "xbg-caller/imain" {
		t.Errorf("branch's chat session = %q, %v; want the caller's", got, ok)
	}
}

// A cross-agent spawn's send_to_chat goes to its CALLER's chat, headed with
// who sent it (Dick 2026-10-30 12:06); with no caller known it is refused,
// never sent to the target's chats. Its send_to_session and remind are
// refused (remind: it could never reach anyone).
func TestCrossAgentSpawnSessionMessages(t *testing.T) {
	t.Parallel()
	send := func(ctx context.Context) (*mockSender, []string, error) {
		mock := &mockSender{sessionKey: "xmsg-caller/c4242"}
		var asked []string
		tool := NewSendToChatTool(func(k string) platform.Sender { asked = append(asked, k); return mock }, nil, nil, nil)
		_, err := tool.Execute(ctx, json.RawMessage(`{"text":"hi from helen"}`))
		return mock, asked, err
	}
	// A direct API spawn: the caller rides in its ctx.
	mock, asked, err := send(crossAgentContext(context.Background(), "xmsg-target", "xmsg-caller/c4242"))
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "xmsg-caller/c4242" {
		t.Fatalf("sender asked for %v, want the caller's session", asked)
	}
	if len(mock.chatTextCalls) != 1 || mock.chatTextCalls[0].chatID != 4242 ||
		mock.chatTextCalls[0].value != "[[ message from xmsg-target's spawn ]]\nhi from helen" {
		t.Fatalf("sent %+v, want the headed text in the caller's chat 4242", mock.chatTextCalls)
	}
	// A direct delegated batch: through crossAgentBatches.
	batchKey := session.CrossAgentSpawnRoot("xmsg-target") + "/b77"
	crossAgentBatches.Store(batchKey, "xmsg-caller/c4242")
	_, asked, err = send(WithSessionKey(context.Background(), batchKey))
	crossAgentBatches.Delete(batchKey)
	if err != nil || len(asked) != 1 || asked[0] != "xmsg-caller/c4242" {
		t.Fatalf("batch send asked %v, err %v; want the caller's session", asked, err)
	}
	// No caller known: refused, and no sender is even looked up.
	_, asked, err = send(WithSessionKey(context.Background(), batchKey))
	if err == nil || len(asked) != 0 {
		t.Fatalf("callerless send: asked %v, err %v; want refused", asked, err)
	}
	// An ordinary session is unchanged.
	if _, asked, err = send(WithSessionKey(context.Background(), "xmsg-target/c9")); err != nil || len(asked) != 1 || asked[0] != "xmsg-target/c9" {
		t.Fatalf("ordinary send asked %v, err %v", asked, err)
	}

	ctx := WithSessionKey(context.Background(), batchKey)
	if err := refuseCrossAgentSpawnMessage(ctx, "send_to_session"); err == nil {
		t.Error("send_to_session allowed from a cross-agent spawn session")
	}
	if err := refuseCrossAgentSpawnMessage(WithSessionKey(context.Background(), "helen/c1/b1"), "send_to_session"); err != nil {
		t.Errorf("an ordinary branch refused: %v", err)
	}
	remind := testRemindTool(t)
	for _, params := range []string{`{"text":"t","when":"1h"}`, `{"text":"t","when":"1h","wake":true}`, `{"list":true}`} {
		if _, err := remind.Execute(ctx, json.RawMessage(params)); err == nil || !strings.Contains(err.Error(), "could never reach") {
			t.Errorf("remind %s from a cross-agent spawn: err = %v, want refused", params, err)
		}
	}
	if _, err := remind.Execute(WithSessionKey(context.Background(), "helen/c1"), json.RawMessage(`{"text":"t","when":"1h"}`)); err != nil {
		t.Errorf("remind from an ordinary session: %v", err)
	}
}

// --- allowance (#2662 part 2) ---

func usageOf(five, week int) *delegator.UsageInfo {
	reset := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	return &delegator.UsageInfo{
		Backend:  "Claude Code",
		FiveHour: delegator.UsageWindow{Percent: five, ResetsAt: reset},
		SevenDay: delegator.UsageWindow{Percent: week, ResetsAt: reset.Add(72 * time.Hour)},
	}
}

func TestLowWindows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		info *delegator.UsageInfo
		want []string
	}{
		{"plenty", usageOf(50, 80), nil},
		{"exactly 10 left", usageOf(90, 0), nil},
		{"5-hour low", usageOf(91, 10), []string{"5-hour"}},
		{"weekly low", usageOf(5, 95), []string{"weekly"}},
		{"both", usageOf(99, 100), []string{"5-hour", "weekly"}},
		{"unreported", &delegator.UsageInfo{}, nil},
		{"codex", &delegator.UsageInfo{Allowance: &delegator.Allowance{Buckets: []delegator.AllowanceBucket{
			{Windows: []delegator.AllowanceWindow{{Label: "5h", Percent: 20, ResetsAt: time.Now()}, {Label: "7d", Percent: 93, ResetsAt: time.Now()}}},
		}}}, []string{"7d"}},
	}
	for _, c := range cases {
		var got []string
		for _, w := range lowWindows(c.info, 10) {
			got = append(got, w.Name)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: low = %v, want %v", c.name, got, c.want)
		}
	}
}

func fixedUsage(r UsageRead) UsageReader { return func(context.Context) UsageRead { return r } }

// A spawn on a backend under its reserve is refused with the window, % left,
// reset time and how to override; use_reserve runs it; no source, a failed
// read and a pending read all run (the latter two say so).
func TestSpawnAllowanceCheck(t *testing.T) {
	t.Parallel()
	ctx := WithSessionKey(context.Background(), "test/c1")
	newS := func(u UsageReader) (*Spawner, *recordingBatchRunner) {
		runner := &recordingBatchRunner{}
		s := newDelegatedSpawner(runner, allBatchCaps)
		s.deps.Tracker = NewSpawnTracker()
		s.deps.Usage = u
		return s, runner
	}

	s, runner := newS(fixedUsage(UsageRead{Info: usageOf(94, 20)}))
	_, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p"}`))
	if err == nil {
		t.Fatal("a spawn on a backend with 6% left ran")
	}
	for _, want := range []string{"running low", "5-hour window has 6% left", "resets", "--use-reserve"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if len(runner.reqs) != 0 {
		t.Fatal("the refused spawn ran a batch")
	}
	if _, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p","use_reserve":true}`)); err != nil || len(runner.reqs) != 1 {
		t.Fatalf("use_reserve: %v, batches %d; want it run", err, len(runner.reqs))
	}

	s, runner = newS(nil)
	if res, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p"}`)); err != nil || strings.Contains(res.Text, "not checked") {
		t.Errorf("no usage source: %q, %v; want it run with no note", res.Text, err)
	}

	s, _ = newS(fixedUsage(UsageRead{Err: errors.New("boom")}))
	if res, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p"}`)); err != nil || !strings.Contains(res.Text, "not checked: the usage read failed: boom") {
		t.Errorf("failed read: %q, %v; want it run with a note", res.Text, err)
	}

	s, _ = newS(fixedUsage(UsageRead{Pending: true}))
	if res, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p"}`)); err != nil || !strings.Contains(res.Text, "still running") {
		t.Errorf("pending read: %q, %v; want it run with a note", res.Text, err)
	}

	// The reserve is the caller's setting; 0 turns the check off.
	s, _ = newS(fixedUsage(UsageRead{Info: usageOf(100, 100)}))
	s.deps.ReservePercent = func() int { return 0 }
	if _, err := s.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p"}`)); err != nil {
		t.Errorf("reserve 0: %v, want no check", err)
	}
}

// A cross-agent spawn checks the TARGET's backend, not the caller's.
func TestCrossAgentSpawnChecksTargetAllowance(t *testing.T) {
	t.Parallel()
	caller, _, target, _ := newCrossPair(t, "xallow")
	caller.deps.Usage = fixedUsage(UsageRead{Info: usageOf(10, 10)})
	target.deps.Usage = fixedUsage(UsageRead{Info: usageOf(10, 97)})
	ctx := WithSessionKey(context.Background(), caller.deps.AgentID+"/c1")
	_, err := caller.SpawnTool().Execute(ctx, json.RawMessage(`{"prompt":"p","agent":"`+target.deps.AgentID+`"}`))
	if err == nil || !strings.Contains(err.Error(), target.deps.AgentID+"'s Claude Code allowance") || !strings.Contains(err.Error(), "weekly window has 3% left") {
		t.Fatalf("err = %v, want refused on the target's weekly window", err)
	}
}

// The cache reads once per usageFresh, serves a stale reading while it
// refreshes, and never waits longer than its wait for a first reading.
func TestUsageCache(t *testing.T) {
	t.Parallel()
	c := NewUsageCache()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	c.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	c.wait = time.Second

	calls := make(chan struct{}, 8)
	gate := make(chan struct{})
	var n int
	read := c.Register("k", func(context.Context) (*delegator.UsageInfo, error) {
		calls <- struct{}{}
		<-gate
		mu.Lock()
		n++
		v := n
		mu.Unlock()
		return usageOf(v, 0), nil
	}, nil)

	// First read: nothing cached, the read is slow → pending, within the wait.
	if r := read(context.Background()); !r.Pending {
		t.Fatalf("first read = %+v, want pending", r)
	}
	<-calls
	gate <- struct{}{}
	// Wait for the refresh to land.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r := read(context.Background()); !r.Pending && r.Info != nil {
			if r.Info.FiveHour.Percent != 1 {
				t.Fatalf("reading = %d, want the first", r.Info.FiveHour.Percent)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refresh never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Fresh (under 20 min, longer than the longest scheduled interval): no new read.
	advance(15 * time.Minute)
	if r := read(context.Background()); r.Info.FiveHour.Percent != 1 {
		t.Fatal("fresh reading changed")
	}
	c.mu.Lock()
	started := c.entries["k"].running != nil
	c.mu.Unlock()
	if started {
		t.Fatal("a fresh reading triggered a read")
	}
	// Stale: served at once while one refresh runs.
	advance(10 * time.Minute)
	if r := read(context.Background()); r.Pending || r.Info.FiveHour.Percent != 1 {
		t.Fatalf("stale read = %+v, want the old reading at once", r)
	}
	<-calls
	if r := read(context.Background()); r.Info.FiveHour.Percent != 1 {
		t.Fatal("second stale read did not serve the old reading")
	}
	select {
	case <-calls:
		t.Fatal("two refreshes ran at once")
	default:
	}
	close(gate)
}

// A failed read is reported, and is not retried by every spawn: the next
// on-demand read waits usageRetry. A good reading survives a later failure.
func TestUsageCacheFailedRead(t *testing.T) {
	t.Parallel()
	c := NewUsageCache()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	c.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	c.wait = 10 * time.Second
	var calls int
	fail := true
	read := c.Register("k", func(context.Context) (*delegator.UsageInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fail {
			return nil, errors.New("boom")
		}
		return usageOf(5, 5), nil
	}, nil)
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }

	if r := read(context.Background()); r.Err == nil {
		t.Fatalf("read = %+v, want the failure", r)
	}
	if r := read(context.Background()); r.Err == nil || count() != 1 {
		t.Fatalf("second read = %+v after %d reads; want the failure again, no new read", r, count())
	}
	advance(usageRetry)
	mu.Lock()
	fail = false
	mu.Unlock()
	if r := read(context.Background()); r.Err != nil || r.Info == nil || count() != 2 {
		t.Fatalf("read after the retry gap = %+v after %d reads; want a new good reading", r, count())
	}
	// A later failure keeps the good reading while it is stale-usable.
	mu.Lock()
	fail = true
	mu.Unlock()
	advance(usageFresh + time.Minute)
	if r := read(context.Background()); r.Info == nil || r.Err != nil {
		t.Fatalf("stale read = %+v, want the good reading", r)
	}
}

// Dick 2026-10-30 12:06: each usage account is read on a schedule, every
// 4-14 minutes, so a spawn finds a fresh reading; a schedule whose check is
// off (reserve 0) reads nothing.
func TestUsageCacheSchedule(t *testing.T) {
	t.Parallel()
	for i := 0; i < 2000; i++ {
		if d := randBetween(usageEveryMin, usageEveryMax); d < 4*time.Minute || d > 14*time.Minute {
			t.Fatalf("interval %s outside 4-14 min", d)
		}
	}
	c := NewUsageCache()
	ticks := make(chan chan time.Time, 8)
	var waits []time.Duration
	var mu sync.Mutex
	c.after = func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		ticks <- ch
		return ch
	}
	c.first = func() time.Duration { return 30 * time.Second }
	c.interval = func() time.Duration { return 9 * time.Minute }
	reads := make(chan struct{}, 8)
	var enabled sync.Mutex
	on := true
	read := c.Register("acct", func(context.Context) (*delegator.UsageInfo, error) {
		reads <- struct{}{}
		return usageOf(1, 1), nil
	}, func() bool { enabled.Lock(); defer enabled.Unlock(); return on })

	// Not started: no schedule.
	c.mu.Lock()
	if c.entries["acct"].scheduled {
		t.Fatal("a schedule runs before Start")
	}
	c.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	tick := func() {
		t.Helper()
		select {
		case ch := <-ticks:
			ch <- time.Now()
		case <-time.After(10 * time.Second):
			t.Fatal("the schedule never waited")
		}
	}
	tick()
	recv(t, reads, "the first scheduled read")
	// The next wait is armed only after the read ended, so the reading is in.
	select {
	case ch := <-ticks:
		if r := read(context.Background()); r.Info == nil || r.Pending {
			t.Fatalf("after a scheduled read: %+v, want its reading", r)
		}
		select {
		case <-reads:
			t.Fatal("a fresh scheduled reading triggered a spawn read")
		default:
		}
		// Turn the check off: the next tick reads nothing.
		enabled.Lock()
		on = false
		enabled.Unlock()
		ch <- time.Now()
	case <-time.After(10 * time.Second):
		t.Fatal("no second wait")
	}
	select {
	case ch := <-ticks:
		select {
		case <-reads:
			t.Fatal("a schedule read while the check is off")
		default:
		}
		_ = ch
	case <-time.After(10 * time.Second):
		t.Fatal("no third wait")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) < 3 || waits[0] != 30*time.Second || waits[1] != 9*time.Minute {
		t.Errorf("waits = %v, want the first delay then the interval", waits)
	}
}

// Every Claude Code agent shares one usage account, so one cache entry (and
// one scheduled read); a per-agent backend (codex) keeps one per agent.
func TestSpawnUsageReaderAccountKey(t *testing.T) {
	t.Parallel()
	q := func(context.Context, string) (*delegator.UsageInfo, error) { return nil, nil }
	SpawnUsageReader("xacct-cc", "a", q, true, nil)
	SpawnUsageReader("xacct-cc", "b", q, true, nil)
	SpawnUsageReader("xacct-codex", "a", q, false, nil)
	SpawnUsageReader("xacct-codex", "b", q, false, nil)
	spawnUsage.mu.Lock()
	defer spawnUsage.mu.Unlock()
	var keys []string
	for k := range spawnUsage.entries {
		if strings.HasPrefix(k, "xacct-") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if want := []string{"xacct-cc", "xacct-codex/a", "xacct-codex/b"}; !slices.Equal(keys, want) {
		t.Errorf("usage keys = %v, want %v", keys, want)
	}
}

// A cross-agent spawn a restart interrupted is resumed on the TARGET's
// agent, with the target's own CanResume/PrepareCharacterBranch, and its
// result still goes to the caller's session.
func TestRestoreCrossAgentSpawnResumesOnTarget(t *testing.T) {
	t.Parallel()
	callerAgent, targetAgent := newCtxSpawnAgent(), newCtxSpawnAgent()
	h := newPersistHarness(callerAgent, nil)
	cb := &characterBranches{}
	var canResume []string
	target := NewSpawner(SpawnDeps{
		AgentID: "xrs-target", MaxInherit: 1, Tracker: h.tracker,
		CharacterBranch: cb.create, PrepareCharacterBranch: cb.prepare,
		CanResume: func(bk string) bool { canResume = append(canResume, bk); return true },
	}, func() SpawnAgent { return targetAgent })
	registerTestSpawner(t, target)

	rec := runningRecord("spawn-xr")
	rec.BranchKey = session.CrossAgentSpawnRoot("xrs-target") + "/b1"
	rec.Character, rec.Agent = true, "xrs-target"
	h.store.put(t, rec)
	h.spawner.RestoreSpawns()
	recv(t, targetAgent.ran, "a run on the target's agent")
	if d := h.nextDelivery(t); d.session != "test/imain" {
		t.Fatalf("result delivered to %s, want the caller's session", d.session)
	} else {
		d.done(true)
	}
	targetAgent.mu.Lock()
	defer targetAgent.mu.Unlock()
	if len(targetAgent.keys) != 1 || targetAgent.keys[0] != rec.BranchKey {
		t.Fatalf("target ran %v, want the resumed branch", targetAgent.keys)
	}
	callerAgent.mu.Lock()
	defer callerAgent.mu.Unlock()
	if len(callerAgent.keys) != 0 {
		t.Fatalf("caller's agent ran %v, want nothing", callerAgent.keys)
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if len(cb.prepared) != 1 || len(canResume) != 1 {
		t.Errorf("target prepared %v, CanResume %v; want both asked of the target", cb.prepared, canResume)
	}
}

// #2656 req 3, background form: spawn_stop stops a running background spawn
// by id, and its [SPAWN RESULT] says its parent agent stopped it.
func TestSpawnStopBackgroundSpawn(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1) // the first turn blocks until stopped
	h := newFollowupHarness(agent)
	id := h.start(t, "a long clone")
	waitStarted(t, agent, 0)
	out, err := h.spawner.StopTool().Execute(h.ctx, json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil || !strings.Contains(out.Text, "Stopping spawn "+id) {
		t.Fatalf("spawn_stop = %q, %v", out.Text, err)
	}
	if msg := h.result(t); !strings.Contains(msg, "was stopped by its parent agent (spawn_stop)") {
		t.Fatalf("result = %q, want stopped by its parent agent", msg)
	}
	out, err = h.spawner.StopTool().Execute(h.ctx, json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil || !strings.Contains(out.Text, "is not running") {
		t.Errorf("second spawn_stop = %q, %v; want not running", out.Text, err)
	}
	if _, err := h.spawner.StopTool().Execute(h.ctx, json.RawMessage(`{"id":"spawn-nope"}`)); err == nil {
		t.Error("spawn_stop of an unknown id succeeded")
	}
}

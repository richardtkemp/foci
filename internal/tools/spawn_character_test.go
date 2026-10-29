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
)

// ctxSpawnAgent records each run's session key and whether its context was
// marked as a character spawn's, and answers at once.
type ctxSpawnAgent struct {
	mu        sync.Mutex
	keys      []string
	character []bool
	inherit   []bool
	deadlines []time.Duration
	ran       chan struct{}
}

func newCtxSpawnAgent() *ctxSpawnAgent { return &ctxSpawnAgent{ran: make(chan struct{}, 8)} }

func (a *ctxSpawnAgent) HandleMessage(ctx context.Context, sk string, _ []string, _ []platform.Attachment) error {
	a.mu.Lock()
	a.keys = append(a.keys, sk)
	a.character = append(a.character, IsSpawnCharacter(ctx))
	a.inherit = append(a.inherit, IsSpawnInherit(ctx))
	if dl, ok := ctx.Deadline(); ok {
		a.deadlines = append(a.deadlines, time.Until(dl))
	}
	a.mu.Unlock()
	a.ran <- struct{}{}
	return nil
}

// characterBranches is a fake SpawnDeps.CharacterBranch/PrepareCharacterBranch.
type characterBranches struct {
	mu       sync.Mutex
	created  []string // parent|model
	prepared []string // branch|model
}

func (c *characterBranches) create(_ context.Context, parent, model string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, parent+"|"+model)
	return parent + "/bchar", nil
}

func (c *characterBranches) prepare(bk, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prepared = append(c.prepared, bk+"|"+model)
}

func newCharacterHarness(agent SpawnAgent) (*followupHarness, *characterBranches) {
	h := newFollowupHarness(agent)
	cb := &characterBranches{}
	h.spawner.deps.CharacterBranch = cb.create
	h.spawner.deps.PrepareCharacterBranch = cb.prepare
	return h, cb
}

// TestSpawnDefaultModeIsCharacter pins #2643 req 1: a spawn with no context
// runs as character wherever character is offered — never a clone, even
// when the agent can fork.
func TestSpawnDefaultModeIsCharacter(t *testing.T) {
	t.Parallel()
	for _, modes := range [][]string{
		apiSpawnModes,
		DelegatedSpawnModes(delegator.Spec{Caps: allBatchCaps}),
		{spawnModeCharacter},
	} {
		if got := defaultMode(modes); got != spawnModeCharacter {
			t.Errorf("defaultMode(%v) = %q, want character", modes, got)
		}
	}
}

// TestDelegatedSpawnTool_DefaultsToCharacter proves a delegated agent's spawn
// with no context runs as a direct character batch on a backend that can
// fork, with the 120s direct default: no branch, nothing tracked.
func TestDelegatedSpawnTool_DefaultsToCharacter(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, allBatchCaps)
	tr := NewSpawnTracker()
	s.deps.Tracker = tr
	res, err := s.SpawnTool().Execute(WithSessionKey(context.Background(), "test/c1"), json.RawMessage(`{"prompt":"do it"}`))
	if err != nil || res.Text != "answer for spawn_character" {
		t.Fatalf("default spawn = %q, %v; want the character batch's answer", res.Text, err)
	}
	if len(runner.reqs) != 1 || runner.reqs[0].Purpose != delegator.BatchPurposeSpawnCharacter {
		t.Fatalf("batches = %+v, want one spawn_character", runner.reqs)
	}
	if d := runner.deadlines[0]; d <= 60*time.Second || d > 120*time.Second {
		t.Errorf("direct character ran with %s left, want the 120s default", d)
	}
	if n := len(tr.List("test/c1")); n != 0 {
		t.Errorf("a direct character spawn was tracked as background (%d)", n)
	}
}

// TestSpawnContextCharacterDefault proves an API agent's spawn with no
// context runs the character one-shot, not a clone: no branch is made and
// the agent is never handed a branch turn.
func TestSpawnContextCharacterDefault(t *testing.T) {
	t.Parallel()
	mockAgent := &mockSpawnAgent{response: "Done."}
	mockSessions := &mockSessionBrancher{}
	tool := NewSpawner(SpawnDeps{
		Sessions:       mockSessions,
		AgentID:        "test",
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxInherit:     3,
	}, func() SpawnAgent { return mockAgent }).SpawnTool()
	if _, err := tool.Execute(WithSessionKey(context.Background(), "test/imain"), json.RawMessage(`{"prompt":"Do something"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if mockSessions.parentKey != "" {
		t.Error("a spawn with no context forked a branch (clone); want character")
	}
}

// TestSpawnTimeoutsByRunForm pins #2643 req 3: 120s default for a spawn that
// returns directly, 3600s for one in the background, and at most 86400s for
// any, clamped.
func TestSpawnTimeoutsByRunForm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sec        int
		background bool
		want       time.Duration
	}{
		{0, false, 120 * time.Second},
		{0, true, time.Hour},
		{30, false, 30 * time.Second},
		{7200, true, 2 * time.Hour},
		{100000, false, 24 * time.Hour},
		{100000, true, 24 * time.Hour},
	}
	for _, c := range cases {
		if got := resolveSpawnTimeout(c.sec, c.background); got != c.want {
			t.Errorf("resolveSpawnTimeout(%d, background=%v) = %s, want %s", c.sec, c.background, got, c.want)
		}
	}

	// Through the tool: a clone with no timeout gets an hour; one asking
	// for more than a day gets a day; a direct one-shot over a day too.
	h := newFollowupHarness(newScriptedSpawnAgent(0))
	if _, err := h.spawner.SpawnTool().Execute(h.ctx, json.RawMessage(`{"prompt":"a","context":"clone"}`)); err != nil {
		t.Fatal(err)
	}
	h.result(t)
	if _, err := h.spawner.SpawnTool().Execute(h.ctx, json.RawMessage(`{"prompt":"b","context":"clone","timeout":999999}`)); err != nil {
		t.Fatal(err)
	}
	h.result(t)
	got := map[string]time.Duration{}
	for _, sp := range h.tracker.List("test/imain") {
		got[sp.Description] = sp.Timeout
	}
	if got["a"] != time.Hour || got["b"] != 24*time.Hour {
		t.Errorf("clone timeouts = %v, want a: 1h (default), b: 24h (clamped)", got)
	}

	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, allBatchCaps)
	if _, err := s.SpawnTool().Execute(WithSessionKey(context.Background(), "test/c1"), json.RawMessage(`{"prompt":"p","context":"explore","timeout":999999}`)); err != nil {
		t.Fatal(err)
	}
	if d := runner.deadlines[0]; d <= 23*time.Hour || d > 24*time.Hour {
		t.Errorf("direct spawn asking 999999s ran with %s left, want the 24h maximum", d)
	}
}

// TestBackgroundCharacterSpawn pins #2643 req 2: a character spawn with
// background=true runs on a fresh character branch (CharacterBranch, with the
// caller's model), marked as a character spawn, with the background default
// timeout; its result arrives as [SPAWN RESULT]; spawn_status shows it and
// spawn_send resumes it on the same branch, still as a character spawn.
func TestBackgroundCharacterSpawn(t *testing.T) {
	t.Parallel()
	agent := newCtxSpawnAgent()
	h, cb := newCharacterHarness(agent)
	res, err := h.spawner.SpawnTool().Execute(h.ctx, json.RawMessage(`{"prompt":"fix the bug","background":true,"model":"cheap"}`))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	m := spawnIDRe.FindStringSubmatch(res.Text)
	if m == nil || !strings.Contains(res.Text, "Character spawn started in background") {
		t.Fatalf("spawn result = %q, want a background character spawn with its id", res.Text)
	}
	id := m[1]
	if len(cb.created) != 1 || cb.created[0] != "test/imain|cheap" {
		t.Fatalf("CharacterBranch calls = %v, want one for test/imain with model cheap", cb.created)
	}
	if msg := h.result(t); !strings.Contains(msg, "[SPAWN RESULT]") || !strings.Contains(msg, id) {
		t.Fatalf("delivered %q, want the spawn's [SPAWN RESULT]", msg)
	}
	agent.mu.Lock()
	if len(agent.keys) != 1 || agent.keys[0] != "test/imain/bchar" || !agent.character[0] {
		t.Fatalf("runs = keys %v character %v, want one character-marked run on the character branch", agent.keys, agent.character)
	}
	if d := agent.deadlines[0]; d <= 59*time.Minute || d > time.Hour {
		t.Errorf("background character ran with %s left, want the 1h default", d)
	}
	agent.mu.Unlock()

	if st := h.status(t, id); !strings.Contains(st, "completed") || !strings.Contains(st, "test/imain/bchar") {
		t.Errorf("spawn_status = %q, want the finished character spawn on its branch", st)
	}
	if out := h.send(t, id, "also add a test"); !strings.Contains(out, "resumed") {
		t.Fatalf("spawn_send to a finished character spawn = %q, want it resumed", out)
	}
	h.result(t)
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.keys) != 2 || agent.keys[1] != "test/imain/bchar" || !agent.character[1] {
		t.Fatalf("resumed run = keys %v character %v, want the same branch, still a character spawn", agent.keys, agent.character)
	}
	if d := agent.deadlines[1]; d <= 59*time.Minute || d > time.Hour {
		t.Errorf("resumed spawn ran with %s left, want the 1h background default", d)
	}
}

// TestCloneNotMarkedCharacter proves a clone's run carries no character mark
// (its tools that need the user still work, as before #2643).
func TestCloneNotMarkedCharacter(t *testing.T) {
	t.Parallel()
	agent := newCtxSpawnAgent()
	h, _ := newCharacterHarness(agent)
	h.start(t, "clone it")
	h.result(t)
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.character) != 1 || agent.character[0] {
		t.Fatalf("clone run character mark = %v, want false", agent.character)
	}
}

// TestBackgroundOnlyForCharacter proves background is refused for a mode
// that returns directly, and a background spawn cannot start another one.
func TestBackgroundOnlyForCharacter(t *testing.T) {
	t.Parallel()
	agent := newCtxSpawnAgent()
	h, cb := newCharacterHarness(agent)
	for _, mode := range []string{"raw", "explore"} {
		params, _ := json.Marshal(map[string]any{"prompt": "p", "context": mode, "background": true})
		if _, err := h.spawner.SpawnTool().Execute(h.ctx, params); err == nil || !strings.Contains(err.Error(), "only a character spawn") {
			t.Errorf("%s with background: err = %v, want refused", mode, err)
		}
	}
	nested := WithSpawnInherit(h.ctx)
	if _, err := h.spawner.SpawnTool().Execute(nested, json.RawMessage(`{"prompt":"p","background":true}`)); err == nil {
		t.Error("a background spawn started another background spawn")
	}
	if len(cb.created) != 0 {
		t.Errorf("refused spawns created branches: %v", cb.created)
	}
}

// TestRestoreCharacterSpawnPreparesBranch proves a character spawn a restart
// interrupted has its branch prepared as a character branch again (its
// launch settings were in memory) before it is resumed, marked as one.
func TestRestoreCharacterSpawnPreparesBranch(t *testing.T) {
	t.Parallel()
	agent := newCtxSpawnAgent()
	cb := &characterBranches{}
	h := newPersistHarness(agent, func(d *SpawnDeps) {
		d.CharacterBranch = cb.create
		d.PrepareCharacterBranch = cb.prepare
	})
	rec := runningRecord("spawn-cc")
	rec.Character, rec.SpawnModel = true, "zai/glm-4.6"
	h.store.put(t, rec)
	h.spawner.RestoreSpawns()
	<-agent.ran
	cb.mu.Lock()
	prepared := append([]string(nil), cb.prepared...)
	cb.mu.Unlock()
	if len(prepared) != 1 || prepared[0] != rec.BranchKey+"|zai/glm-4.6" {
		t.Fatalf("PrepareCharacterBranch calls = %v, want the branch with its model", prepared)
	}
	agent.mu.Lock()
	if !agent.character[0] {
		t.Error("the resumed character spawn's run was not marked as a character spawn")
	}
	agent.mu.Unlock()
	h.nextDelivery(t).done(true)
}

// TestCharacterSpawnRecordPersistsMode proves a background character spawn's
// record carries its mode and model, so a restart can prepare it again.
func TestCharacterSpawnRecordPersistsMode(t *testing.T) {
	t.Parallel()
	gated := newScriptedSpawnAgent(1)
	cb := &characterBranches{}
	h := newPersistHarness(gated, func(d *SpawnDeps) {
		d.CharacterBranch = cb.create
		d.PrepareCharacterBranch = cb.prepare
	})
	res, err := h.spawner.SpawnTool().Execute(h.ctx, json.RawMessage(`{"prompt":"p","background":true,"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	id := spawnIDRe.FindStringSubmatch(res.Text)[1]
	<-gated.started
	rec, ok := h.store.record(t, id)
	if !ok || !rec.Character || rec.SpawnModel != "m1" || rec.BranchKey != "test/imain/bchar" {
		t.Fatalf("record = %+v (ok %v), want a character spawn with model m1", rec, ok)
	}
	close(gated.gates[0])
	h.nextDelivery(t).done(true)
}

// TestCharacterSpawnRefusesUserTools proves a tool that would need the user
// refuses inside a character spawn (API: the context mark; delegated: the
// parent-access exec bridge leaves ask and close_session out).
func TestCharacterSpawnRefusesUserTools(t *testing.T) {
	t.Parallel()
	ctx := WithSpawnCharacter(WithSessionKey(context.Background(), "test/imain/bchar"))
	askTool, _ := NewAskTool(nil, nil, nil, nil, nil, "test")
	for _, tool := range []*Tool{askTool, NewCloseSessionTool(nil, nil)} {
		_, err := tool.Execute(ctx, json.RawMessage(`{"questions":[{"question":"q?","options":[{"label":"a"},{"label":"b"}]}]}`))
		if err == nil || !strings.Contains(err.Error(), "refused in a character spawn") {
			t.Errorf("%s in a character spawn: err = %v, want refused", tool.Name, err)
		}
	}
	reg := NewRegistry()
	for _, n := range []string{"ask", "close_session", "spawn", "read"} {
		reg.Register(&Tool{Name: n})
	}
	sub := RegistryForAccess(reg, delegator.BatchAccessParent)
	for _, n := range []string{"ask", "close_session", "spawn"} {
		if sub.Get(n) != nil {
			t.Errorf("parent-access bridge keeps %s", n)
		}
	}
	if sub.Get("read") == nil {
		t.Error("parent-access bridge dropped read")
	}
	if spawnCharacterBlacklist["ask"] != true {
		t.Error("the API character one-shot keeps ask")
	}
}

// TestSpawnToolTextIsApproved pins #2643 req 5: the tool and parameter text
// say what Dick approved, on API and delegated agents alike.
func TestSpawnToolTextIsApproved(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		modes []string
		deleg bool
	}{
		{"api", apiSpawnModes, false},
		{"delegated", DelegatedSpawnModes(delegator.Spec{Caps: allBatchCaps}), true},
	} {
		desc := spawnToolDescription(c.modes)
		for _, want := range []string{
			"Spawn a sub-call to a model. Modes: raw, character, clone, explore.",
			"Default to 'character' for any task you hand off, including long multi-step work such as a bugfix or feature.",
			"It gets your system prompt, tools and permission rules, but not this conversation, so write the prompt to carry everything the task needs: the ticket id, file paths, what done means.",
			"Use 'clone' only when the task needs so much of this conversation that you could not reasonably write it into a prompt. A clone copies the whole conversation, which costs far more.",
			"Use 'raw' for a question that needs no context of yours, and 'explore' for read-only research.",
		} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s description lacks %q:\n%s", c.name, want, desc)
			}
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(spawnParams(c.modes, c.deleg), &schema); err != nil {
			t.Fatal(err)
		}
		ctxDoc := schema.Properties["context"].Description
		for _, want := range []string{
			"- 'character' (default): your prompt plus your full system prompt, with your own tools and permission rules. Anything that would need the user is refused. Runs in the background with background=true; otherwise it returns its result directly.",
			"- 'clone': a fork of this session that starts from a copy of your conversation. Always runs in the background. The result arrives as [SPAWN RESULT]; steer it with spawn_send and check on it with spawn_status. Use it only when the context can't be written down.",
			"- 'raw': just your prompt, with no other context or prompt provided.",
			"- 'explore': read-only. Reads and searches files and the web, and changes nothing. Returns directly.",
		} {
			if !strings.Contains(ctxDoc, want) {
				t.Errorf("%s context doc lacks %q:\n%s", c.name, want, ctxDoc)
			}
		}
		if got, want := schema.Properties["timeout"].Description, "Seconds the spawn may run. The default is 120 for a spawn that returns directly, and 3600 (1 hour) for one that runs in the background. The maximum is 86400 (24 hours). When it runs out, the spawn is stopped and reported failed."; got != want {
			t.Errorf("%s timeout doc = %q, want %q", c.name, got, want)
		}
		if _, ok := schema.Properties["background"]; !ok {
			t.Errorf("%s schema has no background parameter", c.name)
		}
	}
	// A character-only agent's text names no mode it lacks.
	only := spawnToolDescription([]string{spawnModeCharacter})
	for _, absent := range []string{"'clone'", "'raw'", "'explore'"} {
		if strings.Contains(only, absent) {
			t.Errorf("character-only description mentions %s:\n%s", absent, only)
		}
	}
}

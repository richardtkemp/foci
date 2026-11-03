package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
	"foci/internal/platform"
)

const testSteerNotice = "NOTICE: spawner messages arrive mid-turn and are genuine."

// textSpawnAgent records the texts of each run.
type textSpawnAgent struct {
	mu   sync.Mutex
	runs [][]string
	ran  chan struct{}
}

func newTextSpawnAgent() *textSpawnAgent { return &textSpawnAgent{ran: make(chan struct{}, 8)} }

func (a *textSpawnAgent) HandleMessage(_ context.Context, _ string, texts []string, _ []platform.Attachment) error {
	a.mu.Lock()
	a.runs = append(a.runs, texts)
	a.mu.Unlock()
	a.ran <- struct{}{}
	return nil
}

func (a *textSpawnAgent) firstText(t *testing.T) string {
	t.Helper()
	recv(t, a.ran, "the spawn's first run")
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.runs[0], "\n")
}

func noticeSpec(notice string) *DelegatedSpawn {
	return &DelegatedSpawn{
		Spec: delegator.Spec{
			Caps:             map[delegator.Capability]delegator.Support{delegator.CapBranch: delegator.Yes()},
			SpawnSteerNotice: notice,
		},
		Runner: func() BatchRunner { return nil },
	}
}

// #2833: a background spawn, which spawn_send can reach, is told in its first
// message how a mid-turn message will look, when its backend declares the
// wrapper. The paragraph follows the task.
func TestSpawnSteerNoticeInFirstMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		params   string
		delegate *DelegatedSpawn
		want     bool
	}{
		{"clone, wrapping backend", `{"name":"n1","prompt":"the task","context":"clone"}`, noticeSpec(testSteerNotice), true},
		{"background character, wrapping backend", `{"name":"n2","prompt":"the task","background":true}`, noticeSpec(testSteerNotice), true},
		{"clone, backend with no wrapper", `{"name":"n3","prompt":"the task","context":"clone"}`, noticeSpec(""), false},
		{"background character, backend with no wrapper", `{"name":"n4","prompt":"the task","background":true}`, noticeSpec(""), false},
		{"clone, API agent", `{"name":"n5","prompt":"the task","context":"clone"}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agent := newTextSpawnAgent()
			h, _ := newCharacterHarness(agent)
			h.spawner.deps.Delegated = tc.delegate
			if _, err := h.spawner.SpawnTool().Execute(h.ctx, json.RawMessage(tc.params)); err != nil {
				t.Fatalf("spawn: %v", err)
			}
			want := "the task"
			if tc.want {
				want = "the task\n\n" + testSteerNotice
			}
			if got := agent.firstText(t); got != want {
				t.Fatalf("first message = %q, want %q", got, want)
			}
		})
	}
}

// The paragraph is for the model. What the parent wrote is what the app and
// spawn_status show, and what a restart restates.
func TestSpawnSteerNoticeNotInRecordedPrompt(t *testing.T) {
	t.Parallel()
	agent := newTextSpawnAgent()
	h, _ := newCharacterHarness(agent)
	h.spawner.deps.Delegated = noticeSpec(testSteerNotice)
	id := h.start(t, "the task")
	if got := agent.firstText(t); !strings.Contains(got, testSteerNotice) {
		t.Fatalf("first message %q lacks the notice", got)
	}
	h.obs.mu.Lock()
	prompts := append([]string(nil), h.obs.prompts...)
	h.obs.mu.Unlock()
	if len(prompts) != 1 || prompts[0] != "the task" {
		t.Errorf("observer prompts = %q, want the parent's text alone", prompts)
	}
	if got := h.status(t, id); strings.Contains(got, "NOTICE") {
		t.Errorf("spawn_status shows the notice: %s", got)
	}
}

// A direct spawn returns to the call that started it and takes no spawn_send,
// so it gets no paragraph.
func TestSpawnSteerNoticeNotForDirectSpawn(t *testing.T) {
	t.Parallel()
	runner := &recordingBatchRunner{}
	s := newDelegatedSpawner(runner, map[delegator.Capability]delegator.Support{delegator.CapIsolatedBatch: delegator.Yes()})
	s.deps.Delegated.Spec.SpawnSteerNotice = testSteerNotice
	ctx := WithSessionKey(context.Background(), "test/imain")
	for _, params := range []string{
		`{"name":"d1","prompt":"the task","context":"raw"}`,
		`{"name":"d2","prompt":"the task"}`,
	} {
		if _, err := s.SpawnTool().Execute(ctx, json.RawMessage(params)); err != nil {
			t.Fatalf("spawn %s: %v", params, err)
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.reqs) != 2 {
		t.Fatalf("batches = %d, want 2", len(runner.reqs))
	}
	for _, r := range runner.reqs {
		if !strings.Contains(r.Prompt+r.SystemPrompt, "the task") {
			t.Errorf("batch = %+v, want the task in it (or this test proves nothing)", r)
		}
		if strings.Contains(r.Prompt, "NOTICE") || strings.Contains(r.SystemPrompt, "NOTICE") {
			t.Errorf("direct spawn got the notice: %+v", r)
		}
	}
}

// A spawn as another agent runs on that agent's backend, so that backend's
// declaration decides, not the caller's.
func TestSpawnSteerNoticeCrossAgentUsesTargetBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		caller, host string
		want         bool
	}{
		{"target wraps, caller does not", "", testSteerNotice, true},
		{"caller wraps, target does not", testSteerNotice, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			callerAgent, targetAgent := newTextSpawnAgent(), newTextSpawnAgent()
			h, _ := newCharacterHarness(callerAgent)
			suffix := strings.ReplaceAll(tc.name, " ", "-")
			h.spawner.deps.AgentID = "xsn-caller-" + suffix
			h.spawner.deps.Delegated = noticeSpec(tc.caller)
			h.ctx = WithSessionKey(context.Background(), h.spawner.deps.AgentID+"/imain")
			cb := &characterBranches{}
			targetID := "xsn-target-" + suffix
			target := NewSpawner(SpawnDeps{AgentID: targetID, MaxInherit: 1, CharacterBranch: cb.create, Tracker: h.tracker, Delegated: noticeSpec(tc.host)},
				func() SpawnAgent { return targetAgent })
			registerTestSpawner(t, h.spawner)
			registerTestSpawner(t, target)
			params, _ := json.Marshal(map[string]any{"name": "cross", "prompt": "the task", "background": true, "agent": targetID})
			if _, err := h.spawner.SpawnTool().Execute(h.ctx, params); err != nil {
				t.Fatalf("spawn: %v", err)
			}
			if got := targetAgent.firstText(t); strings.Contains(got, testSteerNotice) != tc.want {
				t.Fatalf("first message = %q, want notice=%v", got, tc.want)
			}
		})
	}
}

// A restart restates the task in case the backend lost the context, so it
// restates the paragraph too: for a spawn cut off before its first input
// reached the model, this is the first message it sees.
func TestSpawnSteerNoticeRestatedAfterRestart(t *testing.T) {
	t.Parallel()
	agent := newTextSpawnAgent()
	h := newPersistHarness(agent, func(d *SpawnDeps) { d.Delegated = noticeSpec(testSteerNotice) })
	h.store.put(t, runningRecord("spawn-sn"))
	h.spawner.RestoreSpawns()
	got := agent.firstText(t)
	if !strings.Contains(got, "count the stars\n\n"+testSteerNotice) {
		t.Fatalf("resume text = %q, want the task then the notice", got)
	}
}

package tools

// A background spawn's Background ID is its name (#2807): the start reply and
// the [SPAWN RESULT] it delivers carry no generated 3-word ID, which
// spawn_send, spawn_status and spawn_stop would not know.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var bgIDRe = regexp.MustCompile(`Background ID: ([^\]\n]+)`)

// Every Background ID a spawn prints, at start and in its result, is its
// name, and each one resolves in spawn_status and spawn_send. Fail-arm:
// before #2807 both were a generated 3-word ID that no spawn tool accepted.
func TestSpawnBackgroundIDIsName(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(2)
	h := newFollowupHarness(agent)
	params, _ := json.Marshal(map[string]string{"name": "ci watcher", "prompt": "Watch the CI run", "context": "clone"})
	res, err := h.spawner.SpawnTool().Execute(h.ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	m := bgIDRe.FindStringSubmatch(res.Text)
	if m == nil || m[1] != "ci watcher" {
		t.Fatalf("start reply = %q, want Background ID: ci watcher", res.Text)
	}
	<-agent.started
	raw, _ := json.Marshal(map[string]string{"id": m[1]})
	if _, err := h.spawner.StatusTool().Execute(h.ctx, raw); err != nil {
		t.Errorf("spawn_status %q: %v", m[1], err)
	}
	if txt := h.send(t, m[1], "also check lint"); txt == "" {
		t.Error("spawn_send by the printed Background ID returned nothing")
	}
	close(agent.gates[0])
	close(agent.gates[1]) // the follow-up turn spawn_send queued
	msg := <-h.delivered
	got := bgIDRe.FindAllStringSubmatch(msg, -1)
	if len(got) == 0 {
		t.Fatalf("result = %q, want a Background ID", msg)
	}
	for _, g := range got {
		if g[1] != "ci watcher" {
			t.Errorf("result Background ID = %q, want ci watcher (result %q)", g[1], msg)
		}
	}
}

// A spawn with no name (a record from before names, #2721), or a name that
// one line cannot carry as typed, prints its spawn id as its Background ID:
// not the placeholder label, nor a collapsed name lookup would not match.
func TestSpawnBackgroundIDFallsBackToID(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "two\nwords", "two  spaces"} {
		if got := spawnBackgroundID(RunningSpawn{ID: "spawn-abc", Name: name}); got != "spawn-abc" {
			t.Errorf("spawnBackgroundID(name %q) = %q, want the spawn id", name, got)
		}
	}
	if got := spawnBackgroundID(RunningSpawn{ID: "spawn-abc", Name: "ci watcher"}); got != "ci watcher" {
		t.Errorf("spawnBackgroundID(named) = %q, want the name", got)
	}
}

// RunInBackground uses a caller-given ID in place of a generated one, at
// start and on the notification.
func TestRunInBackgroundGivenID(t *testing.T) {
	t.Parallel()
	got := make(chan string, 1)
	n := NewAsyncNotifier(func(_, msg, _, _ string, _ *AgentOrigin) { got <- msg })
	done := make(chan struct{})
	close(done)
	res, err := RunInBackground(context.Background(), BackgroundParams{
		SessionKey:    "s",
		Notifier:      n,
		Done:          done,
		ID:            "my-spawn",
		NotifyMessage: func() string { return "[TEST] done" },
		PendingResult: TextResult("pending"),
	})
	if err != nil || res.Text != "pending\nBackground ID: my-spawn" {
		t.Fatalf("result = %q, %v", res.Text, err)
	}
	if msg := <-got; !strings.HasPrefix(msg, "[Background ID: my-spawn]\n[TEST] done") {
		t.Errorf("notification = %q", msg)
	}
}

// A name the one-line display would alter is refused at spawn time (#2810):
// the user and the agent would see one name while spawn_send, spawn_status and
// spawn_stop match another. Whitespace around a name is trimmed, not refused.
func TestValidSpawnNameRefusesNameDisplayWouldAlter(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"ci  watcher", "ci   watcher", "ci\nwatcher", "ci\twatcher", "ci \t watcher"} {
		if got, err := validSpawnName(raw); err == nil {
			t.Errorf("validSpawnName(%q) = %q, want a refusal", raw, got)
		} else if !strings.Contains(err.Error(), "ci watcher") {
			t.Errorf("validSpawnName(%q) error = %q, want it to offer the name 'ci watcher'", raw, err)
		}
	}
	for raw, want := range map[string]string{"ci watcher": "ci watcher", "  ci watcher\n": "ci watcher", "reviewer": "reviewer"} {
		if got, err := validSpawnName(raw); err != nil || got != want {
			t.Errorf("validSpawnName(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

// The spawn tool itself refuses such a name and starts nothing.
func TestSpawnToolRefusesRunOfSpacesInName(t *testing.T) {
	t.Parallel()
	agent := newScriptedSpawnAgent(1)
	h := newFollowupHarness(agent)
	params, _ := json.Marshal(map[string]string{"name": "ci  watcher", "prompt": "Watch the CI run", "context": "clone"})
	if res, err := h.spawner.SpawnTool().Execute(h.ctx, params); err == nil {
		t.Fatalf("spawn with name %q = %q, want a refusal", "ci  watcher", res.Text)
	}
	select {
	case <-agent.started:
		t.Error("a refused spawn started a turn")
	default:
	}
}

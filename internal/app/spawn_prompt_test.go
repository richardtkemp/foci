package app

import (
	"testing"

	"foci/internal/fap"
	"foci/internal/tools"
)

// TestSpawnObserver_FollowupIsSubagentPrompt proves a parent's spawn_send
// message to a running spawn reaches the app as a subagent.prompt frame on
// the spawn's group, after its start and text, so the client adds it to the
// spawn's view as another "asked" block (#2822). Fail-arm: before #2822 the
// observer had no such call and the view never showed the message.
func TestSpawnObserver_FollowupIsSubagentPrompt(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })
	b := h.ensureBinding(nil, "scout", "conv-1")
	client := fakeClient()
	b.attach(client)

	obs := SpawnObserver{}
	obs.SpawnStarted(b.key(), tools.RunningSpawn{ID: "spawn-1", Name: "fixer"}, "fix the bug")
	obs.SpawnText(b.key(), "spawn-1", "looking")
	obs.SpawnPrompt(b.key(), "spawn-1", "new fact: it is the server")

	var types []string
	var prompt map[string]any
	for _, f := range drain(t, client) {
		switch f.t {
		case fap.TypeSubagentStart, fap.TypeSubagentText, fap.TypeSubagentPrompt:
			types = append(types, f.t)
		}
		if f.t == fap.TypeSubagentPrompt {
			prompt = f.d
		}
	}
	want := []string{fap.TypeSubagentStart, fap.TypeSubagentText, fap.TypeSubagentPrompt}
	if len(types) != len(want) || types[0] != want[0] || types[1] != want[1] || types[2] != want[2] {
		t.Fatalf("frames = %q, want %q", types, want)
	}
	if prompt["groupKey"] != "spawn-1" || prompt["prompt"] != "new fact: it is the server" || prompt["conversationId"] != "conv-1" {
		t.Errorf("subagent.prompt = %v, want the message on the spawn's group in conv-1", prompt)
	}
	if ri, _ := prompt["runIndex"].(float64); ri != 1 {
		t.Errorf("subagent.prompt runIndex = %v, want 1 (a spawn's chit has one run)", prompt["runIndex"])
	}
}

package app

import (
	"testing"

	"foci/internal/fap"
	"foci/internal/tools"
)

// TestSpawnObserver_ModelLearnedLater proves a spawn entry that started with no
// model gets one in a new activity frame, the only list the client reads (#2879).
func TestSpawnObserver_ModelLearnedLater(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })
	b := h.ensureBinding(nil, "cam", "conv-1")
	client := fakeClient()
	b.attach(client)

	obs := SpawnObserver{}
	obs.SpawnStarted(b.key(), tools.RunningSpawn{ID: "spawn-1", Name: "reviewer", Agent: "codex"}, "review")
	drain(t, client)

	obs.SpawnModel(b.key(), "spawn-1", "codex/gpt-6.1-sol")

	var model string
	var frames int
	for _, f := range drain(t, client) {
		list, ok := f.d["subagents"].([]any)
		if f.t != fap.TypeActivity || !ok {
			continue
		}
		frames++
		model, _ = list[0].(map[string]any)["model"].(string)
	}
	if frames != 1 || model != "codex/gpt-6.1-sol" {
		t.Errorf("activity frames with a list = %d, model = %q; want 1 and codex/gpt-6.1-sol", frames, model)
	}
}

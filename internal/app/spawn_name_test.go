package app

import (
	"strings"
	"testing"

	"foci/internal/fap"
	"foci/internal/tools"
)

// TestSpawnObserver_LabelsSpawnByName proves the app is fed a foci spawn's
// name, not its prompt opening (#2721): the subagent.start frame's label (the
// chip and the view header), the running entry's description (the stop
// dialog and the view it opens) and the activity caption all carry the name
// on one line, while the prompt still arrives as the run's first message. A
// spawn with no name shows the placeholder. Fail-arm: before #2721 all three
// carried the prompt's opening.
func TestSpawnObserver_LabelsSpawnByName(t *testing.T) {
	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })
	b := h.ensureBinding(nil, "scout", "conv-1")
	client := fakeClient()
	b.attach(client)

	const prompt = "Todo #9: WRITE THE SPEC for the parser rewrite"
	SpawnObserver{}.SpawnStarted(b.sessionKey, tools.RunningSpawn{
		ID: "spawn-1", Name: "spec\n  writer", Description: prompt, Model: "opus",
	}, prompt)
	SpawnObserver{}.SpawnStarted(b.sessionKey, tools.RunningSpawn{ID: "spawn-2", Description: "an old task"}, "an old task")

	var labels []string
	for _, f := range drain(t, client) {
		if f.t == fap.TypeSubagentStart {
			labels = append(labels, f.d["label"].(string))
			if f.d["groupKey"] == "spawn-1" && f.d["prompt"] != prompt {
				t.Errorf("spawn-1 start prompt = %v, want the prompt as its first message", f.d["prompt"])
			}
		}
	}
	if len(labels) != 2 || labels[0] != "spec writer" || labels[1] != tools.UnnamedSpawnLabel {
		t.Errorf("subagent.start labels = %q, want the name and the placeholder", labels)
	}
	info := b.info()
	if len(info.Subagents) != 2 || info.Subagents[0].Description != "spec writer" || info.Subagents[1].Description != tools.UnnamedSpawnLabel {
		t.Errorf("running entries = %+v, want them described by name", info.Subagents)
	}
	if info.ActivityDetail != "spec writer, "+tools.UnnamedSpawnLabel || strings.Contains(info.ActivityDetail, "WRITE THE SPEC") {
		t.Errorf("activity caption = %q, want the names, not the prompt", info.ActivityDetail)
	}
}

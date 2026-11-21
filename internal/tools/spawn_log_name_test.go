package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"foci/internal/log"
)

// startLine returns the first spawn log line in logs that contains marker,
// or "" when none does.
func startLine(logs, marker string) string {
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, marker) {
			return l
		}
	}
	return ""
}

// TestSpawnStartLinesNameTheSpawn proves the spawn start line, for a direct
// and a background spawn, carries the spawn's name next to its id (#3167):
// a name a user or agent quotes greps straight to the spawn id. Not
// parallel: it captures the process-global log output.
func TestSpawnStartLinesNameTheSpawn(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// Direct: a character one-shot on an API client.
	direct := SpawnDeps{
		Client:         &fallbackProbeClient{responses: []fallbackProbeResponse{{text: "done"}}},
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxToolLoops:   func() int { return 3 },
	}
	params, _ := json.Marshal(map[string]string{"name": "swale-fetch-prefix", "prompt": "test", "context": "character"})
	if _, err := NewSpawner(direct, nil).SpawnTool().Execute(WithSessionKey(context.Background(), "gil/cfb"), params); err != nil {
		t.Fatalf("direct spawn: %v", err)
	}
	if l := startLine(logs.String(), "direct spawn="); !strings.Contains(l, `name="swale-fetch-prefix"`) {
		t.Errorf("direct spawn start line does not carry name=\"swale-fetch-prefix\"; line: %q\nlogs:\n%s", l, logs.String())
	}

	// Background: a clone spawn with a notifier returns at once.
	delivered := make(chan struct{}, 1)
	bg := SpawnDeps{
		Sessions:       &mockSessionBrancher{},
		AgentID:        "test",
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxInherit:     3,
		Notifier: NewAsyncNotifier(func(_, _, _, _ string, _ *AgentOrigin) {
			delivered <- struct{}{}
		}),
	}
	agent := &channelSpawnAgent{response: "ok", called: make(chan struct{}, 1)}
	params, _ = json.Marshal(map[string]string{"name": "pebble-bg-name", "prompt": "Do research", "context": "clone"})
	if _, err := NewSpawner(bg, func() SpawnAgent { return agent }).SpawnTool().Execute(WithSessionKey(context.Background(), "test/imain"), params); err != nil {
		t.Fatalf("background spawn: %v", err)
	}
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("background spawn result not delivered")
	}
	l := startLine(logs.String(), "background spawn=")
	if !strings.Contains(l, `name="pebble-bg-name"`) || !strings.Contains(l, "spawn=spawn-") {
		t.Errorf("background spawn start line does not carry its id and name=\"pebble-bg-name\"; line: %q\nlogs:\n%s", l, logs.String())
	}
}

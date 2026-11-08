package tools

import (
	"context"
	"encoding/json"
	"testing"

	"foci/internal/provider"
)

// TestSpawnOneShotBudgetDefaultsWhenUnset is #2977: a one-shot spawn whose
// tool-loop budget reads 0 (agent_loop.max_tool_loops unset) or has no budget
// dep at all still makes its model call, on the documented default budget,
// instead of answering "Max tool call depth reached." at once.
func TestSpawnOneShotBudgetDefaultsWhenUnset(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		deps func(d *SpawnDeps)
	}{
		{"budget reads 0", func(d *SpawnDeps) {
			d.MaxToolLoops = func() int { return 0 }
			d.ExploreMaxDepth = func() int { return 0 }
		}},
		{"no budget dep", func(*SpawnDeps) {}},
	} {
		for _, mode := range []string{spawnModeRaw, spawnModeCharacter, spawnModeExplore} {
			calls := 0
			server := mockModelServer(func(*provider.MessageRequest) *provider.MessageResponse {
				calls++
				return &provider.MessageResponse{
					ID: "msg_test", Type: "message", Role: "assistant",
					Content: provider.TextContent("done"), StopReason: "end_turn",
				}
			})
			deps := SpawnDeps{
				Client:         newTestAnthropicClient(server.URL, "test-token"),
				FallbackModel:  "anthropic/claude-haiku-4-5",
				FallbackFormat: "anthropic",
			}
			tc.deps(&deps)
			params, _ := json.Marshal(map[string]string{"name": "budget", "prompt": "Say done.", "context": mode})
			res, err := NewSpawner(deps, nil).SpawnTool().Execute(context.Background(), params)
			server.Close()
			if err != nil {
				t.Fatalf("%s, %s: %v", tc.name, mode, err)
			}
			if calls != 1 || res.Text != "done" {
				t.Errorf("%s, %s: %d model calls, result %q; want 1 call and \"done\"", tc.name, mode, calls, res.Text)
			}
		}
	}
}

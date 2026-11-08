package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"foci/internal/provider"
)

// #2978: an API agent's direct spawn shows its progress while it runs, as a
// Claude Code spawn does: the text between its tool calls and each tool call
// reach the app (SpawnText, SpawnTool) and its spawn_status log, before it
// ends.
func TestAPIOneShot_ReportsProgressWhileRunning(t *testing.T) {
	t.Parallel()
	obs := &recordingSpawnObserver{}
	tr := NewSpawnTracker()
	const parent = "oshp/c1"

	// What the observer and the tracker held when the tool ran: the spawn
	// had not ended yet.
	var mu sync.Mutex
	var midTools, midTexts []string
	var midLog []SpawnLogEntry

	calls := 0
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		calls++
		if calls == 1 {
			return &provider.MessageResponse{
				ID: "m1", Type: "message", Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Let me look that up."},
					{Type: "tool_use", ID: "tu_1", Name: "probe_tool", Input: json.RawMessage(`{"q":"weather"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID: "m2", Type: "message", Role: "assistant",
			Content:    provider.TextContent("It is sunny."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 20, OutputTokens: 5},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(&Tool{
		Name:       "probe_tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			obs.mu.Lock()
			tools := append([]string(nil), obs.tools...)
			texts := append([]string(nil), obs.texts...)
			obs.mu.Unlock()
			var log []SpawnLogEntry
			if r := tr.Running(parent); len(r) == 1 {
				if snap, ok := tr.Lookup(parent, r[0].ID, 0); ok {
					log = snap.Log
				}
			}
			mu.Lock()
			midTools, midTexts, midLog = tools, texts, log
			mu.Unlock()
			return TextResult("sunny"), nil
		},
	})

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{
		AgentID: "oshp", Client: client, Registry: reg, Tracker: tr, Observer: obs,
		FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic",
		MaxToolLoops: func() int { return 10 },
	}
	tool := NewSpawner(deps, nil).SpawnTool()
	params, _ := json.Marshal(map[string]string{"name": "prog1", "prompt": "weather?", "context": "character"})
	res, err := tool.Execute(WithSessionKey(context.Background(), parent), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Text != "It is sunny." {
		t.Errorf("result = %q", res.Text)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(midTools) != 1 || !strings.HasSuffix(midTools[0], `=probe_tool {"q":"weather"}`) {
		t.Errorf("tool calls shown while running = %v, want the probe_tool call", midTools)
	}
	if len(midTexts) != 1 || !strings.HasSuffix(midTexts[0], "=Let me look that up.") {
		t.Errorf("text shown while running = %v, want the text before the tool call", midTexts)
	}
	var kinds []string
	for _, e := range midLog {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "prompt,text,tool" {
		t.Errorf("spawn_status log while running = %s, want prompt,text,tool", got)
	}

	// The answer is shown once, at the end.
	obs.mu.Lock()
	defer obs.mu.Unlock()
	var answers int
	for _, tx := range obs.texts {
		if strings.HasSuffix(tx, "=It is sunny.") {
			answers++
		}
	}
	if answers != 1 || len(obs.texts) != 2 {
		t.Errorf("texts = %v, want the intermediate text then the answer once", obs.texts)
	}
	// The entry named the resolved model from the start (directModel), and
	// the model that served did not differ: no model update is due.
	if len(obs.models) != 0 {
		t.Errorf("model updates = %v, want none (the served model is the one it started on)", obs.models)
	}
}

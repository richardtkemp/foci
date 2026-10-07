package tools

import (
	"context"
	"encoding/json"
	"testing"

	"foci/internal/provider"
)

// spawnRoutingClient is a provider.Client for the spawn routing tests.
// ProviderRouting and RoutingFor are json:"-" fields, invisible to the
// httptest wire-capture route the other spawn tests use, so this stub
// observes them on the request object instead. spawnOneShot builds a fresh
// request per loop iteration, so keeping the pointers is safe.
// HandlesOwnRetries makes sendWithRetry a single pass, so the scripted
// responses control exactly how many calls happen.
type spawnRoutingClient struct {
	responses []*provider.MessageResponse // consumed in order; panics if exhausted
	callIdx   int
	reqs      []*provider.MessageRequest
}

func (c *spawnRoutingClient) SendMessage(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	if c.callIdx >= len(c.responses) {
		panic("spawnRoutingClient: no more responses")
	}
	c.reqs = append(c.reqs, req)
	r := c.responses[c.callIdx]
	c.callIdx++
	if r.Model == "" {
		r.Model = req.Model
	}
	return r, nil
}

func (c *spawnRoutingClient) CountTokens(_ context.Context, _ *provider.MessageRequest) (int, error) {
	return 0, nil
}

func (c *spawnRoutingClient) IsCachingAvailable() bool { return false }

// HandlesOwnRetries makes sendWithRetry skip its retry loop.
func (c *spawnRoutingClient) HandlesOwnRetries() bool { return true }

// newSpawnRoutingClient scripts a two-call tool loop: one tool_use round,
// then a final text answer.
func newSpawnRoutingClient() *spawnRoutingClient {
	return &spawnRoutingClient{responses: []*provider.MessageResponse{
		{
			ID: "msg_1", Type: "message", Role: "assistant",
			Content: []provider.ContentBlock{
				{Type: "tool_use", ID: "tu_1", Name: "echo_tool", Input: json.RawMessage(`{"text":"hello"}`)},
			},
			StopReason: "tool_use",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		},
		{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content:    provider.TextContent("done"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
		},
	}}
}

// newSpawnEchoRegistry builds a registry with one trivial echo tool.
func newSpawnEchoRegistry() *Registry {
	reg := NewRegistry()
	reg.Register(&Tool{
		Name:       "echo_tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return TextResult("echo"), nil
		},
	})
	return reg
}

// runCharacterSpawn drives the spawn tool in character mode (a synchronous
// one-shot); the recorded requests are read from the deps' client afterwards.
func runCharacterSpawn(t *testing.T, deps SpawnDeps) {
	t.Helper()
	tool := NewSpawnTool(deps, nil)
	params, err := json.Marshal(map[string]string{"prompt": "test", "context": "character"})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestSpawnOneShot_RoutingOnEveryLoop(t *testing.T) {
	// Proves that a one-shot spawn on a model with [models.*.provider]
	// routing carries that routing on EVERY request of its tool loop (a
	// hop between upstream providers would drop the prompt cache), plus
	// the per-model lookup so the spawn's own fallback hops resolve their
	// own routing.
	t.Parallel()
	routingR := &provider.ProviderRouting{Order: []string{"prov-r"}}
	client := newSpawnRoutingClient()
	deps := SpawnDeps{
		Client:         client,
		Registry:       newSpawnEchoRegistry(),
		FallbackModel:  "openrouter/pinned-model",
		FallbackFormat: "openai",
		MaxToolLoops:   func() int { return 5 },
		ProviderRoutingFor: func(model string) *provider.ProviderRouting {
			if model == "openrouter/pinned-model" {
				return routingR
			}
			return nil
		},
	}
	runCharacterSpawn(t, deps)

	if len(client.reqs) != 2 {
		t.Fatalf("spawn sent %d requests, want 2 (one per tool-loop round)", len(client.reqs))
	}
	for i, req := range client.reqs {
		if req.Model != "openrouter/pinned-model" {
			t.Errorf("request %d model = %q, want openrouter/pinned-model", i, req.Model)
		}
		if req.ProviderRouting != routingR {
			t.Errorf("request %d ProviderRouting = %+v, want routing R", i, req.ProviderRouting)
		}
		if req.RoutingFor == nil {
			t.Fatalf("request %d RoutingFor is nil, want the configured lookup", i)
		}
		if got := req.RoutingFor("openrouter/pinned-model"); got != routingR {
			t.Errorf("request %d RoutingFor(openrouter/pinned-model) = %+v, want routing R", i, got)
		}
	}
}

func TestSpawnOneShot_NilRoutingLookupSendsNil(t *testing.T) {
	// Proves the unchanged case: a nil ProviderRoutingFor (no [models.*]
	// config) means no routing on any spawn request — today's behaviour.
	t.Parallel()
	client := newSpawnRoutingClient()
	deps := SpawnDeps{
		Client:         client,
		Registry:       newSpawnEchoRegistry(),
		FallbackModel:  "openrouter/pinned-model",
		FallbackFormat: "openai",
		MaxToolLoops:   func() int { return 5 },
	}
	runCharacterSpawn(t, deps)

	if len(client.reqs) != 2 {
		t.Fatalf("spawn sent %d requests, want 2 (one per tool-loop round)", len(client.reqs))
	}
	for i, req := range client.reqs {
		if req.ProviderRouting != nil {
			t.Errorf("request %d ProviderRouting = %+v, want nil", i, req.ProviderRouting)
		}
	}
}

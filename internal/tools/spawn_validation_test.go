package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/provider"
)

func TestSpawnEmptyPrompt(t *testing.T) {
	// Proves that an empty prompt is rejected with a descriptive error before any API call is made.
	t.Parallel()
	deps := SpawnDeps{FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "",
		"context": "raw",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for empty prompt")
	}
	if !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSpawnInvalidContext(t *testing.T) {
	// Proves that an unrecognized context value is rejected with an "invalid context" error.
	t.Parallel()
	server := mockModelServer(okResponse("ok"))
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{Client: client, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "bogus",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for invalid context")
	}
	if !strings.Contains(err.Error(), "invalid context") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSpawnInheritNoParentSession(t *testing.T) {
	// Proves that clone context without a session key in the context returns a "no parent session" error,
	// since clone requires an existing session to branch from.
	t.Parallel()
	mockAgent := &mockSpawnAgent{response: "ok"}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:       mockSessions,
		AgentID:        "test",
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxInherit:     3,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	// No session key in context
	params, _ := json.Marshal(map[string]string{
		"prompt":  "task",
		"context": "clone",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing parent session")
	}
	if !strings.Contains(err.Error(), "no parent session") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSpawnNoRecursiveInherit(t *testing.T) {
	// Proves that nested clone spawns (a spawned agent trying to spawn another clone) are rejected,
	// while raw and character modes are still allowed from within a spawned context.
	t.Parallel()
	mockAgent := &mockSpawnAgent{response: "ok"}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:       mockSessions,
		AgentID:        "test",
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxInherit:     3,
		MaxToolLoops:   func() int { return 10 },
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	// Mark context as already inside a spawn inherit
	ctx := WithSessionKey(context.Background(), "test/imain")
	ctx = WithSpawnInherit(ctx)

	// Inherit should be rejected
	params, _ := json.Marshal(map[string]string{
		"prompt":  "nested task",
		"context": "clone",
	})
	_, err := tool.Execute(ctx, params)
	if err == nil {
		t.Fatal("expected error for nested inherit")
	}
	if !strings.Contains(err.Error(), "nested inherit spawns not allowed") {
		t.Errorf("error = %q, want 'nested inherit spawns not allowed'", err.Error())
	}

	// But raw/character should still work from inside a spawn inherit
	server := mockModelServer(okResponse("ok"))
	defer server.Close()

	deps.Client = newTestAnthropicClient(server.URL, "test-token")
	tool = NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	params, _ = json.Marshal(map[string]string{
		"prompt":  "simple query",
		"context": "raw",
	})
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("raw mode from spawn inherit should work: %v", err)
	}
	if result.Text != "ok" {
		t.Errorf("result = %q", result.Text)
	}

	params, _ = json.Marshal(map[string]string{
		"prompt":  "full query",
		"context": "character",
	})
	result, err = tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("full mode from spawn inherit should work: %v", err)
	}
	if result.Text != "ok" {
		t.Errorf("result = %q", result.Text)
	}
}

func TestSpawnInheritOrientationTemplate(t *testing.T) {
	// Proves that the OrientationTemplate is passed through to the session brancher
	// with the correct branch type.
	t.Parallel()
	mockAgent := &mockSpawnAgent{response: "Done."}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:            mockSessions,
		AgentID:             "test",
		FallbackModel:       "anthropic/claude-haiku-4-5",
		FallbackFormat:      "anthropic",
		MaxInherit:          3,
		OrientationTemplate: "Type: {branch_type}, key: {branch_key}, parent: {parent_key}.",
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "test/imain")
	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do something",
		"context": "clone",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Template should be passed through to session brancher
	if mockSessions.opts.OrientationTemplate != deps.OrientationTemplate {
		t.Errorf("orientation template = %q, want %q", mockSessions.opts.OrientationTemplate, deps.OrientationTemplate)
	}
	if mockSessions.opts.BranchType != "spawn" {
		t.Errorf("branch type = %q, want %q", mockSessions.opts.BranchType, "spawn")
	}
	if mockSessions.parentKey != "test/imain" {
		t.Errorf("parent key = %q, want %q", mockSessions.parentKey, "test/imain")
	}
}

func TestSpawnModelGroups(t *testing.T) {
	// Proves that model group names (powerful, fast, cheap) are resolved to their
	// configured model IDs via the GroupResolver before the request is sent to the API.
	t.Parallel()
	tests := []struct {
		group string
		full  string
	}{
		{"powerful", "claude-opus-4-6"},
		{"fast", "claude-sonnet-4-6"},
		{"cheap", "claude-haiku-4-5"},
	}

	gr := config.NewGroupResolver(config.GroupsConfig{
		Groups: map[string]string{
			"powerful": "anthropic/claude-opus-4-6",
			"fast":     "anthropic/claude-sonnet-4-6",
			"cheap":    "anthropic/claude-haiku-4-5",
		},
	}, nil, true)

	for _, tt := range tests {
		var receivedModel string
		server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
			receivedModel = req.Model
			return &provider.MessageResponse{
				ID: "msg_test", Type: "message", Role: "assistant",
				Content: provider.TextContent("ok"), StopReason: "end_turn",
				Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		})

		client := newTestAnthropicClient(server.URL, "test-token")
		deps := SpawnDeps{Client: client, GroupResolver: gr, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: func() int { return 10 }}
		tool := NewSpawnTool(deps, nil)

		params, _ := json.Marshal(map[string]string{
			"model":   tt.group,
			"prompt":  "test",
			"context": "raw",
		})
		tool.Execute(context.Background(), params)
		server.Close()

		if receivedModel != tt.full {
			t.Errorf("group=%q: model=%q, want %q", tt.group, receivedModel, tt.full)
		}
	}
}

func TestSpawnModelDefault(t *testing.T) {
	// Proves that omitting the model parameter causes the spawn to inherit the parent agent's configured model.
	t.Parallel()
	var receivedModel string
	var receivedReq *provider.MessageRequest
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		receivedModel = req.Model
		t.Logf("Received request: Model=%q, MaxTokens=%d, Messages=%d", req.Model, req.MaxTokens, len(req.Messages))
		return &provider.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: provider.TextContent("ok"), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-token")
	// Powerful defaults all groups to the same model
	gr := config.NewGroupResolver(config.GroupsConfig{Groups: map[string]string{"powerful": "anthropic/claude-sonnet-4-5"}}, nil, true)
	deps := SpawnDeps{Client: client, GroupResolver: gr, FallbackModel: "anthropic/claude-sonnet-4-5", FallbackFormat: "anthropic", MaxToolLoops: func() int { return 10 }}
	tool := NewSpawnTool(deps, nil)

	// No model specified — should use parent's default (session model)
	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "raw",
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if receivedModel != "claude-sonnet-4-5" {
		t.Errorf("model = %q, want claude-sonnet-4-5 (parent default)", receivedModel)
		if receivedReq != nil {
			t.Logf("Full request: %+v", receivedReq)
		}
	}
}

func TestSpawnModelOpenRouter(t *testing.T) {
	// Proves that a 3-segment OpenRouter model ID (openrouter/stepfun/step-3.5-flash)
	// flows through GroupResolver → resolveSpawnGroup → OpenAI buildParams → HTTP body
	// as the correct 2-segment OpenRouter model ID "stepfun/step-3.5-flash".
	// This was a regression where the developer prefix was double-stripped, sending
	// just "step-3.5-flash" which OpenRouter rejects.
	t.Parallel()

	tests := []struct {
		name     string
		config   string // model string in GroupsConfig.Powerful
		wantWire string // expected model in HTTP request body
	}{
		{"3-segment stepfun", "openrouter/stepfun/step-3.5-flash", "stepfun/step-3.5-flash"},
		{"3-segment deepseek", "openrouter/deepseek/deepseek-r1", "deepseek/deepseek-r1"},
		{"2-segment openai via openrouter", "openrouter/openai/gpt-4o", "openai/gpt-4o"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedModel string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				receivedModel, _ = body["model"].(string)

				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"id":      "chatcmpl-test",
					"object":  "chat.completion",
					"model":   receivedModel,
					"created": 1700000000,
					"choices": []map[string]any{{
						"index":         0,
						"finish_reason": "stop",
						"message": map[string]any{
							"role":    "assistant",
							"content": "ok",
						},
					}},
					"usage": map[string]any{
						"prompt_tokens":     10,
						"completion_tokens": 5,
						"total_tokens":      15,
					},
				})
			}))
			defer server.Close()

			client := newTestOpenAIClient(server.URL, "test-key")
			gr := config.NewGroupResolver(config.GroupsConfig{Groups: map[string]string{"powerful": tt.config}}, nil, true)
			deps := SpawnDeps{
				Client:         client,
				GroupResolver:  gr,
				FallbackModel:  tt.config,
				FallbackFormat: "openai",
				MaxToolLoops:   func() int { return 10 },
			}
			tool := NewSpawnTool(deps, nil)

			params, _ := json.Marshal(map[string]string{
				"prompt":  "test",
				"context": "raw",
			})
			_, err := tool.Execute(context.Background(), params)
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}

			if receivedModel != tt.wantWire {
				t.Errorf("wire model = %q, want %q", receivedModel, tt.wantWire)
			}
		})
	}
}

func TestSpawnTimeout(t *testing.T) {
	// Proves that the timeout parameter is respected: a hanging server causes the spawn to fail
	// within the specified number of seconds rather than blocking indefinitely.
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // longer than our timeout
	}))
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{Client: client, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: func() int { return 10 }}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"prompt":  "test",
		"context": "raw",
		"timeout": 1, // 1 second timeout
	})

	start := time.Now()
	_, err := tool.Execute(context.Background(), params)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %v, expected ~1s timeout", elapsed)
	}
}

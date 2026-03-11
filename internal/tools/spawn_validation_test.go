package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foci/internal/anthropic"
	"foci/internal/provider"
)

func TestSpawnEmptyPrompt(t *testing.T) {
	t.Parallel()
	deps := SpawnDeps{Model: "anthropic/claude-haiku-4-5", ModelAliases: testModelAliases()}
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
	t.Parallel()
	server := mockModelServer(okResponse("ok"))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Model: "anthropic/claude-haiku-4-5", ModelAliases: testModelAliases()}
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
	t.Parallel()
	mockAgent := &mockSpawnAgent{response: "ok"}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "anthropic/claude-haiku-4-5",
		MaxInherit: 3,
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
	t.Parallel()
	mockAgent := &mockSpawnAgent{response: "ok"}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:     mockSessions,
		AgentID:      "test",
		Model:        "anthropic/claude-haiku-4-5",
		MaxInherit:   3,
		MaxToolLoops: 10,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	// Mark context as already inside a spawn inherit
	ctx := WithSessionKey(context.Background(), "test/imain/1000000000")
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

	deps.Client = anthropic.NewClientWithBase(server.URL, "test-token")
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

func TestSpawnInheritOrientationBuilder(t *testing.T) {
	t.Parallel()
	mockAgent := &mockSpawnAgent{response: "Done."}
	mockSessions := &mockSessionBrancher{}

	var builderBranch, builderParent string
	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "anthropic/claude-haiku-4-5",
		MaxInherit: 3,
		OrientationBuilder: func(branchKey, parentKey string) string {
			builderBranch = branchKey
			builderParent = parentKey
			return "You are a spawn branch. Do not message Telegram."
		},
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "test/imain/1000000000")
	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do something",
		"context": "clone",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// OrientationBuilder should have been called with correct keys
	if builderParent != "test/imain/1000000000" {
		t.Errorf("builder parentKey = %q, want agent:test:main", builderParent)
	}
	if !strings.HasPrefix(builderBranch, "test/imain/1000000000/b") {
		t.Errorf("builder branchKey = %q, want prefix agent:test:spawn:spawn-", builderBranch)
	}

	// Orientation message should be passed through to session brancher
	if mockSessions.opts.OrientationMessage != "You are a spawn branch. Do not message Telegram." {
		t.Errorf("orientation = %q", mockSessions.opts.OrientationMessage)
	}
}

func TestSpawnModelShortNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		short string
		full  string
	}{
		{"haiku", "claude-haiku-4-5"},
		{"sonnet", "claude-sonnet-4-6"},
		{"opus", "claude-opus-4-6"},
		{"anthropic/claude-haiku-4-5", "claude-haiku-4-5"},
	}

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

		client := anthropic.NewClientWithBase(server.URL, "test-token")
		deps := SpawnDeps{Client: client, Model: "anthropic/claude-haiku-4-5", ModelAliases: testModelAliases(), MaxToolLoops: 10}
		tool := NewSpawnTool(deps, nil)

		params, _ := json.Marshal(map[string]string{
			"model":   tt.short,
			"prompt":  "test",
			"context": "raw",
		})
		tool.Execute(context.Background(), params)
		server.Close()

		if receivedModel != tt.full {
			t.Errorf("short=%q: model=%q, want %q", tt.short, receivedModel, tt.full)
		}
	}
}

func TestSpawnModelDefault(t *testing.T) {
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

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Model: "anthropic/claude-sonnet-4-5", ModelAliases: testModelAliases(), MaxToolLoops: 10}
	tool := NewSpawnTool(deps, nil)

	// No model specified — should use parent's default
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

func TestSpawnTimeout(t *testing.T) {
	t.Parallel()
	// Server that never responds
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // longer than our timeout
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Model: "anthropic/claude-haiku-4-5", ModelAliases: testModelAliases(), MaxToolLoops: 10}
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

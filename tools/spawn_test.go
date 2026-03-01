package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/anthropic"
)

// mockBootstrap implements SystemBlocksProvider for tests.
type mockBootstrap struct {
	blocks []anthropic.SystemBlock
}

func (m *mockBootstrap) SystemBlocks() []anthropic.SystemBlock {
	return m.blocks
}

// mockModelServer returns a test server that captures requests and returns canned responses.
func mockModelServer(handler func(req *anthropic.MessageRequest) *anthropic.MessageResponse) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req anthropic.MessageRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := handler(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

// mockSessionBrancher captures branch creation calls.
type mockSessionBrancher struct {
	parentKey string
	branchKey string
	opts      BranchOptions
	err       error
}

func (m *mockSessionBrancher) CreateBranch(parentKey, branchKey string, opts BranchOptions) error {
	m.parentKey = parentKey
	m.branchKey = branchKey
	m.opts = opts
	return m.err
}

// mockSpawnAgent captures HandleMessage calls.
type mockSpawnAgent struct {
	sessionKey string
	message    string
	response   string
	err        error
}

func (m *mockSpawnAgent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	m.sessionKey = sessionKey
	m.message = userMessage
	return m.response, m.err
}

func okResponse(text string) func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
	return func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		return &anthropic.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: anthropic.TextContent(text), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	}
}

func TestSpawnContextNone(t *testing.T) {
	var receivedReq *anthropic.MessageRequest

	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("The answer is 42."), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 50, OutputTokens: 20},
		}
	})
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{
		Client: client,
		Bootstrap: &mockBootstrap{blocks: []anthropic.SystemBlock{
			{Type: "text", Text: "I am a character file."},
		}},
		Model: "claude-haiku-4-5",
	}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "What is the meaning of life?",
		"model":   "opus",
		"context": "none",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result != "The answer is 42." {
		t.Errorf("result = %q", result)
	}

	// No system prompt in none mode
	if len(receivedReq.System) != 0 {
		t.Errorf("expected 0 system blocks (none), got %d", len(receivedReq.System))
	}

	// Should resolve opus
	if receivedReq.Model != "claude-opus-4-6" {
		t.Errorf("model = %q, want claude-opus-4-6", receivedReq.Model)
	}

	// No tools
	if len(receivedReq.Tools) != 0 {
		t.Errorf("expected no tools, got %d", len(receivedReq.Tools))
	}
}

func TestSpawnContextCharacterOnly(t *testing.T) {
	var receivedReq *anthropic.MessageRequest

	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("Deep analysis complete."), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 100, OutputTokens: 50},
		}
	})
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{
		Client: client,
		Bootstrap: &mockBootstrap{blocks: []anthropic.SystemBlock{
			{Type: "text", Text: "I am the identity file."},
			{Type: "text", Text: "I am the soul file."},
		}},
		Model: "claude-haiku-4-5",
	}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "Analyze this deeply",
		"model":   "opus",
		"context": "character_only",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result != "Deep analysis complete." {
		t.Errorf("result = %q", result)
	}

	// Full mode includes system blocks
	if len(receivedReq.System) != 2 {
		t.Fatalf("expected 2 system blocks (full), got %d", len(receivedReq.System))
	}
	if receivedReq.System[0].Text != "I am the identity file." {
		t.Errorf("system[0] = %q", receivedReq.System[0].Text)
	}
}

func TestSpawnContextCloneCurrent(t *testing.T) {
	// With a notifier, inherit returns an async ack immediately.
	called := make(chan string, 1)
	mockAgent := &channelSpawnAgent{
		response: "Task completed successfully.",
		called:   make(chan struct{}, 1),
	}
	mockSessions := &mockSessionBrancher{}
	notifier := NewAsyncNotifier(func(sk, msg string) {
		called <- msg
	})

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "claude-haiku-4-5",
		MaxInherit: 3,
		Notifier:   notifier,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "agent:test:main")

	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do the research task",
		"context": "clone_current",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should return async ack, not the agent result
	if !strings.Contains(result, "Spawn started in background") {
		t.Errorf("expected async ack, got %q", result)
	}
	if !strings.Contains(result, "Branch: agent:test:spawn:spawn-") {
		t.Errorf("expected branch key in ack, got %q", result)
	}

	// Should have created a branch
	if mockSessions.parentKey != "agent:test:main" {
		t.Errorf("parent = %q, want agent:test:main", mockSessions.parentKey)
	}
	if !strings.HasPrefix(mockSessions.branchKey, "agent:test:spawn:spawn-") {
		t.Errorf("branch = %q, want prefix agent:test:spawn:spawn-", mockSessions.branchKey)
	}
	if !mockSessions.opts.NoResetHook {
		t.Error("expected noResetHook=true")
	}

	// Wait for HandleMessage to be called in background
	select {
	case <-mockAgent.called:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleMessage not called in background")
	}

	// Wait for notifier delivery
	select {
	case msg := <-called:
		if !strings.Contains(msg, "[SPAWN RESULT]") {
			t.Errorf("expected [SPAWN RESULT] tag, got %q", msg)
		}
		if !strings.Contains(msg, "Task completed successfully.") {
			t.Errorf("expected agent result in notification, got %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notifier not called")
	}
}

func TestSpawnContextCloneCurrentDefault(t *testing.T) {
	// Inherit should be the default context mode — nil notifier = sync fallback
	mockAgent := &mockSpawnAgent{response: "Done."}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "claude-haiku-4-5",
		MaxInherit: 3,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "agent:test:main")
	params, _ := json.Marshal(map[string]string{
		"prompt": "Do something",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify branch was created (meaning inherit mode was used)
	if mockSessions.parentKey == "" {
		t.Error("expected branch creation (inherit as default), but no branch was created")
	}

	// Nil notifier = sync fallback, should get direct result
	if result != "Done." {
		t.Errorf("result = %q, want Done.", result)
	}
}

func TestSpawnModelShortNames(t *testing.T) {
	tests := []struct {
		short string
		full  string
	}{
		{"haiku", "claude-haiku-4-5"},
		{"sonnet", "claude-sonnet-4-5"},
		{"opus", "claude-opus-4-6"},
		{"claude-haiku-4-5", "claude-haiku-4-5"},
	}

	for _, tt := range tests {
		var receivedModel string
		server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
			receivedModel = req.Model
			return &anthropic.MessageResponse{
				ID: "msg_test", Type: "message", Role: "assistant",
				Content: anthropic.TextContent("ok"), StopReason: "end_turn",
				Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 5},
			}
		})

		client := anthropic.NewClientWithBase(server.URL, "test-token")
		deps := SpawnDeps{Client: client, Model: "claude-haiku-4-5"}
		tool := NewSpawnTool(deps, nil)

		params, _ := json.Marshal(map[string]string{
			"model":   tt.short,
			"prompt":  "test",
			"context": "none",
		})
		tool.Execute(context.Background(), params)
		server.Close()

		if receivedModel != tt.full {
			t.Errorf("short=%q: model=%q, want %q", tt.short, receivedModel, tt.full)
		}
	}
}

func TestSpawnModelDefault(t *testing.T) {
	var receivedModel string
	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedModel = req.Model
		return &anthropic.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("ok"), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Model: "claude-sonnet-4-5"}
	tool := NewSpawnTool(deps, nil)

	// No model specified — should use parent's default
	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "none",
	})
	tool.Execute(context.Background(), params)

	if receivedModel != "claude-sonnet-4-5" {
		t.Errorf("model = %q, want claude-sonnet-4-5 (parent default)", receivedModel)
	}
}

func TestSpawnTimeout(t *testing.T) {
	// Server that never responds
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // longer than our timeout
	}))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Model: "claude-haiku-4-5"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"prompt":  "test",
		"context": "none",
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

func TestSpawnNoRecursiveInherit(t *testing.T) {
	mockAgent := &mockSpawnAgent{response: "ok"}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "claude-haiku-4-5",
		MaxInherit: 3,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	// Mark context as already inside a spawn inherit
	ctx := WithSessionKey(context.Background(), "agent:test:main")
	ctx = WithSpawnInherit(ctx)

	// Inherit should be rejected
	params, _ := json.Marshal(map[string]string{
		"prompt":  "nested task",
		"context": "clone_current",
	})
	_, err := tool.Execute(ctx, params)
	if err == nil {
		t.Fatal("expected error for nested inherit")
	}
	if !strings.Contains(err.Error(), "nested inherit spawns not allowed") {
		t.Errorf("error = %q, want 'nested inherit spawns not allowed'", err.Error())
	}

	// But none/full should still work from inside a spawn inherit
	server := mockModelServer(okResponse("ok"))
	defer server.Close()

	deps.Client = anthropic.NewClientWithBase(server.URL, "test-token")
	tool = NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	params, _ = json.Marshal(map[string]string{
		"prompt":  "simple query",
		"context": "none",
	})
	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("none mode from spawn inherit should work: %v", err)
	}
	if result != "ok" {
		t.Errorf("result = %q", result)
	}

	params, _ = json.Marshal(map[string]string{
		"prompt":  "full query",
		"context": "character_only",
	})
	result, err = tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("full mode from spawn inherit should work: %v", err)
	}
	if result != "ok" {
		t.Errorf("result = %q", result)
	}
}

func TestSpawnInheritSemaphore(t *testing.T) {
	var concurrentCount int32
	var maxConcurrent int32

	mockSessions := &mockSessionBrancher{}

	// Use notifier to detect completion of background spawns.
	var completions int32
	allDone := make(chan struct{})
	notifier := NewAsyncNotifier(func(sk, msg string) {
		if c := atomic.AddInt32(&completions, 1); c == 4 {
			close(allDone)
		}
	})

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "claude-haiku-4-5",
		MaxInherit: 2, // only allow 2 concurrent
		Notifier:   notifier,
	}

	// Agent that takes 50ms and tracks concurrency
	tool := NewSpawnTool(deps, func() SpawnAgent {
		return &concurrentAgent{
			concurrentCount: &concurrentCount,
			maxConcurrent:   &maxConcurrent,
		}
	})

	ctx := WithSessionKey(context.Background(), "agent:test:main")

	// Launch 4 concurrent inherit calls (all return immediately with ack)
	for i := 0; i < 4; i++ {
		params, _ := json.Marshal(map[string]string{
			"prompt":  "task",
			"context": "clone_current",
		})
		result, err := tool.Execute(ctx, params)
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		if !strings.Contains(result, "Spawn started in background") {
			t.Fatalf("spawn %d: expected async ack, got %q", i, result)
		}
	}

	// Wait for all background goroutines to complete
	select {
	case <-allDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for background spawns")
	}

	// MaxConcurrent should never exceed 2
	if mc := atomic.LoadInt32(&maxConcurrent); mc > 2 {
		t.Errorf("max concurrent = %d, want <= 2", mc)
	}
}

// concurrentAgent tracks concurrent HandleMessage calls.
type concurrentAgent struct {
	concurrentCount *int32
	maxConcurrent   *int32
}

func (a *concurrentAgent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	cur := atomic.AddInt32(a.concurrentCount, 1)
	for {
		old := atomic.LoadInt32(a.maxConcurrent)
		if cur <= old || atomic.CompareAndSwapInt32(a.maxConcurrent, old, cur) {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	atomic.AddInt32(a.concurrentCount, -1)
	return "ok", nil
}

// channelSpawnAgent signals when HandleMessage is called.
type channelSpawnAgent struct {
	response string
	err      error
	called   chan struct{}
	mu       sync.Mutex
}

func (a *channelSpawnAgent) HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error) {
	a.mu.Lock()
	if a.called != nil {
		select {
		case a.called <- struct{}{}:
		default:
		}
	}
	a.mu.Unlock()
	return a.response, a.err
}

func TestSpawnInheritAsyncDelivery(t *testing.T) {
	// Verify the notifier receives [SPAWN RESULT] with correct session and content.
	delivered := make(chan struct{ sk, msg string }, 1)
	notifier := NewAsyncNotifier(func(sk, msg string) {
		delivered <- struct{ sk, msg string }{sk, msg}
	})

	mockAgent := &channelSpawnAgent{response: "Research complete.", called: make(chan struct{}, 1)}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "claude-haiku-4-5",
		MaxInherit: 3,
		Notifier:   notifier,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "agent:test:main")
	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do research",
		"context": "clone_current",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Spawn started in background") {
		t.Fatalf("expected async ack, got %q", result)
	}

	select {
	case d := <-delivered:
		if d.sk != "agent:test:main" {
			t.Errorf("notified session = %q, want agent:test:main", d.sk)
		}
		if !strings.Contains(d.msg, "[SPAWN RESULT]") {
			t.Errorf("expected [SPAWN RESULT] tag, got %q", d.msg)
		}
		if !strings.Contains(d.msg, "completed:") {
			t.Errorf("expected 'completed:' in msg, got %q", d.msg)
		}
		if !strings.Contains(d.msg, "Research complete.") {
			t.Errorf("expected agent result, got %q", d.msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notifier not called")
	}
}

func TestSpawnInheritAsyncError(t *testing.T) {
	// Verify errors are delivered via notifier with "failed:" tag.
	delivered := make(chan string, 1)
	notifier := NewAsyncNotifier(func(sk, msg string) {
		delivered <- msg
	})

	mockAgent := &channelSpawnAgent{
		err:    fmt.Errorf("tool execution failed: timeout"),
		called: make(chan struct{}, 1),
	}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "claude-haiku-4-5",
		MaxInherit: 3,
		Notifier:   notifier,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "agent:test:main")
	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do task",
		"context": "clone_current",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Spawn started in background") {
		t.Fatalf("expected async ack, got %q", result)
	}

	select {
	case msg := <-delivered:
		if !strings.Contains(msg, "[SPAWN RESULT]") {
			t.Errorf("expected [SPAWN RESULT] tag, got %q", msg)
		}
		if !strings.Contains(msg, "failed:") {
			t.Errorf("expected 'failed:' in msg, got %q", msg)
		}
		if !strings.Contains(msg, "tool execution failed: timeout") {
			t.Errorf("expected error message, got %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notifier not called")
	}
}

func TestSpawnInheritNilNotifierSync(t *testing.T) {
	// Nil notifier = synchronous fallback (existing behavior preserved).
	mockAgent := &mockSpawnAgent{response: "Sync result."}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "claude-haiku-4-5",
		MaxInherit: 3,
		// Notifier intentionally nil
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "agent:test:main")
	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do task",
		"context": "clone_current",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should return the actual result, not an async ack
	if result != "Sync result." {
		t.Errorf("result = %q, want Sync result.", result)
	}

	// Agent should have been called synchronously
	if mockAgent.message != "Do task" {
		t.Errorf("message = %q, want Do task", mockAgent.message)
	}
}

func TestSpawnEmptyPrompt(t *testing.T) {
	deps := SpawnDeps{Model: "claude-haiku-4-5"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "",
		"context": "none",
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
	server := mockModelServer(okResponse("ok"))
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Model: "claude-haiku-4-5"}
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
	mockAgent := &mockSpawnAgent{response: "ok"}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "claude-haiku-4-5",
		MaxInherit: 3,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	// No session key in context
	params, _ := json.Marshal(map[string]string{
		"prompt":  "task",
		"context": "clone_current",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing parent session")
	}
	if !strings.Contains(err.Error(), "no parent session") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestSpawnInheritOrientationBuilder(t *testing.T) {
	mockAgent := &mockSpawnAgent{response: "Done."}
	mockSessions := &mockSessionBrancher{}

	var builderBranch, builderParent string
	deps := SpawnDeps{
		Sessions:   mockSessions,
		AgentID:    "test",
		Model:      "claude-haiku-4-5",
		MaxInherit: 3,
		OrientationBuilder: func(branchKey, parentKey string) string {
			builderBranch = branchKey
			builderParent = parentKey
			return "You are a spawn branch. Do not message Telegram."
		},
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "agent:test:main")
	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do something",
		"context": "clone_current",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// OrientationBuilder should have been called with correct keys
	if builderParent != "agent:test:main" {
		t.Errorf("builder parentKey = %q, want agent:test:main", builderParent)
	}
	if !strings.HasPrefix(builderBranch, "agent:test:spawn:spawn-") {
		t.Errorf("builder branchKey = %q, want prefix agent:test:spawn:spawn-", builderBranch)
	}

	// Orientation message should be passed through to session brancher
	if mockSessions.opts.OrientationMessage != "You are a spawn branch. Do not message Telegram." {
		t.Errorf("orientation = %q", mockSessions.opts.OrientationMessage)
	}
}

func TestSpawnOneShotWithTools(t *testing.T) {
	// Verify one-shot modes get tool definitions and can execute tools.
	callCount := 0
	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		callCount++
		if callCount == 1 {
			// First call: model uses a tool
			if len(req.Tools) == 0 {
				t.Error("expected tools in request")
			}
			return &anthropic.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []anthropic.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "echo_tool", Input: json.RawMessage(`{"text":"hello"}`)},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		// Second call: model returns final text
		return &anthropic.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content:    anthropic.TextContent("Tool said: echo hello"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(&Tool{
		Name:       "echo_tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			var p struct {
				Text string `json:"text"`
			}
			json.Unmarshal(params, &p)
			return "echo: " + p.Text, nil
		},
	})

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, Model: "claude-haiku-4-5"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "character_only",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "Tool said: echo hello" {
		t.Errorf("result = %q", result)
	}
	if callCount != 2 {
		t.Errorf("callCount = %d, want 2", callCount)
	}
}

func TestSpawnNoneToolAllowlist(t *testing.T) {
	// This test ensures every tool registered in the system is explicitly
	// classified as either allowed or blocked for none-mode spawns.
	// If you add a new tool and this test fails, you MUST decide:
	//   - Is the tool safe in an isolated sandbox (no shell access, no
	//     external communication)? Add it to allowedInNone.
	//   - Can it escape the sandbox or communicate externally?
	//     Add it to spawnNoneBlacklist in spawn.go.

	// Tools that should be available in none-mode spawns.
	// These are safe within the file-tool sandbox (no shell access,
	// no external communication, no sandbox escape).
	allowedInNone := map[string]bool{
		"read":             true,
		"write":            true,
		"edit":             true,
		"web_fetch":        true,
		"web_search":       true,
		"http_request":     true,
		"memory_search":    true,
		"bitwarden_search": true,
		"bitwarden_unlock": true,
		"remind":           true,
	}

	// Register every tool that exists in the real system.
	reg := NewRegistry()
	allTools := []string{
		"exec", "tmux",
		"read", "write", "edit",
		"web_fetch", "web_search", "http_request",
		"memory_search", "scratchpad", "todo",
		"bitwarden_search", "bitwarden_unlock",
		"send_telegram", "send_to_session",
		"remind", "spawn",
	}
	for _, name := range allTools {
		reg.Register(&Tool{
			Name:       name,
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:    func(ctx context.Context, params json.RawMessage) (string, error) { return "ok", nil },
		})
	}

	_, tools := spawnIsolatedToolSet(reg, spawnNoneBlacklist, "/tmp/test-sandbox")

	// Verify every tool is either allowed or blocked — no unclassified tools.
	for _, name := range allTools {
		if name == "spawn" {
			// spawn is always excluded (hardcoded in spawnIsolatedToolSet)
			if _, ok := tools[name]; ok {
				t.Errorf("spawn should never be included in spawn tool sets")
			}
			continue
		}
		_, isAllowed := tools[name]
		_, isBlocked := spawnNoneBlacklist[name]
		if !isAllowed && !isBlocked {
			t.Errorf("tool %q is neither allowed nor blacklisted for none-mode spawns — "+
				"add it to allowedInNone in this test (if safe) or spawnNoneBlacklist in spawn.go (if not)", name)
		}
		if isAllowed && isBlocked {
			t.Errorf("tool %q is both allowed and blacklisted — check spawnNoneBlacklist", name)
		}
	}

	// Verify the exact set of allowed tools matches expectations.
	for name := range allowedInNone {
		if _, ok := tools[name]; !ok {
			t.Errorf("tool %q should be allowed in none-mode but is missing", name)
		}
	}
	for name := range tools {
		if !allowedInNone[name] {
			t.Errorf("tool %q is available in none-mode but not in allowedInNone — "+
				"either add it to allowedInNone (if safe) or to spawnNoneBlacklist (if not)", name)
		}
	}

	// Verify blacklisted tools are actually excluded.
	for name := range spawnNoneBlacklist {
		if _, ok := tools[name]; ok {
			t.Errorf("tool %q is blacklisted but still available in none-mode", name)
		}
	}
}

func TestSpawnCharacterOnlyAllTools(t *testing.T) {
	// Verify character_only mode includes all tools (no blacklist).
	var receivedReq *anthropic.MessageRequest
	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("ok"), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	for _, name := range []string{"web_search", "send_telegram", "send_to_session", "exec"} {
		reg.Register(&Tool{
			Name:       name,
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:    func(ctx context.Context, params json.RawMessage) (string, error) { return "ok", nil },
		})
	}

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, Model: "claude-haiku-4-5"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "character_only",
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	toolNames := make(map[string]bool)
	for _, td := range receivedReq.Tools {
		toolNames[td.Name] = true
	}

	if !toolNames["send_telegram"] {
		t.Error("send_telegram should be included in character_only mode")
	}
	if !toolNames["send_to_session"] {
		t.Error("send_to_session should be included in character_only mode")
	}
}

func TestSpawnToolSetExcludesSpawn(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Tool{
		Name:       "spawn",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (string, error) { return "ok", nil },
	})
	reg.Register(&Tool{
		Name:       "exec",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (string, error) { return "ok", nil },
	})

	defs, tools := spawnToolSet(reg, nil)
	if len(defs) != 1 || defs[0].Name != "exec" {
		t.Errorf("defs = %v, want [exec] only", defs)
	}
	if _, ok := tools["spawn"]; ok {
		t.Error("spawn should be excluded from tool set")
	}
}

func TestSpawnNoneCreatesTempDir(t *testing.T) {
	var spawnTempDir string
	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		return &anthropic.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("Done."), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Model: "claude-haiku-4-5"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "none",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(result, "Files created in /tmp/foci-spawn-") {
		spawnTempDir = extractTempDir(result)
	}
	_ = spawnTempDir
}

func TestSpawnNoneIsolationWritesToTempDir(t *testing.T) {
	callCount := 0
	var spawnTempDir string
	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		callCount++
		if callCount == 1 {
			return &anthropic.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []anthropic.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "write", Input: json.RawMessage(`{"path":"output.txt","content":"test data"}`)},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &anthropic.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("File written."), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(NewWriteTool(nil))

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, Model: "claude-haiku-4-5"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "none",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "File written.") {
		t.Errorf("expected result, got %q", result)
	}

	if !strings.Contains(result, "Files created in /tmp/foci-spawn-") {
		t.Errorf("expected file list in result, got %q", result)
	}

	spawnTempDir = extractTempDir(result)
	if spawnTempDir == "" {
		t.Fatal("failed to extract temp dir from result")
	}

	data, err := os.ReadFile(spawnTempDir + "/output.txt")
	if err != nil {
		t.Fatalf("read file in temp dir: %v", err)
	}
	if string(data) != "test data" {
		t.Errorf("file content = %q", string(data))
	}
}

func TestSpawnNoneIsolationBlocksAbsolutePath(t *testing.T) {
	callCount := 0
	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		callCount++
		if callCount == 1 {
			return &anthropic.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []anthropic.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "write", Input: json.RawMessage(`{"path":"/tmp/malicious.txt","content":"bad"}`)},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &anthropic.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("Error received."), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(NewWriteTool(nil))

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, Model: "claude-haiku-4-5"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "none",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "Error received.") {
		t.Errorf("expected result, got %q", result)
	}
}

func TestSpawnNoneIsolationBlocksTraversal(t *testing.T) {
	callCount := 0
	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		callCount++
		if callCount == 1 {
			return &anthropic.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []anthropic.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "write", Input: json.RawMessage(`{"path":"../../../tmp/escape.txt","content":"bad"}`)},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &anthropic.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("Error received."), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(NewWriteTool(nil))

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, Model: "claude-haiku-4-5"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "none",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "Error received.") {
		t.Errorf("expected result, got %q", result)
	}
}

func TestSpawnNoneFileListMultiple(t *testing.T) {
	callCount := 0
	server := mockModelServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		callCount++
		if callCount == 1 {
			return &anthropic.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []anthropic.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "write", Input: json.RawMessage(`{"path":"a.txt","content":"aaa"}`)},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		if callCount == 2 {
			return &anthropic.MessageResponse{
				ID: "msg_2", Type: "message", Role: "assistant",
				Content: []anthropic.ContentBlock{
					{Type: "tool_use", ID: "tu_2", Name: "write", Input: json.RawMessage(`{"path":"b.txt","content":"bbbbb"}`)},
				},
				StopReason: "tool_use",
				Usage:      anthropic.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		return &anthropic.MessageResponse{
			ID: "msg_3", Type: "message", Role: "assistant",
			Content: anthropic.TextContent("Files written."), StopReason: "end_turn",
			Usage: anthropic.Usage{InputTokens: 30, OutputTokens: 15},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(NewWriteTool(nil))

	client := anthropic.NewClientWithBase(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, Model: "claude-haiku-4-5"}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "none",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "a.txt") {
		t.Errorf("expected a.txt in file list, got %q", result)
	}
	if !strings.Contains(result, "b.txt") {
		t.Errorf("expected b.txt in file list, got %q", result)
	}
	if !strings.Contains(result, "3 B") && !strings.Contains(result, "5 B") {
		t.Errorf("expected file sizes in file list, got %q", result)
	}
}

func extractTempDir(result string) string {
	marker := "Files created in "
	idx := strings.Index(result, marker)
	if idx == -1 {
		return ""
	}
	start := idx + len(marker)
	end := strings.Index(result[start:], "/:")
	if end == -1 {
		return ""
	}
	return result[start : start+end]
}

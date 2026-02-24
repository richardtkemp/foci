package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clod/anthropic"
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
	parentKey   string
	branchKey   string
	noResetHook bool
	err         error
}

func (m *mockSessionBrancher) CreateBranch(parentKey, branchKey string, noResetHook bool) error {
	m.parentKey = parentKey
	m.branchKey = branchKey
	m.noResetHook = noResetHook
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
	if !mockSessions.noResetHook {
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

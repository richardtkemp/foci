package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/provider"
)

// spawnTestAliases mirrors the default model aliases from config/load.go for tests.
var spawnTestAliases = map[string]string{
	"opus":   "anthropic/claude-opus-4-6",
	"sonnet": "anthropic/claude-sonnet-4-6",
	"haiku":  "anthropic/claude-haiku-4-5",
}

func TestSpawnContextRaw(t *testing.T) {
	// Proves that raw context sends no system prompt to the model, resolves the model group,
	// and returns the model's response directly.
	t.Parallel()
	var receivedReq *provider.MessageRequest

	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: provider.TextContent("The answer is 42."), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 50, OutputTokens: 20},
		}
	})
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-token")
	gr := config.NewGroupResolver(config.ModelsConfig{
		Powerful: "anthropic/claude-opus-4-6",
		Fast:     "anthropic/claude-sonnet-4-6",
		Cheap:    "anthropic/claude-haiku-4-5",
	}, spawnTestAliases)
	deps := SpawnDeps{
		Client: client,
		Bootstrap: &mockBootstrap{blocks: []provider.SystemBlock{
			{Type: "text", Text: "I am a character file."},
		}},
		GroupResolver:  gr,
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxToolLoops:   10,
	}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "What is the meaning of life?",
		"model":   "powerful",
		"context": "raw",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Text != "The answer is 42." {
		t.Errorf("result = %q", result.Text)
	}

	// No system prompt in raw mode
	if len(receivedReq.System) != 0 {
		t.Errorf("expected 0 system blocks (raw), got %d", len(receivedReq.System))
	}

	// Should resolve opus via group resolver
	if receivedReq.Model != "claude-opus-4-6" {
		t.Errorf("model = %q, want claude-opus-4-6", receivedReq.Model)
	}

	// Has tools (raw mode has tools)
}

func TestSpawnContextCharacter(t *testing.T) {
	// Proves that character context injects the full bootstrap system blocks into the request,
	// giving the spawned model the agent's identity and soul.
	t.Parallel()
	var receivedReq *provider.MessageRequest

	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: provider.TextContent("Deep analysis complete."), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 100, OutputTokens: 50},
		}
	})
	defer server.Close()

	client := newTestAnthropicClient(server.URL, "test-token")
	gr := config.NewGroupResolver(config.ModelsConfig{
		Powerful: "anthropic/claude-opus-4-6",
	}, spawnTestAliases)
	deps := SpawnDeps{
		Client: client,
		Bootstrap: &mockBootstrap{blocks: []provider.SystemBlock{
			{Type: "text", Text: "I am the identity file."},
			{Type: "text", Text: "I am the soul file."},
		}},
		GroupResolver:  gr,
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxToolLoops:   10,
	}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "Analyze this deeply",
		"model":   "opus",
		"context": "character",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Text != "Deep analysis complete." {
		t.Errorf("result = %q", result.Text)
	}

	// Character mode includes system blocks
	if len(receivedReq.System) != 2 {
		t.Fatalf("expected 2 system blocks (character), got %d", len(receivedReq.System))
	}
	if receivedReq.System[0].Text != "I am the identity file." {
		t.Errorf("system[0] = %q", receivedReq.System[0].Text)
	}
}

func TestSpawnContextClone(t *testing.T) {
	// With a notifier, inherit returns an async ack immediately.
	t.Parallel()
	called := make(chan string, 1)
	mockAgent := &channelSpawnAgent{
		response: "Task completed successfully.",
		called:   make(chan struct{}, 1),
	}
	mockSessions := &mockSessionBrancher{}
	notifier := NewAsyncNotifier(func(sk, msg, replyTo, trigger string) {
		called <- msg
	})

	deps := SpawnDeps{
		Sessions:       mockSessions,
		AgentID:        "test",
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxInherit:     3,
		Notifier:       notifier,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "test/imain/1000000000")

	params, _ := json.Marshal(map[string]string{
		"prompt":  "Do the research task",
		"context": "clone",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should return async ack, not the agent result
	if !strings.Contains(result.Text, "Spawn started in background") {
		t.Errorf("expected async ack, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "Branch: test/imain/1000000000/b") {
		t.Errorf("expected branch key in ack, got %q", result.Text)
	}

	// Should have created a branch
	if mockSessions.parentKey != "test/imain/1000000000" {
		t.Errorf("parent = %q, want test/imain/1000000000", mockSessions.parentKey)
	}
	if !strings.HasPrefix(mockSessions.branchKey, "test/imain/1000000000/b") {
		t.Errorf("branch = %q, want prefix test/imain/1000000000/b", mockSessions.branchKey)
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

func TestSpawnContextCloneDefault(t *testing.T) {
	// Proves that omitting the context parameter defaults to clone mode, and that a nil notifier
	// causes execution to be synchronous rather than async.
	t.Parallel()
	mockAgent := &mockSpawnAgent{response: "Done."}
	mockSessions := &mockSessionBrancher{}

	deps := SpawnDeps{
		Sessions:       mockSessions,
		AgentID:        "test",
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		MaxInherit:     3,
	}
	tool := NewSpawnTool(deps, func() SpawnAgent { return mockAgent })

	ctx := WithSessionKey(context.Background(), "test/imain/1000000000")
	params, _ := json.Marshal(map[string]string{
		"prompt": "Do something",
	})

	result, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify branch was created (meaning clone mode was used)
	if mockSessions.parentKey == "" {
		t.Error("expected branch creation (clone as default), but no branch was created")
	}

	// Nil notifier = sync fallback, should get direct result
	if result.Text != "Done." {
		t.Errorf("result = %q, want Done.", result.Text)
	}
}

func TestSpawnExploreMode(t *testing.T) {
	// Proves that explore context always uses haiku regardless of the requested model, injects
	// a read-only explorer system prompt, and provides exploration tools but not shell.
	t.Parallel()
	var receivedReq *provider.MessageRequest

	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: provider.TextContent("Found 3 Go files."), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 50, OutputTokens: 20},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(&Tool{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil },
	})
	reg.Register(&Tool{
		Name:       "shell",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil },
	})

	client := newTestAnthropicClient(server.URL, "test-token")
	gr := config.NewGroupResolver(config.ModelsConfig{
		Powerful: "anthropic/claude-opus-4-6",
		Fast:     "anthropic/claude-sonnet-4-6",
		Cheap:    "anthropic/claude-haiku-4-5",
	}, spawnTestAliases)
	deps := SpawnDeps{
		Client:          client,
		Registry:        reg,
		GroupResolver:   gr,
		FallbackModel:   "anthropic/claude-opus-4-6", // parent uses opus
		FallbackFormat:  "anthropic",
		ExploreMaxDepth: 10,
	}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "Find all Go files in the project",
		"model":   "powerful", // explicitly request powerful — explore ignores it, uses cheap group
		"context": "explore",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if result.Text != "Found 3 Go files." {
		t.Errorf("result = %q", result.Text)
	}

	// Should use cheap group (haiku) regardless of parent model or explicit model param
	if receivedReq.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q, want claude-haiku-4-5 (explore uses cheap group)", receivedReq.Model)
	}

	// Should have the explore system prompt
	if len(receivedReq.System) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(receivedReq.System))
	}
	if !strings.Contains(receivedReq.System[0].Text, "read-only code explorer") {
		t.Errorf("system[0] = %q, want explore system prompt", receivedReq.System[0].Text)
	}

	// Should have tools (ls, find, grep, read — but NOT exec)
	toolNames := make(map[string]bool)
	for _, td := range receivedReq.Tools {
		toolNames[td.Name()] = true
	}
	for _, expected := range []string{"ls", "find", "grep", "git", "read"} {
		if !toolNames[expected] {
			t.Errorf("expected %s in explore tools", expected)
		}
	}
	if toolNames["shell"] {
		t.Error("shell should not be in explore tools")
	}
}

func TestSpawnExploreToolSet(t *testing.T) {
	// Proves that the explore tool set includes all required exploration tools (ls, find, grep, git, read)
	// and excludes dangerous tools, with consistent defs and handler maps.
	t.Parallel()
	reg := NewRegistry()
	allRegistryTools := []string{
		"shell", "tmux",
		"read", "write", "edit",
		"web_fetch", "web_search", "http_request",
		"memory_search", "scratchpad", "todo",
		"bitwarden_search", "bitwarden_unlock",
		"send_to_chat", "send_to_session",
		"remind", "spawn",
	}
	for _, name := range allRegistryTools {
		reg.Register(&Tool{
			Name:       name,
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:    func(ctx context.Context, params json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil },
		})
	}

	defs, tools := spawnExploreToolSet(reg)

	// Build name sets.
	defNames := make(map[string]bool, len(defs))
	for _, d := range defs {
		defNames[d.Name()] = true
	}
	toolNames := make(map[string]bool, len(tools))
	for name := range tools {
		toolNames[name] = true
	}

	// Must include: ls, find, grep, git (exploration tools)
	for _, name := range []string{"ls", "find", "grep", "git"} {
		if !toolNames[name] {
			t.Errorf("expected exploration tool %q in explore mode", name)
		}
		if !defNames[name] {
			t.Errorf("expected exploration tool %q in explore defs", name)
		}
	}

	// Must include: allowed registry tools
	for name := range spawnExploreAllowed {
		if !toolNames[name] {
			t.Errorf("expected allowed tool %q in explore mode", name)
		}
		if !defNames[name] {
			t.Errorf("expected allowed tool %q in explore defs", name)
		}
	}

	// Must exclude: dangerous tools
	excluded := []string{
		"shell", "write", "edit", "spawn", "send_to_chat",
		"send_to_session", "scratchpad", "remind",
		"http_request", "tmux", "bitwarden_search", "bitwarden_unlock",
	}
	for _, name := range excluded {
		if toolNames[name] {
			t.Errorf("tool %q should be excluded from explore mode", name)
		}
		if defNames[name] {
			t.Errorf("tool %q should not appear in explore defs", name)
		}
	}

	// Verify conditional tools appear iff binary is in PATH.
	for _, opt := range optionalExploreTools {
		_, lookupErr := exec.LookPath(opt.binary)
		available := lookupErr == nil
		tool := opt.create("/dummy")
		if available && !toolNames[tool.Name] {
			t.Errorf("conditional tool %q should be present (binary %q found in PATH)", tool.Name, opt.binary)
		}
		if !available && toolNames[tool.Name] {
			t.Errorf("conditional tool %q should NOT be present (binary %q not in PATH)", tool.Name, opt.binary)
		}
	}

	// Verify defs and tools map are consistent.
	for _, d := range defs {
		if _, ok := tools[d.Name()]; !ok {
			t.Errorf("tool %q has a def but no handler", d.Name())
		}
	}
	for name := range tools {
		if !defNames[name] {
			t.Errorf("tool %q has a handler but no def", name)
		}
	}
}

func TestSpawnExploreToolAllowlist(t *testing.T) {
	// Exhaustive audit: every known tool must be explicitly classified as either allowed
	// or excluded for explore mode to prevent accidental exposure of dangerous capabilities.
	t.Parallel()
	// If you add a new tool and this test fails, you MUST decide:
	//   - Is the tool safe for read-only exploration (no mutation, no messaging)?
	//     Add it to spawnExploreAllowed in spawn.go.
	//   - Otherwise, add it to blockedInExplore below.

	// Tools blocked from explore mode (everything not in the allowlist).
	blockedInExplore := map[string]bool{
		"shell":            true,
		"tmux":             true,
		"write":            true,
		"edit":             true,
		"http_request":     true,
		"send_to_chat":    true,
		"send_to_session":  true,
		"scratchpad":       true,
		"remind":           true,
		"bitwarden_search": true,
		"bitwarden_unlock": true,
		"spawn":            true,
	}

	// Every tool in the real system.
	allTools := []string{
		"shell", "tmux",
		"read", "write", "edit",
		"web_fetch", "web_search", "http_request",
		"memory_search", "scratchpad", "todo",
		"bitwarden_search", "bitwarden_unlock",
		"send_to_chat", "send_to_session",
		"remind", "spawn",
	}

	for _, name := range allTools {
		_, isAllowed := spawnExploreAllowed[name]
		_, isBlocked := blockedInExplore[name]
		if !isAllowed && !isBlocked {
			t.Errorf("tool %q is neither allowed nor blocked for explore mode — "+
				"add it to spawnExploreAllowed (if safe) or blockedInExplore in this test (if not)", name)
		}
		if isAllowed && isBlocked {
			t.Errorf("tool %q is both allowed and blocked for explore mode — resolve the conflict", name)
		}
	}

	// ls, find, grep are NOT in the main registry — they're created fresh
	// inside spawnExploreToolSet. They don't need classification here.
}

package tools

import (
	"context"
	"encoding/json"
	"testing"

	"foci/internal/provider"
)

func TestSpawnOneShotWithTools(t *testing.T) {
	// Verify one-shot modes get tool definitions and can execute tools.
	t.Parallel()
	callCount := 0
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount++
		if callCount == 1 {
			// First call: model uses a tool
			if len(req.Tools) == 0 {
				t.Error("expected tools in request")
			}
			return &provider.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "echo_tool", Input: json.RawMessage(`{"text":"hello"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		// Second call: model returns final text
		return &provider.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant",
			Content:    provider.TextContent("Tool said: echo hello"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	reg.Register(&Tool{
		Name:       "echo_tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			var p struct {
				Text string `json:"text"`
			}
			json.Unmarshal(params, &p)
			return TextResult("echo: " + p.Text), nil
		},
	})

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: 10}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "character",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Text != "Tool said: echo hello" {
		t.Errorf("result = %q", result.Text)
	}
	if callCount != 2 {
		t.Errorf("callCount = %d, want 2", callCount)
	}
}

func TestSpawnRawToolAllowlist(t *testing.T) {
	// This test ensures every tool registered in the system is explicitly
	t.Parallel()
	// classified as either allowed or blocked for raw-mode spawns.
	// If you add a new tool and this test fails, you MUST decide:
	//   - Is the tool safe in an isolated sandbox (no shell access, no
	//     external communication)? Add it to allowedInRaw.
	//   - Can it escape the sandbox or communicate externally?
	//     Add it to spawnRawBlacklist in spawn.go.

	// Tools that should be available in raw-mode spawns.
	// These are safe within the file-tool sandbox (no shell access,
	// no external communication, no sandbox escape).
	allowedInRaw := map[string]bool{
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
		"shell", "tmux",
		"read", "write", "edit",
		"web_fetch", "web_search", "http_request",
		"memory_search", "scratchpad", "todo",
		"bitwarden_search", "bitwarden_unlock",
		"send_to_chat", "send_to_session",
		"remind", "spawn",
	}
	for _, name := range allTools {
		reg.Register(&Tool{
			Name:       name,
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:    func(ctx context.Context, params json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil },
		})
	}

	defs, tools := spawnIsolatedToolSet(reg, spawnRawBlacklist, nil, "/tmp/test-sandbox", 0640)

	// Build a set of tool names present in the API schema (defs).
	defNames := make(map[string]bool, len(defs))
	for _, d := range defs {
		defNames[d.Name()] = true
	}

	// Verify every tool is either allowed or blocked — no unclassified tools.
	for _, name := range allTools {
		if name == "spawn" {
			// spawn is always excluded (hardcoded in spawnIsolatedToolSet)
			if _, ok := tools[name]; ok {
				t.Errorf("spawn should never be included in spawn tool sets")
			}
			if defNames[name] {
				t.Errorf("spawn should not appear in tool schema")
			}
			continue
		}
		_, isAllowed := tools[name]
		_, isBlocked := spawnRawBlacklist[name]
		if !isAllowed && !isBlocked {
			t.Errorf("tool %q is neither allowed nor blacklisted for raw-mode spawns — "+
				"add it to allowedInRaw in this test (if safe) or spawnRawBlacklist in spawn.go (if not)", name)
		}
		if isAllowed && isBlocked {
			t.Errorf("tool %q is both allowed and blacklisted — check spawnRawBlacklist", name)
		}
	}

	// Verify the exact set of allowed tools matches expectations.
	for name := range allowedInRaw {
		if _, ok := tools[name]; !ok {
			t.Errorf("tool %q should be allowed in raw-mode but is missing", name)
		}
	}
	for name := range tools {
		if !allowedInRaw[name] {
			t.Errorf("tool %q is available in raw-mode but not in allowedInRaw — "+
				"either add it to allowedInRaw (if safe) or to spawnRawBlacklist (if not)", name)
		}
	}

	// Verify blacklisted tools are excluded from BOTH the tools map and the
	// API schema (defs). Previously only the tools map was checked, so a
	// blacklisted tool could still appear in the schema sent to the model.
	for name := range spawnRawBlacklist {
		if _, ok := tools[name]; ok {
			t.Errorf("tool %q is blacklisted but still available in raw-mode tools map", name)
		}
		if defNames[name] {
			t.Errorf("tool %q is blacklisted but still appears in raw-mode tool schema", name)
		}
	}

	// Verify defs and tools map are consistent — every def has a handler.
	for _, d := range defs {
		if _, ok := tools[d.Name()]; !ok {
			t.Errorf("tool %q has a schema definition but no handler in tools map", d.Name())
		}
	}
	for name := range tools {
		if !defNames[name] {
			t.Errorf("tool %q has a handler but no schema definition", name)
		}
	}
}

func TestSpawnCharacterTools(t *testing.T) {
	// Proves character context mode sends registered tools to the model and
	// keeps send_to_chat, but excludes send_to_session: a one-shot character
	// spawn has no persistent session of its own, so it must not be able to
	// inject into other sessions (raw/explore already exclude it).
	t.Parallel()
	var receivedReq *provider.MessageRequest
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID: "msg_test", Type: "message", Role: "assistant",
			Content: provider.TextContent("ok"), StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	reg := NewRegistry()
	for _, name := range []string{"web_search", "send_to_chat", "send_to_session", "shell"} {
		reg.Register(&Tool{
			Name:       name,
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Execute:    func(ctx context.Context, params json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil },
		})
	}

	client := newTestAnthropicClient(server.URL, "test-token")
	deps := SpawnDeps{Client: client, Registry: reg, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: 10}
	tool := NewSpawnTool(deps, nil)

	params, _ := json.Marshal(map[string]string{
		"prompt":  "test",
		"context": "character",
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	toolNames := make(map[string]bool)
	for _, td := range receivedReq.Tools {
		toolNames[td.Name()] = true
	}

	if !toolNames["send_to_chat"] {
		t.Error("send_to_chat should be included in character mode")
	}
	if toolNames["send_to_session"] {
		t.Error("send_to_session should be excluded from character mode (ephemeral one-shot spawn)")
	}
}

func TestSpawnToolSetExcludesSpawn(t *testing.T) {
	// Proves that the spawn tool is always excluded from the tool set passed to spawned agents,
	// preventing recursive spawn chains.
	t.Parallel()
	reg := NewRegistry()
	reg.Register(&Tool{
		Name:       "spawn",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil },
	})
	reg.Register(&Tool{
		Name:       "shell",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil },
	})

	defs, tools := spawnToolSet(reg, nil)
	if len(defs) != 1 || defs[0].Name() != "shell" {
		t.Errorf("defs = %v, want [shell] only", defs)
	}
	if _, ok := tools["spawn"]; ok {
		t.Error("spawn should be excluded from tool set")
	}
}

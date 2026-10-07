package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/modelinfo"
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
	deps := SpawnDeps{Client: client, Registry: reg, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: func() int { return 10 }}
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

	// whoami must be classified too: a one-shot spawn has no session of
	// its own (its tool calls run on the parent's context), so whoami
	// would report the parent's identity — it belongs in the blacklist.
	// On code missing that entry this fails as "neither allowed nor
	// blacklisted", which is exactly the drift this test exists to catch.

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
		"remind", "spawn", "whoami",
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

// TestSpawnOneShotToolSetsExcludeWhoami proves whoami is excluded from both
// one-shot spawn tool sets: a one-shot spawn has no session of its own and
// its tool calls run on the parent's context, so whoami would report the
// parent's session key, chat and model as if they were the spawn's (#2226).
// It must be absent from both the defs and the tools map of each set, while
// an ordinary tool (read) still passes both filters.
func TestSpawnOneShotToolSetsExcludeWhoami(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(NewWhoamiTool(WhoamiDeps{}))
	reg.Register(&Tool{
		Name:       "read",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute:    func(ctx context.Context, params json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil },
	})

	type toolSet struct {
		mode  string
		defs  []provider.ToolDef
		tools map[string]*Tool
	}
	rawDefs, rawTools := spawnIsolatedToolSet(reg, spawnRawBlacklist, nil, t.TempDir(), 0o640)
	charDefs, charTools := spawnToolSet(reg, spawnCharacterBlacklist)
	for _, s := range []toolSet{{"raw", rawDefs, rawTools}, {"character", charDefs, charTools}} {
		defNames := make(map[string]bool, len(s.defs))
		for _, d := range s.defs {
			defNames[d.Name()] = true
		}
		if _, ok := s.tools["whoami"]; ok {
			t.Errorf("%s mode: whoami is in the tools map — a one-shot would report the parent's identity", s.mode)
		}
		if defNames["whoami"] {
			t.Errorf("%s mode: whoami is in the defs — the spawned model could call it", s.mode)
		}
		if _, ok := s.tools["read"]; !ok {
			t.Errorf("%s mode: read (control) missing from tools map — the exclusion is over-broad", s.mode)
		}
		if !defNames["read"] {
			t.Errorf("%s mode: read (control) missing from defs — the exclusion is over-broad", s.mode)
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
	deps := SpawnDeps{Client: client, Registry: reg, FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: func() int { return 10 }}
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

// spawnWithLedger runs a two-loop one-shot spawn from session gil/c1 under ctx
// with a live ledger, and returns the ledger's calls.
func spawnWithLedger(t *testing.T, ctx context.Context) (*accounting.Ledger, []accounting.CallRow) {
	t.Helper()
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })

	calls := 0
	server := mockModelServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		calls++
		if calls == 1 {
			return &provider.MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant",
				Content:    []provider.ContentBlock{{Type: "tool_use", ID: "tu_1", Name: "echo_tool", Input: json.RawMessage(`{}`)}},
				StopReason: "tool_use", Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		}
		return &provider.MessageResponse{
			ID: "msg_2", Type: "message", Role: "assistant", Content: provider.TextContent("done"),
			StopReason: "end_turn", Usage: provider.Usage{InputTokens: 20, OutputTokens: 10, CacheCreationInputTokens: 7},
		}
	})
	t.Cleanup(server.Close)
	reg := NewRegistry()
	reg.Register(&Tool{
		Name: "echo_tool", Parameters: json.RawMessage(`{"type":"object"}`),
		Execute: func(context.Context, json.RawMessage) (ToolResult, error) { return TextResult("ok"), nil },
	})
	deps := SpawnDeps{Client: newTestAnthropicClient(server.URL, "test-token"), Registry: reg,
		FallbackModel: "anthropic/claude-haiku-4-5", FallbackFormat: "anthropic", MaxToolLoops: func() int { return 10 }}
	params, _ := json.Marshal(map[string]string{"prompt": "test", "context": "character"})
	if _, err := NewSpawnTool(deps, nil).Execute(WithSessionKey(ctx, "gil/c1"), params); err != nil {
		t.Fatal(err)
	}
	rows, err := l.Calls(time.Time{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("ledger calls = %d (%v), want one per loop", len(rows), err)
	}
	return l, rows
}

// TestSpawnBooksOntoTheInvokingTurn: a spawn a conversation turn's tool made
// is that turn's spend — each loop is its own spawn call, booked on the
// invoking turn (#2111; Dick: "subagent turns should be tied to their
// parent"). Not parallel: it makes a ledger the live one.
func TestSpawnBooksOntoTheInvokingTurn(t *testing.T) {
	parent := accounting.Turn{TurnID: "gil/c1@42", Session: "gil/c1", AgentID: "gil",
		Backend: accounting.BackendAPI, Source: accounting.SourceUser, StartedAt: time.Now()}
	l, rows := spawnWithLedger(t, accounting.WithTurn(context.Background(), parent))
	for _, r := range rows {
		if r.Kind != accounting.KindSpawn || r.TurnID != parent.TurnID {
			t.Errorf("call = %s on %q, want a spawn call on the invoking turn %q", r.Kind, r.TurnID, parent.TurnID)
		}
	}
	if st, err := l.SessionStats("gil/c1"); err != nil || st.TurnCount != 1 || st.TotalCalls != 2 {
		t.Errorf("session stats = %+v (%v): one turn, the parent, holding both calls", st, err)
	}
}

// TestSpawnBooksEachCallOnOneSystemTurn: a spawn no turn made (no turn on
// its context) books every loop as a spawn call on one system turn of its own.
// Not parallel: it makes a ledger the live one.
func TestSpawnBooksEachCallOnOneSystemTurn(t *testing.T) {
	l, rows := spawnWithLedger(t, context.Background())
	for _, r := range rows {
		if r.Kind != accounting.KindSpawn || r.TurnID != rows[0].TurnID || !strings.HasSuffix(r.TurnID, ":spawn") {
			t.Errorf("call = %+v, want spawn calls on one spawn turn", r)
		}
	}
	if w := rows[1].Count(modelinfo.ClassCacheWrite1h); w != 7 {
		t.Errorf("second call's 1h writes = %d, want 7 (direct-API writes are 1h)", w)
	}
	if st, err := l.SessionStats("gil/c1"); err != nil || st.TurnCount != 0 || st.TotalCalls != 2 {
		t.Errorf("session stats = %+v (%v): a spawn's own turn is a system turn", st, err)
	}
}

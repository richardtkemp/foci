//go:build ignore
// Content below is fully disabled (no kept tests); Step 9+ replaces with fresh tests.
package opencode

import (
	"encoding/json"
	"testing"
)

// DISABLED(opencode): asserts ccstream's UserMessage wire shape — opencode defines its own outbound message types in Step 2.4 of the plan, with fresh round-trip tests.
// func TestUserMessageJSON(t *testing.T) {
// 	t.Parallel()
//
// 	msg := NewUserMessage("hello")
// 	data, err := json.Marshal(msg)
// 	if err != nil {
// 		t.Fatalf("marshal: %v", err)
// 	}
//
// 	var got map[string]any
// 	if err := json.Unmarshal(data, &got); err != nil {
// 		t.Fatalf("unmarshal round-trip: %v", err)
// 	}
//
// 	if got["type"] != "user" {
// 		t.Errorf("type = %v, want %q", got["type"], "user")
// 	}
//
// 	message, ok := got["message"].(map[string]any)
// 	if !ok {
// 		t.Fatalf("message is not an object: %T", got["message"])
// 	}
// 	if message["role"] != "user" {
// 		t.Errorf("message.role = %v, want %q", message["role"], "user")
// 	}
// 	if message["content"] != "hello" {
// 		t.Errorf("message.content = %v, want %q", message["content"], "hello")
// 	}
//
// 	// parent_tool_use_id should be absent (omitempty with nil pointer).
// 	if _, present := got["parent_tool_use_id"]; present {
// 		t.Errorf("parent_tool_use_id should be omitted for nil pointer, got %v", got["parent_tool_use_id"])
// 	}
// }

// DISABLED(opencode): asserts ccstream's ContentBlock wire shape — opencode defines its own content/part types in Step 2.4, with fresh round-trip tests.
// func TestUserMessageBlocksJSON(t *testing.T) {
// 	t.Parallel()
//
// 	blocks := []ContentBlock{
// 		{Type: "text", Text: "first"},
// 		{Type: "text", Text: "second"},
// 	}
// 	msg := NewUserMessageBlocks(blocks)
// 	data, err := json.Marshal(msg)
// 	if err != nil {
// 		t.Fatalf("marshal: %v", err)
// 	}
//
// 	var got map[string]any
// 	if err := json.Unmarshal(data, &got); err != nil {
// 		t.Fatalf("unmarshal round-trip: %v", err)
// 	}
//
// 	message, ok := got["message"].(map[string]any)
// 	if !ok {
// 		t.Fatalf("message is not an object: %T", got["message"])
// 	}
//
// 	content, ok := message["content"].([]any)
// 	if !ok {
// 		t.Fatalf("message.content is not an array: %T", message["content"])
// 	}
// 	if len(content) != 2 {
// 		t.Fatalf("content length = %d, want 2", len(content))
// 	}
//
// 	first, ok := content[0].(map[string]any)
// 	if !ok {
// 		t.Fatalf("content[0] is not an object: %T", content[0])
// 	}
// 	if first["text"] != "first" {
// 		t.Errorf("content[0].text = %v, want %q", first["text"], "first")
// 	}
// }

// DISABLED(opencode): asserts ccstream's ResultMessage wire type — opencode has no equivalent (turn completion is a session.updated event); replaced in Step 7.
// func TestResultMessageUnmarshal(t *testing.T) {
// 	t.Parallel()
//
// 	const raw = `{
// 		"type": "result",
// 		"subtype": "success",
// 		"is_error": false,
// 		"duration_ms": 12345,
// 		"duration_api_ms": 9000,
// 		"num_turns": 3,
// 		"result": "Here is the answer.",
// 		"total_cost_usd": 0.0042,
// 		"usage": {
// 			"input_tokens": 1000,
// 			"output_tokens": 250,
// 			"cache_read_input_tokens": 500,
// 			"cache_creation_input_tokens": 100
// 		},
// 		"modelUsage": {
// 			"claude-sonnet-4-20250514": {
// 				"inputTokens": 1000,
// 				"outputTokens": 250,
// 				"cacheReadInputTokens": 500,
// 				"cacheCreationInputTokens": 100,
// 				"costUSD": 0.0042,
// 				"contextWindow": 200000,
// 				"maxOutputTokens": 16384
// 			}
// 		},
// 		"session_id": "sess-abc123"
// 	}`
//
// 	var msg ResultMessage
// 	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
//
// 	if msg.Subtype != "success" {
// 		t.Errorf("subtype = %q, want %q", msg.Subtype, "success")
// 	}
// 	if msg.IsError {
// 		t.Errorf("is_error = true, want false")
// 	}
// 	if msg.Result != "Here is the answer." {
// 		t.Errorf("result = %q, want %q", msg.Result, "Here is the answer.")
// 	}
// 	if msg.TotalCostUSD != 0.0042 {
// 		t.Errorf("total_cost_usd = %v, want 0.0042", msg.TotalCostUSD)
// 	}
// 	if msg.Usage.InputTokens != 1000 {
// 		t.Errorf("usage.input_tokens = %d, want 1000", msg.Usage.InputTokens)
// 	}
// 	if msg.Usage.OutputTokens != 250 {
// 		t.Errorf("usage.output_tokens = %d, want 250", msg.Usage.OutputTokens)
// 	}
//
// 	mu, ok := msg.ModelUsage["claude-sonnet-4-20250514"]
// 	if !ok {
// 		t.Fatal("modelUsage missing claude-sonnet-4-20250514")
// 	}
// 	if mu.ContextWindow != 200000 {
// 		t.Errorf("modelUsage.contextWindow = %d, want 200000", mu.ContextWindow)
// 	}
// 	if mu.MaxOutputTokens != 16384 {
// 		t.Errorf("modelUsage.maxOutputTokens = %d, want 16384", mu.MaxOutputTokens)
// 	}
// }

// DISABLED(opencode): asserts ccstream's AssistantMessage / BetaMessage / ContentBlock wire types — opencode surfaces assistant content via message.part.updated events (Step 7), with fresh round-trip tests on new types.
// func TestAssistantMessageUnmarshal(t *testing.T) {
// 	t.Parallel()
//
// 	const raw = `{
// 		"type": "assistant",
// 		"message": {
// 			"id": "msg_01XYZ",
// 			"role": "assistant",
// 			"content": [
// 				{"type": "text", "text": "Let me check that file."},
// 				{"type": "tool_use", "id": "toolu_01ABC", "name": "Read", "input": {"file_path": "/tmp/test.txt"}}
// 			],
// 			"model": "claude-sonnet-4-20250514",
// 			"stop_reason": "tool_use",
// 			"usage": {
// 				"input_tokens": 500,
// 				"output_tokens": 120,
// 				"cache_read_input_tokens": 300,
// 				"cache_creation_input_tokens": 0
// 			}
// 		},
// 		"session_id": "sess-abc123"
// 	}`
//
// 	var msg AssistantMessage
// 	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
//
// 	if msg.Message.Model != "claude-sonnet-4-20250514" {
// 		t.Errorf("model = %q, want %q", msg.Message.Model, "claude-sonnet-4-20250514")
// 	}
// 	if msg.Message.StopReason == nil || *msg.Message.StopReason != "tool_use" {
// 		t.Errorf("stop_reason = %v, want %q", msg.Message.StopReason, "tool_use")
// 	}
// 	if len(msg.Message.Content) != 2 {
// 		t.Fatalf("content length = %d, want 2", len(msg.Message.Content))
// 	}
// 	if msg.Message.Content[0].Type != "text" {
// 		t.Errorf("content[0].type = %q, want %q", msg.Message.Content[0].Type, "text")
// 	}
// 	if msg.Message.Content[1].Type != "tool_use" {
// 		t.Errorf("content[1].type = %q, want %q", msg.Message.Content[1].Type, "tool_use")
// 	}
// 	if msg.Message.Content[1].Name != "Read" {
// 		t.Errorf("content[1].name = %q, want %q", msg.Message.Content[1].Name, "Read")
// 	}
// }

// DISABLED(opencode): asserts ccstream's PermissionRequest / PermSuggestion wire types — opencode surfaces tool permissions via permission.updated events (Step 9), with fresh round-trip tests on new types.
// func TestPermissionRequestUnmarshal(t *testing.T) {
// 	t.Parallel()
//
// 	const raw = `{
// 		"type": "control_request",
// 		"request_id": "req-999",
// 		"request": {
// 			"subtype": "can_use_tool",
// 			"tool_name": "Bash",
// 			"input": {"command": "rm -rf /"},
// 			"tool_use_id": "toolu_01DEF",
// 			"permission_suggestions": [
// 				{"prefix": "Bash(/home/user)", "scope": "session"}
// 			],
// 			"description": "Execute a bash command",
// 			"display_name": "Bash",
// 			"title": "rm -rf /"
// 		}
// 	}`
//
// 	var msg PermissionRequest
// 	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
//
// 	if msg.Request.ToolName != "Bash" {
// 		t.Errorf("tool_name = %q, want %q", msg.Request.ToolName, "Bash")
// 	}
// 	if msg.Request.Description != "Execute a bash command" {
// 		t.Errorf("description = %q, want %q", msg.Request.Description, "Execute a bash command")
// 	}
// 	if len(msg.Request.PermissionSuggestions) != 1 {
// 		t.Fatalf("permission_suggestions length = %d, want 1", len(msg.Request.PermissionSuggestions))
// 	}
// 	if msg.Request.PermissionSuggestions[0].Prefix != "Bash(/home/user)" {
// 		t.Errorf("suggestion prefix = %q, want %q", msg.Request.PermissionSuggestions[0].Prefix, "Bash(/home/user)")
// 	}
// 	if msg.Request.PermissionSuggestions[0].Scope != "session" {
// 		t.Errorf("suggestion scope = %q, want %q", msg.Request.PermissionSuggestions[0].Scope, "session")
// 	}
// }

// DISABLED(opencode): asserts ccstream's StdoutEnvelope NDJSON discriminator — opencode has no envelope; SSE event types from /event replace it (Step 4).
// func TestStdoutEnvelopeDiscrimination(t *testing.T) {
// 	t.Parallel()
//
// 	cases := []struct {
// 		name        string
// 		json        string
// 		wantType    string
// 		wantSubtype string
// 	}{
// 		{
// 			name:     "assistant",
// 			json:     `{"type":"assistant","message":{"id":"msg_01","role":"assistant","content":[],"model":"claude-sonnet-4-20250514","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
// 			wantType: "assistant",
// 		},
// 		{
// 			name:     "result",
// 			json:     `{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"duration_api_ms":80,"num_turns":1,"result":"done","total_cost_usd":0.001,"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`,
// 			wantType: "result",
// 		},
// 		{
// 			name:        "system with subtype",
// 			json:        `{"type":"system","subtype":"init","claude_code_version":"1.0","cwd":"/tmp","model":"claude-sonnet-4-20250514","permissionMode":"default","tools":["Bash"]}`,
// 			wantType:    "system",
// 			wantSubtype: "init",
// 		},
// 		{
// 			name:     "control_request",
// 			json:     `{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{},"tool_use_id":"t1"}}`,
// 			wantType: "control_request",
// 		},
// 		{
// 			name:     "tool_progress",
// 			json:     `{"type":"tool_progress","tool_use_id":"t1","tool_name":"Bash","elapsed_time_seconds":5}`,
// 			wantType: "tool_progress",
// 		},
// 	}
//
// 	for _, tc := range cases {
// 		t.Run(tc.name, func(t *testing.T) {
// 			t.Parallel()
//
// 			var env StdoutEnvelope
// 			if err := json.Unmarshal([]byte(tc.json), &env); err != nil {
// 				t.Fatalf("unmarshal: %v", err)
// 			}
// 			if env.Type != tc.wantType {
// 				t.Errorf("type = %q, want %q", env.Type, tc.wantType)
// 			}
// 			if tc.wantSubtype != "" && env.Subtype != tc.wantSubtype {
// 				t.Errorf("subtype = %q, want %q", env.Subtype, tc.wantSubtype)
// 			}
// 		})
// 	}
// }

// DISABLED(opencode): asserts ccstream's InitMessage wire type (with ClaudeCodeVersion) — opencode has no init handshake; session.created replaces it (Step 4), fresh round-trip tests on new types.
// func TestInitMessageUnmarshal(t *testing.T) {
// 	t.Parallel()
//
// 	const raw = `{
// 		"type": "system",
// 		"subtype": "init",
// 		"claude_code_version": "1.0.27",
// 		"cwd": "/home/user/project",
// 		"model": "claude-sonnet-4-20250514",
// 		"permissionMode": "default",
// 		"tools": ["Bash", "Read", "Write", "Edit", "Glob", "Grep"],
// 		"session_id": "sess-init-001"
// 	}`
//
// 	var msg InitMessage
// 	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
// 		t.Fatalf("unmarshal: %v", err)
// 	}
//
// 	if msg.Model != "claude-sonnet-4-20250514" {
// 		t.Errorf("model = %q, want %q", msg.Model, "claude-sonnet-4-20250514")
// 	}
// 	if msg.SessionID != "sess-init-001" {
// 		t.Errorf("session_id = %q, want %q", msg.SessionID, "sess-init-001")
// 	}
// 	if len(msg.Tools) != 6 {
// 		t.Fatalf("tools length = %d, want 6", len(msg.Tools))
// 	}
// 	if msg.Tools[0] != "Bash" {
// 		t.Errorf("tools[0] = %q, want %q", msg.Tools[0], "Bash")
// 	}
// 	if msg.Tools[5] != "Grep" {
// 		t.Errorf("tools[5] = %q, want %q", msg.Tools[5], "Grep")
// 	}
// 	if msg.ClaudeCodeVersion != "1.0.27" {
// 		t.Errorf("claude_code_version = %q, want %q", msg.ClaudeCodeVersion, "1.0.27")
// 	}
// }

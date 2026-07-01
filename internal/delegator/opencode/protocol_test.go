package opencode

import (
	"encoding/json"
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// Round-trip tests for the OpenCode wire types in protocol.go.
//
// Each test parses a representative JSON blob (captured from the shape
// documented at https://opencode.ai/docs/server / types.gen.ts) and
// asserts field extraction. Together they pin the JSON tags and verify
// the discriminated-union dispatch works the way the SSE subscriber
// (Step 4) and event handlers (Step 7) will rely on.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

func TestSession_Unmarshal(t *testing.T) {
	// Verifies Session pulls id/title/time.{created,updated} from the wire
	// shape documented at /docs/server/#project.
	raw := `{
		"id": "sess-abc",
		"title": "My session",
		"time": {"created": 1719523200000, "updated": 1719526800000}
	}`
	var s Session
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.ID != "sess-abc" {
		t.Errorf("ID = %q, want sess-abc", s.ID)
	}
	if s.Title != "My session" {
		t.Errorf("Title = %q, want %q", s.Title, "My session")
	}
	if s.Time.Created == 0 {
		t.Error("Time.Created not parsed")
	}
}

// ---------------------------------------------------------------------------
// Message — user vs assistant discrimination
// ---------------------------------------------------------------------------

func TestMessage_UnmarshalUser(t *testing.T) {
	// Verifies the user-message shape parses; assistant-only fields stay
	// zero-valued.
	raw := `{
		"id": "msg-u1",
		"sessionID": "sess-abc",
		"role": "user",
		"agent": "build",
		"model": {"providerID": "anthropic", "modelID": "claude-3-5-sonnet"},
		"time": {"created": 1719523200000}
	}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Role != "user" {
		t.Errorf("Role = %q, want user", m.Role)
	}
	if m.ID != "msg-u1" {
		t.Errorf("ID = %q", m.ID)
	}
	if m.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q", m.SessionID)
	}
	if m.Tokens != nil {
		t.Errorf("Tokens should be nil for user message, got %+v", m.Tokens)
	}
	if m.Finish != "" {
		t.Errorf("Finish should be empty for user message, got %q", m.Finish)
	}
}

func TestMessage_UnmarshalAssistant(t *testing.T) {
	// Verifies the assistant-message shape parses with tokens, cost, and
	// finish_reason all extracted. This is the message.updated payload the
	// Step 7 OnMessageUpdated handler reads.
	raw := `{
		"id": "msg-a1",
		"sessionID": "sess-abc",
		"role": "assistant",
		"parentID": "msg-u1",
		"modelID": "claude-sonnet-4",
		"providerID": "anthropic",
		"mode": "build",
		"finish": "stop",
		"cost": 0.0042,
		"tokens": {
			"input": 1234,
			"output": 567,
			"reasoning": 0,
			"cache": {"read": 8900, "write": 1100}
		},
		"time": {"created": 1719523201000, "completed": 1719523210000}
	}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Role != "assistant" {
		t.Errorf("Role = %q, want assistant", m.Role)
	}
	if m.ModelID != "claude-sonnet-4" {
		t.Errorf("ModelID = %q", m.ModelID)
	}
	if m.ProviderID != "anthropic" {
		t.Errorf("ProviderID = %q", m.ProviderID)
	}
	if m.Finish != "stop" {
		t.Errorf("Finish = %q, want stop", m.Finish)
	}
	if m.Cost != 0.0042 {
		t.Errorf("Cost = %v, want 0.0042", m.Cost)
	}
	if m.Tokens == nil {
		t.Fatal("Tokens nil")
	}
	if m.Tokens.Input != 1234 {
		t.Errorf("Tokens.Input = %d, want 1234", m.Tokens.Input)
	}
	if m.Tokens.Output != 567 {
		t.Errorf("Tokens.Output = %d, want 567", m.Tokens.Output)
	}
	if m.Tokens.Cache.Read != 8900 {
		t.Errorf("Tokens.Cache.Read = %d, want 8900", m.Tokens.Cache.Read)
	}
	if m.Tokens.Cache.Write != 1100 {
		t.Errorf("Tokens.Cache.Write = %d, want 1100", m.Tokens.Cache.Write)
	}
	if m.Time.Created == 0 {
		t.Error("Time.Created not parsed")
	}
	if m.Time.Completed == 0 {
		t.Error("Time.Completed not parsed")
	}
}

func TestMessage_UnmarshalWithError(t *testing.T) {
	// Verifies a Message carrying an error payload (e.g. ProviderAuthError
	// on a 401) parses and exposes the discriminator Name.
	raw := `{
		"id": "msg-a-err",
		"sessionID": "sess-abc",
		"role": "assistant",
		"error": {
			"name": "ProviderAuthError",
			"data": {"providerID": "anthropic", "message": "invalid api key"}
		},
		"time": {"created": 1719523200000}
	}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Error == nil {
		t.Fatal("Error nil")
	}
	if m.Error.Name != ErrProviderAuth {
		t.Errorf("Error.Name = %q, want %q", m.Error.Name, ErrProviderAuth)
	}
	// Decode the typed payload.
	var data ProviderAuthErrorData
	if err := json.Unmarshal(m.Error.Data, &data); err != nil {
		t.Fatalf("decode ProviderAuthErrorData: %v", err)
	}
	if data.ProviderID != "anthropic" {
		t.Errorf("ProviderID = %q", data.ProviderID)
	}
	if data.Message != "invalid api key" {
		t.Errorf("Message = %q", data.Message)
	}
}

// ---------------------------------------------------------------------------
// Part — each variant
// ---------------------------------------------------------------------------

func TestPart_Text(t *testing.T) {
	raw := `{
		"id": "pt-text",
		"sessionID": "sess-abc",
		"messageID": "msg-a1",
		"type": "text",
		"text": "Hello world",
		"time": {"start": 1719523201000, "end": 1719523202000}
	}`
	var p Part
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != PartText {
		t.Errorf("Type = %q, want %q", p.Type, PartText)
	}
	if p.Text != "Hello world" {
		t.Errorf("Text = %q", p.Text)
	}
	if p.Time == nil || p.Time.End == 0 {
		t.Error("Time.End not parsed")
	}
}

func TestPart_TextSyntheticIgnored(t *testing.T) {
	// Verifies synthetic:true / ignored:true flags round-trip — these
	// gate whether Step 7 fires OnText.
	raw := `{
		"id": "pt-syn",
		"type": "text",
		"text": "server-injected banner",
		"synthetic": true,
		"ignored": true
	}`
	var p Part
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !p.Synthetic {
		t.Error("Synthetic = false, want true")
	}
	if !p.Ignored {
		t.Error("Ignored = false, want true")
	}
}

func TestPart_Reasoning(t *testing.T) {
	raw := `{
		"id": "pt-reasoning",
		"type": "reasoning",
		"text": "thinking about the approach..."
	}`
	var p Part
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != PartReasoning {
		t.Errorf("Type = %q, want %q", p.Type, PartReasoning)
	}
	if p.Text != "thinking about the approach..." {
		t.Errorf("Text = %q", p.Text)
	}
}

func TestPart_ToolRunning(t *testing.T) {
	// Verifies a tool_use start: state.status=="running" with input JSON.
	raw := `{
		"id": "pt-tool1",
		"type": "tool",
		"callID": "call-abc",
		"tool": "bash",
		"state": {
			"status": "running",
			"input": {"command": "ls -la"},
			"title": "Running ls -la",
			"time": {"start": 1719523203000}
		}
	}`
	var p Part
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != PartTool {
		t.Errorf("Type = %q, want %q", p.Type, PartTool)
	}
	if p.CallID != "call-abc" {
		t.Errorf("CallID = %q", p.CallID)
	}
	if p.Tool != "bash" {
		t.Errorf("Tool = %q", p.Tool)
	}
	if p.State == nil {
		t.Fatal("State nil")
	}
	if p.State.Status != ToolStateRunning {
		t.Errorf("State.Status = %q, want %q", p.State.Status, ToolStateRunning)
	}
	if string(p.State.Input) == "" {
		t.Error("State.Input empty")
	}
	// Verify the input is parseable JSON.
	var input map[string]string
	if err := json.Unmarshal(p.State.Input, &input); err != nil {
		t.Errorf("State.Input not valid JSON object: %v", err)
	}
	if input["command"] != "ls -la" {
		t.Errorf("input.command = %q", input["command"])
	}
}

func TestPart_ToolCompleted(t *testing.T) {
	// Verifies a tool completion: state.status=="completed" with output.
	raw := `{
		"id": "pt-tool1",
		"type": "tool",
		"callID": "call-abc",
		"tool": "bash",
		"state": {
			"status": "completed",
			"input": {"command": "ls -la"},
			"output": "total 0\ndrwxr-xr-x 2 user user 40 Jun 14 12:00 .",
			"title": "Ran ls -la",
			"time": {"start": 1719523203000, "end": 1719523203500}
		}
	}`
	var p Part
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.State.Status != ToolStateCompleted {
		t.Errorf("Status = %q, want %q", p.State.Status, ToolStateCompleted)
	}
	if p.State.Output == "" {
		t.Error("Output empty")
	}
}

func TestPart_ToolError(t *testing.T) {
	raw := `{
		"id": "pt-tool-err",
		"type": "tool",
		"callID": "call-def",
		"tool": "bash",
		"state": {
			"status": "error",
			"input": {"command": "rm -rf /"},
			"error": "permission denied",
			"time": {"start": 1719523203000, "end": 1719523203100}
		}
	}`
	var p Part
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.State.Status != ToolStateError {
		t.Errorf("Status = %q, want %q", p.State.Status, ToolStateError)
	}
	if p.State.Error != "permission denied" {
		t.Errorf("Error = %q", p.State.Error)
	}
}

func TestPart_Subtask(t *testing.T) {
	// Verifies subtask parts parse — these drive Step 7's blockquote-
	// styled subagent visibility.
	raw := `{
		"id": "pt-sub1",
		"type": "subtask",
		"prompt": "Find all usages of foo()",
		"description": "Searching for foo() usages",
		"agent": "explore"
	}`
	var p Part
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != PartSubtask {
		t.Errorf("Type = %q, want %q", p.Type, PartSubtask)
	}
	if p.Description != "Searching for foo() usages" {
		t.Errorf("Description = %q", p.Description)
	}
	if p.Agent != "explore" {
		t.Errorf("Agent = %q", p.Agent)
	}
}

func TestPart_Compaction(t *testing.T) {
	raw := `{
		"id": "pt-comp",
		"type": "compaction",
		"auto": true
	}`
	var p Part
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != PartCompaction {
		t.Errorf("Type = %q, want %q", p.Type, PartCompaction)
	}
	if !p.Auto {
		t.Error("Auto = false, want true")
	}
}

func TestPart_File(t *testing.T) {
	raw := `{
		"id": "pt-file",
		"type": "file",
		"mime": "image/png",
		"filename": "screenshot.png",
		"url": "data:image/png;base64,iVBORw0KGgo="
	}`
	var p Part
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != PartFile {
		t.Errorf("Type = %q, want %q", p.Type, PartFile)
	}
	if p.Mime != "image/png" {
		t.Errorf("Mime = %q", p.Mime)
	}
	if p.URL == "" {
		t.Error("URL empty")
	}
}

// ---------------------------------------------------------------------------
// Permission
// ---------------------------------------------------------------------------

func TestPermission_Bash(t *testing.T) {
	// Verifies a typical bash permission prompt parses with its metadata.
	raw := `{
		"id": "perm-1",
		"type": "bash",
		"pattern": "git status",
		"sessionID": "sess-abc",
		"messageID": "msg-a1",
		"callID": "call-abc",
		"title": "Run bash command",
		"metadata": {"command": "git status"},
		"time": {"created": 1719523204000}
	}`
	var p Permission
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ID != "perm-1" {
		t.Errorf("ID = %q", p.ID)
	}
	if p.Type != PermBash {
		t.Errorf("Type = %q, want %q", p.Type, PermBash)
	}
	if p.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q", p.SessionID)
	}
	if p.CallID != "call-abc" {
		t.Errorf("CallID = %q", p.CallID)
	}
	if p.Title != "Run bash command" {
		t.Errorf("Title = %q", p.Title)
	}
}

func TestPermission_Question(t *testing.T) {
	// Verifies a question-tool permission parses. The metadata carries the
	// question schema (header/text/options); Step 9.4 decodes per Type.
	raw := `{
		"id": "perm-q1",
		"type": "question",
		"sessionID": "sess-abc",
		"messageID": "msg-a1",
		"title": "Pick a flavour",
		"metadata": {
			"header": "Flavour",
			"text": "Which flavour?",
			"options": [{"label": "Vanilla"}, {"label": "Chocolate"}]
		},
		"time": {"created": 1719523205000}
	}`
	var p Permission
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Type != PermQuestion {
		t.Errorf("Type = %q, want %q", p.Type, PermQuestion)
	}
	// Metadata is raw JSON — caller decodes per Type.
	var meta struct {
		Header string `json:"header"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(p.Metadata, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.Header != "Flavour" {
		t.Errorf("metadata.Header = %q", meta.Header)
	}
	if meta.Text != "Which flavour?" {
		t.Errorf("metadata.Text = %q", meta.Text)
	}
}

func TestPermission_PatternArray(t *testing.T) {
	// Verifies Permission.Pattern handles both the string and []string
	// wire shapes (it's a string|Array<string> union in types.gen.ts).
	// We keep it as json.RawMessage so the caller can decode either form.
	raw := `{
		"id": "perm-arr",
		"type": "edit",
		"pattern": ["src/**", "tests/**"],
		"sessionID": "sess-abc",
		"messageID": "msg-a1",
		"title": "Edit files",
		"metadata": {},
		"time": {"created": 1719523206000}
	}`
	var p Permission
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var patterns []string
	if err := json.Unmarshal(p.Pattern, &patterns); err != nil {
		t.Errorf("Pattern not decodable as []string: %v", err)
	}
	if len(patterns) != 2 || patterns[0] != "src/**" {
		t.Errorf("patterns = %v, want [src/** tests/**]", patterns)
	}
}

// ---------------------------------------------------------------------------
// SessionStatus
// ---------------------------------------------------------------------------

func TestSessionStatus_Busy(t *testing.T) {
	raw := `{"type": "busy"}`
	var s SessionStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Type != StatusBusy {
		t.Errorf("Type = %q, want %q", s.Type, StatusBusy)
	}
}

func TestSessionStatus_Retry(t *testing.T) {
	raw := `{"type": "retry", "attempt": 2, "message": "rate limited", "next": 1719523210000}`
	var s SessionStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Type != StatusRetry {
		t.Errorf("Type = %q, want %q", s.Type, StatusRetry)
	}
	if s.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", s.Attempt)
	}
	if s.Message != "rate limited" {
		t.Errorf("Message = %q", s.Message)
	}
}

// ---------------------------------------------------------------------------
// Events — the SSE envelope and typed payloads
// ---------------------------------------------------------------------------

// (rawEvent.sessionID() extraction is covered by Step 4's subscriber
// tests — the helper itself lands in Step 4 alongside its production
// caller, so deadcode sees the reachability chain.)

func TestEventMessagePartUpdated_TypedDecode(t *testing.T) {
	// Verifies a message.part.updated event decodes into the typed
	// payload — the shape Step 7's OnMessagePartUpdated reads.
	raw := `{
		"type": "message.part.updated",
		"properties": {
			"part": {
				"id": "pt-1",
				"sessionID": "sess-x",
				"messageID": "msg-1",
				"type": "text",
				"text": "Hello "
			},
			"delta": "Hello "
		}
	}`
	var ev rawEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if ev.Type != EventMessagePartUpdated {
		t.Errorf("Type = %q, want %q", ev.Type, EventMessagePartUpdated)
	}
	var payload eventMessagePartUpdated
	if err := json.Unmarshal(ev.Properties, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Part.Type != PartText {
		t.Errorf("Part.Type = %q", payload.Part.Type)
	}
	if payload.Part.Text != "Hello " {
		t.Errorf("Part.Text = %q", payload.Part.Text)
	}
	if payload.Delta != "Hello " {
		t.Errorf("Delta = %q", payload.Delta)
	}
}

func TestEventMessagePartDelta_TypedDecode(t *testing.T) {
	// Verifies a message.part.delta event decodes into the typed
	// payload — the shape onMessagePartDelta reads. Note the payload
	// carries NO part type, only a partID; the type is resolved at
	// dispatch time from the preceding message.part.updated.
	raw := `{
		"type": "message.part.delta",
		"properties": {
			"sessionID": "sess-x",
			"messageID": "msg-1",
			"partID": "pr-1",
			"field": "text",
			"delta": "reasoning fragment"
		}
	}`
	var ev rawEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if ev.Type != EventMessagePartDelta {
		t.Errorf("Type = %q, want %q", ev.Type, EventMessagePartDelta)
	}
	var payload eventMessagePartDelta
	if err := json.Unmarshal(ev.Properties, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.PartID != "pr-1" {
		t.Errorf("PartID = %q, want pr-1", payload.PartID)
	}
	if payload.Field != "text" {
		t.Errorf("Field = %q, want text", payload.Field)
	}
	if payload.Delta != "reasoning fragment" {
		t.Errorf("Delta = %q", payload.Delta)
	}
}

func TestEventMessageUpdated_TypedDecode(t *testing.T) {
	raw := `{
		"type": "message.updated",
		"properties": {
			"info": {
				"id": "msg-1",
				"sessionID": "sess-x",
				"role": "assistant",
				"modelID": "claude-sonnet-4",
				"finish": "stop",
				"tokens": {"input": 10, "output": 5, "reasoning": 0, "cache": {"read": 0, "write": 0}},
				"time": {"created": 1719523201000, "completed": 1719523202000}
			}
		}
	}`
	var ev rawEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var payload eventMessageUpdated
	if err := json.Unmarshal(ev.Properties, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Info.ModelID != "claude-sonnet-4" {
		t.Errorf("ModelID = %q", payload.Info.ModelID)
	}
	if payload.Info.Tokens == nil || payload.Info.Tokens.Output != 5 {
		t.Errorf("Tokens.Output wrong: %+v", payload.Info.Tokens)
	}
}

func TestEventPermissionUpdated_TypedDecode(t *testing.T) {
	raw := `{
		"type": "permission.updated",
		"properties": {
			"permission": {
				"id": "perm-1",
				"type": "bash",
				"sessionID": "sess-x",
				"messageID": "msg-1",
				"title": "Run bash",
				"metadata": {},
				"time": {"created": 1719523204000}
			}
		}
	}`
	var ev rawEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var payload eventPermissionUpdated
	if err := json.Unmarshal(ev.Properties, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Permission.ID != "perm-1" {
		t.Errorf("Permission.ID = %q", payload.Permission.ID)
	}
	if payload.Permission.Type != PermBash {
		t.Errorf("Permission.Type = %q", payload.Permission.Type)
	}
}

func TestEventSessionIdle_TypedDecode(t *testing.T) {
	raw := `{"type": "session.idle", "properties": {"sessionID": "sess-x"}}`
	var ev rawEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var payload eventSessionIdle
	if err := json.Unmarshal(ev.Properties, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.SessionID != "sess-x" {
		t.Errorf("SessionID = %q", payload.SessionID)
	}
}

func TestEventSessionStatus_TypedDecode(t *testing.T) {
	raw := `{
		"type": "session.status",
		"properties": {
			"sessionID": "sess-x",
			"status": {"type": "busy"}
		}
	}`
	var ev rawEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var payload eventSessionStatus
	if err := json.Unmarshal(ev.Properties, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.SessionID != "sess-x" {
		t.Errorf("SessionID = %q", payload.SessionID)
	}
	if payload.Status.Type != StatusBusy {
		t.Errorf("Status.Type = %q", payload.Status.Type)
	}
}

func TestEventSessionError_TypedDecode(t *testing.T) {
	// Verifies session.error carries a typed MessageError — the auth-
	// failure detection path in Step 11 reads this.
	raw := `{
		"type": "session.error",
		"properties": {
			"sessionID": "sess-x",
			"error": {
				"name": "ProviderAuthError",
				"data": {"providerID": "anthropic", "message": "expired"}
			}
		}
	}`
	var ev rawEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var payload eventSessionError
	if err := json.Unmarshal(ev.Properties, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.SessionID != "sess-x" {
		t.Errorf("SessionID = %q", payload.SessionID)
	}
	if payload.Error == nil || payload.Error.Name != ErrProviderAuth {
		t.Errorf("Error.Name wrong: %+v", payload.Error)
	}
}

// ---------------------------------------------------------------------------
// MessageError typed payloads
// ---------------------------------------------------------------------------

func TestMessageError_ApiErrorDecode(t *testing.T) {
	// Verifies an ApiError payload decodes into ApiErrorData — used by
	// Step 7/11 when classifying non-auth failures.
	raw := `{
		"name": "APIError",
		"data": {
			"message": "rate limited",
			"statusCode": 429,
			"isRetryable": true
		}
	}`
	var me MessageError
	if err := json.Unmarshal([]byte(raw), &me); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if me.Name != ErrAPI {
		t.Errorf("Name = %q, want %q", me.Name, ErrAPI)
	}
	var data ApiErrorData
	if err := json.Unmarshal(me.Data, &data); err != nil {
		t.Fatalf("decode ApiErrorData: %v", err)
	}
	if data.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", data.StatusCode)
	}
	if !data.IsRetryable {
		t.Error("IsRetryable = false, want true")
	}
}

func TestMessageError_MessageAbortedDecode(t *testing.T) {
	raw := `{
		"name": "MessageAbortedError",
		"data": {"message": "user aborted"}
	}`
	var me MessageError
	if err := json.Unmarshal([]byte(raw), &me); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if me.Name != ErrMessageAborted {
		t.Errorf("Name = %q, want %q", me.Name, ErrMessageAborted)
	}
	var data MessageAbortedErrorData
	if err := json.Unmarshal(me.Data, &data); err != nil {
		t.Fatalf("decode MessageAbortedErrorData: %v", err)
	}
	if data.Message != "user aborted" {
		t.Errorf("Message = %q", data.Message)
	}
}

// ---------------------------------------------------------------------------
// Unknown-event tolerance
// ---------------------------------------------------------------------------

func TestRawEvent_UnknownTypeDoesNotError(t *testing.T) {
	// Verifies a future/unknown event type still parses into the envelope
	// (the dispatcher just won't switch on it). Future-proofs Step 4's
	// subscriber against new event types opencode may add.
	raw := `{"type": "future.event", "properties": {"foo": "bar"}}`
	var ev rawEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Type != "future.event" {
		t.Errorf("Type = %q", ev.Type)
	}
	// Caller's switch falls through to default — no error here.
	if errors.Is(errUnknownEvent, nil) {
		t.Error("errUnknownEvent should not be nil-equal unless we define it")
	}
}

// errUnknownEvent is a placeholder for a future sentinel; defined here so
// the test above compiles. Step 4 will replace with the real one if
// needed.
var errUnknownEvent = errors.New("opencode: unknown event type")

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
)

// Tests that formatLine correctly identifies and formats session metadata lines.
func TestFormatLine_SessionMeta(t *testing.T) {
	meta := session.SessionMeta{
		Type:      "session_meta",
		CreatedAt: "2026-03-14T10:00:00Z",
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	if !strings.Contains(out, "session_meta") {
		t.Errorf("expected session_meta mention, got: %s", out)
	}
	if !strings.Contains(out, "2026-03-14T10:00:00Z") {
		t.Errorf("expected timestamp, got: %s", out)
	}
}

// Tests that formatLine correctly identifies and formats branch metadata lines.
func TestFormatLine_BranchMeta(t *testing.T) {
	line := `{"type":"branch_meta","parent_key":"scout/c123/1709590000","branch_point":5}`
	out := formatLine([]byte(line))
	if !strings.Contains(out, "branch_meta") {
		t.Errorf("expected branch_meta mention, got: %s", out)
	}
	if !strings.Contains(out, "scout/c123/1709590000") {
		t.Errorf("expected parent key, got: %s", out)
	}
}

// Tests that formatLine renders a user text message with the role header and text content.
func TestFormatLine_UserTextMessage(t *testing.T) {
	msg := provider.Message{
		Role:    "user",
		Content: provider.TextContent("Hello, world!"),
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	if !strings.Contains(out, "USER") {
		t.Errorf("expected USER header, got: %s", out)
	}
	if !strings.Contains(out, "Hello, world!") {
		t.Errorf("expected message text, got: %s", out)
	}
}

// Tests that formatLine renders assistant messages with tool_use blocks showing tool name and input.
func TestFormatLine_AssistantToolUse(t *testing.T) {
	msg := provider.Message{
		Role: "assistant",
		Content: []provider.ContentBlock{
			{Type: "text", Text: "Let me search for that."},
			{Type: "tool_use", ID: "toolu_01X", Name: "web_search", Input: json.RawMessage(`{"query":"weather"}`)},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	if !strings.Contains(out, "ASSISTANT") {
		t.Errorf("expected ASSISTANT header, got: %s", out)
	}
	if !strings.Contains(out, "web_search") {
		t.Errorf("expected tool name, got: %s", out)
	}
	if !strings.Contains(out, "weather") {
		t.Errorf("expected tool input, got: %s", out)
	}
}

// Tests that formatLine renders tool_result blocks with content and ID.
func TestFormatLine_ToolResult(t *testing.T) {
	msg := provider.Message{
		Role: "user",
		Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_01XYZ789AB", Content: "Sunny, 72°F"},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	if !strings.Contains(out, "toolu_01XYZ7") {
		t.Errorf("expected truncated tool ID, got: %s", out)
	}
	if !strings.Contains(out, "Sunny") {
		t.Errorf("expected result content, got: %s", out)
	}
}

// Tests that tool_result blocks with is_error=true show the error indicator.
func TestFormatLine_ToolResultError(t *testing.T) {
	msg := provider.Message{
		Role: "user",
		Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_err", Content: "command failed", IsError: true},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	if !strings.Contains(out, "✗") {
		t.Errorf("expected error indicator, got: %s", out)
	}
}

// Tests that image blocks show mime type and data size without dumping base64.
func TestFormatLine_ImageBlock(t *testing.T) {
	msg := provider.Message{
		Role: "user",
		Content: []provider.ContentBlock{
			provider.ImageBlock("image/png", "aGVsbG8="),
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	if !strings.Contains(out, "image/png") {
		t.Errorf("expected mime type, got: %s", out)
	}
	if !strings.Contains(out, "image") {
		t.Errorf("expected image label, got: %s", out)
	}
}

// Tests that thinking blocks are rendered dim/indented with the │ prefix.
func TestFormatLine_ThinkingBlock(t *testing.T) {
	msg := provider.Message{
		Role: "assistant",
		Content: []provider.ContentBlock{
			{Type: "thinking", Thinking: "Let me reason about this..."},
			{Type: "text", Text: "The answer is 42."},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	if !strings.Contains(out, "│") {
		t.Errorf("expected thinking prefix, got: %s", out)
	}
	if !strings.Contains(out, "reason about") {
		t.Errorf("expected thinking content, got: %s", out)
	}
}

// Tests that long thinking blocks are abbreviated with an omission notice.
func TestFormatLine_ThinkingTruncation(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "thinking line"
	}
	msg := provider.Message{
		Role: "assistant",
		Content: []provider.ContentBlock{
			{Type: "thinking", Thinking: strings.Join(lines, "\n")},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	if !strings.Contains(out, "omitted") {
		t.Errorf("expected omission notice for long thinking, got: %s", out)
	}
}

// Tests that tool_use blocks pretty-print their JSON input with indentation.
func TestFormatLine_ToolUsePrettyPrint(t *testing.T) {
	msg := provider.Message{
		Role: "assistant",
		Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_01X", Name: "send_message", Input: json.RawMessage(`{"text":"hello","channel":"general"}`)},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	if !strings.Contains(out, "send_message") {
		t.Errorf("expected tool name, got: %s", out)
	}
	// Should have indented JSON, not single-line
	if !strings.Contains(out, "\"text\": \"hello\"") {
		t.Errorf("expected pretty-printed JSON with key on own line, got: %s", out)
	}
	if !strings.Contains(out, "\"channel\": \"general\"") {
		t.Errorf("expected pretty-printed JSON with key on own line, got: %s", out)
	}
}

// Tests that pretty-printed JSON truncates long string values.
func TestFormatLine_ToolUseTruncatesLongStrings(t *testing.T) {
	longStr := strings.Repeat("x", 300)
	input := `{"text":"` + longStr + `"}`
	msg := provider.Message{
		Role: "assistant",
		Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "toolu_01X", Name: "send_message", Input: json.RawMessage(input)},
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := formatLine(data)
	// Should contain the truncation marker
	if !strings.Contains(out, "…") {
		t.Errorf("expected truncation marker for long string, got: %s", out)
	}
	// Should NOT contain the full 300-char string
	if strings.Contains(out, longStr) {
		t.Errorf("expected truncated string, but found full string in output")
	}
}

// Tests that empty lines return empty string.
func TestFormatLine_EmptyLine(t *testing.T) {
	out := formatLine([]byte(""))
	if out != "" {
		t.Errorf("expected empty output for empty line, got: %q", out)
	}
}

// Tests resolveSessionKey with a full 3-segment key (direct passthrough, no index needed).
func TestResolveSessionKey_FullKey(t *testing.T) {
	idx := testIndex(t)
	defer idx.Close()

	key, err := resolveSessionKey(idx, "scout/c123/1709590000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "scout/c123/1709590000" {
		t.Errorf("expected passthrough, got %q", key)
	}
}

// Tests resolveSessionKey with a 2-segment partial key that matches an indexed session.
func TestResolveSessionKey_PartialKey(t *testing.T) {
	idx := testIndex(t)
	defer idx.Close()

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  "scout/c123/1709590000",
		FilePath:    "/tmp/test.jsonl",
		CreatedAt:   time.Now(),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})

	key, err := resolveSessionKey(idx, "scout/c123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "scout/c123/1709590000" {
		t.Errorf("expected resolved key, got %q", key)
	}
}

// Tests resolveSessionKey with a bare agent name that matches via DefaultSessionKeyForAgent.
func TestResolveSessionKey_AgentName(t *testing.T) {
	idx := testIndex(t)
	defer idx.Close()

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "scout/c999/1709590000",
		FilePath:       "/tmp/test.jsonl",
		CreatedAt:      time.Now(),
		LastActivityAt: time.Now(),
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})

	key, err := resolveSessionKey(idx, "scout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "scout/c999/1709590000" {
		t.Errorf("expected resolved key, got %q", key)
	}
}

// Tests resolveSessionKey returns error for a bare agent name with no matching sessions.
func TestResolveSessionKey_AgentNotFound(t *testing.T) {
	idx := testIndex(t)
	defer idx.Close()

	_, err := resolveSessionKey(idx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("expected 'no active session' error, got: %v", err)
	}
}

// Tests DefaultSessionKeyForAgent returns the session key from chat_metadata.
func TestDefaultSessionKeyForAgent_ChatMetadata(t *testing.T) {
	idx := testIndex(t)
	defer idx.Close()

	if err := idx.SetChatMetadata("scout", "", 123, "session_key", "scout/c123/1709590000"); err != nil {
		t.Fatalf("set chat metadata: %v", err)
	}

	key := idx.DefaultSessionKeyForAgent("scout")
	if key != "scout/c123/1709590000" {
		t.Errorf("expected chat metadata key, got %q", key)
	}
}

// Tests DefaultSessionKeyForAgent falls back to session_index when no chat_metadata exists.
func TestDefaultSessionKeyForAgent_Fallback(t *testing.T) {
	idx := testIndex(t)
	defer idx.Close()

	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "scout/c999/1709590000",
		FilePath:       "/tmp/test.jsonl",
		CreatedAt:      time.Now(),
		LastActivityAt: time.Now(),
		SessionType:    session.SessionTypeChat,
		Status:         session.SessionStatusActive,
	})

	key := idx.DefaultSessionKeyForAgent("scout")
	if key != "scout/c999/1709590000" {
		t.Errorf("expected fallback key, got %q", key)
	}
}

// Tests DefaultSessionKeyForAgent excludes child sessions (4+ segment keys).
func TestDefaultSessionKeyForAgent_ExcludesChildren(t *testing.T) {
	idx := testIndex(t)
	defer idx.Close()

	// Insert only a branch session (4 segments)
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:     "scout/c999/1709590000/b1709590001",
		FilePath:       "/tmp/branch.jsonl",
		CreatedAt:      time.Now(),
		LastActivityAt: time.Now(),
		SessionType:    session.SessionTypeBranch,
		Status:         session.SessionStatusActive,
	})

	key := idx.DefaultSessionKeyForAgent("scout")
	if key != "" {
		t.Errorf("expected empty (no root sessions), got %q", key)
	}
}

// Tests printExistingContent reads a JSONL file and returns the correct offset.
func TestPrintExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	// Write some test lines
	meta := `{"type":"session_meta","created_at":"2026-03-14T10:00:00Z"}`
	msg1 := `{"role":"user","content":[{"type":"text","text":"hello"}]}`
	msg2 := `{"role":"assistant","content":[{"type":"text","text":"hi"}]}`
	content := meta + "\n" + msg1 + "\n" + msg2 + "\n"

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	offset, err := printExistingContent(path)
	if err != nil {
		t.Fatalf("printExistingContent: %v", err)
	}
	if offset != int64(len(content)) {
		t.Errorf("expected offset %d, got %d", len(content), offset)
	}
}

// Tests printNewContent reads only content added after the given offset.
func TestPrintNewContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	initial := `{"type":"session_meta","created_at":"2026-03-14T10:00:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
		t.Fatalf("write initial: %v", err)
	}
	initialOffset := int64(len(initial))

	// Append new content
	added := `{"role":"user","content":[{"type":"text","text":"new message"}]}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	f.WriteString(added)
	f.Close()

	newOffset, err := printNewContent(path, initialOffset)
	if err != nil {
		t.Fatalf("printNewContent: %v", err)
	}
	if newOffset != int64(len(initial)+len(added)) {
		t.Errorf("expected offset %d, got %d", len(initial)+len(added), newOffset)
	}
}

func testIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	dir := t.TempDir()
	idx, err := session.NewSessionIndex(filepath.Join(dir, "test_index.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	return idx
}

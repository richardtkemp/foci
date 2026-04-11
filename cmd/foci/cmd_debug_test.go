package main

import (
	"encoding/json"
	"io"
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

	offset, err := printExistingContent(path, outputHuman)
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

	newOffset, err := printNewContent(path, initialOffset, outputHuman)
	if err != nil {
		t.Fatalf("printNewContent: %v", err)
	}
	if newOffset != int64(len(initial)+len(added)) {
		t.Errorf("expected offset %d, got %d", len(initial)+len(added), newOffset)
	}
}

// Tests parseTimeArg with RFC3339 timestamps and relative durations.
func TestParseTimeArg(t *testing.T) {
	// RFC3339 should parse exactly
	ts, err := parseTimeArg("2026-03-14T10:00:00Z")
	if err != nil {
		t.Fatalf("RFC3339: %v", err)
	}
	if ts.Format(time.RFC3339) != "2026-03-14T10:00:00Z" {
		t.Errorf("RFC3339 = %v, want 2026-03-14T10:00:00Z", ts)
	}

	// Relative duration should return a time in the past
	before := time.Now().UTC()
	ts, err = parseTimeArg("1h")
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	after := time.Now().UTC()
	expectedApprox := before.Add(-time.Hour)
	if ts.Before(expectedApprox.Add(-time.Second)) || ts.After(after.Add(-time.Hour).Add(time.Second)) {
		t.Errorf("1h duration: got %v, expected ~%v", ts, expectedApprox)
	}

	// Invalid input
	_, err = parseTimeArg("not-a-time")
	if err == nil {
		t.Error("expected error for invalid input")
	}
}

// Tests inTimeRange with various boundary conditions.
func TestInTimeRange(t *testing.T) {
	base := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	before := base.Add(-time.Hour)
	after := base.Add(time.Hour)

	// Zero timestamp always excluded
	if inTimeRange(time.Time{}, before, after) {
		t.Error("zero time should be excluded")
	}

	// Within range
	if !inTimeRange(base, before, after) {
		t.Error("base should be in [before, after]")
	}

	// Before range
	if inTimeRange(before.Add(-time.Minute), before, after) {
		t.Error("should be out of range (too early)")
	}

	// After range
	if inTimeRange(after.Add(time.Minute), before, after) {
		t.Error("should be out of range (too late)")
	}

	// Open lower bound (zero from)
	if !inTimeRange(base, time.Time{}, after) {
		t.Error("open lower bound should include base")
	}

	// Open upper bound (zero to)
	if !inTimeRange(base, before, time.Time{}) {
		t.Error("open upper bound should include base")
	}
}

// Tests lineTimestamp extracts the timestamp from a JSONL message line.
func TestLineTimestamp(t *testing.T) {
	ts := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	msg := provider.Message{
		Role:      "user",
		Content:   provider.TextContent("hello"),
		Timestamp: &ts,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := lineTimestamp(data)
	if !got.Equal(ts) {
		t.Errorf("lineTimestamp = %v, want %v", got, ts)
	}

	// Line without timestamp
	noTS := `{"role":"user","content":[{"type":"text","text":"hi"}]}`
	got = lineTimestamp([]byte(noTS))
	if !got.IsZero() {
		t.Errorf("expected zero time for line without timestamp, got %v", got)
	}

	// Meta line (no timestamp)
	meta := `{"type":"session_meta","created_at":"2026-03-14T10:00:00Z"}`
	got = lineTimestamp([]byte(meta))
	if !got.IsZero() {
		t.Errorf("expected zero time for meta line, got %v", got)
	}
}

// Tests renderLine in JSON mode returns raw line with newline.
func TestRenderLine_JSON(t *testing.T) {
	line := `{"role":"user","content":[{"type":"text","text":"hello"}]}`
	out := renderLine([]byte(line), outputJSON)
	if out != line+"\n" {
		t.Errorf("JSON render = %q, want %q", out, line+"\n")
	}
}

// Tests renderLine in human mode delegates to formatLine.
func TestRenderLine_Human(t *testing.T) {
	msg := provider.Message{
		Role:    "user",
		Content: provider.TextContent("hello"),
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := renderLine(data, outputHuman)
	if !strings.Contains(out, "USER") {
		t.Errorf("human render should contain USER header, got: %s", out)
	}
}

// Tests printFilteredContent filters messages by timestamp range.
func TestPrintFilteredContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	base := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	early := base.Add(-2 * time.Hour)
	late := base.Add(2 * time.Hour)

	msg1 := provider.Message{Role: "user", Content: provider.TextContent("early"), Timestamp: &early}
	msg2 := provider.Message{Role: "user", Content: provider.TextContent("middle"), Timestamp: &base}
	msg3 := provider.Message{Role: "assistant", Content: provider.TextContent("late"), Timestamp: &late}

	var content string
	for _, msg := range []provider.Message{msg1, msg2, msg3} {
		data, _ := json.Marshal(msg)
		content += string(data) + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Filter: from 1h before base to 1h after base — should include only msg2
	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)

	// Capture output by redirecting stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := printFilteredContent(path, from, to, outputJSON)
	w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("printFilteredContent: %v", err)
	}

	var buf strings.Builder
	io.Copy(&buf, r) //nolint:errcheck
	output := buf.String()

	if !strings.Contains(output, "middle") {
		t.Errorf("expected 'middle' in output, got: %s", output)
	}
	if strings.Contains(output, "early") {
		t.Errorf("'early' should be filtered out, got: %s", output)
	}
	if strings.Contains(output, "late") {
		t.Errorf("'late' should be filtered out, got: %s", output)
	}
}

// Tests printExistingContent with JSON format outputs raw JSONL.
func TestPrintExistingContent_JSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	line := `{"role":"user","content":[{"type":"text","text":"hello"}]}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	offset, err := printExistingContent(path, outputJSON)
	w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("printExistingContent: %v", err)
	}
	if offset != int64(len(line)+1) {
		t.Errorf("offset = %d, want %d", offset, len(line)+1)
	}

	var buf strings.Builder
	io.Copy(&buf, r) //nolint:errcheck
	output := buf.String()

	// JSON mode should pass through the raw line
	if !strings.Contains(output, line) {
		t.Errorf("expected raw JSON in output, got: %s", output)
	}
}

// Tests that formatMessage uses stored timestamp when available.
func TestFormatMessage_UsesStoredTimestamp(t *testing.T) {
	ts := time.Date(2026, 3, 14, 15, 30, 45, 0, time.UTC)
	msg := provider.Message{
		Role:      "user",
		Content:   provider.TextContent("hello"),
		Timestamp: &ts,
	}

	out := formatMessage(msg)
	if !strings.Contains(out, "15:30:45") {
		t.Errorf("expected stored timestamp 15:30:45 in output, got: %s", out)
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

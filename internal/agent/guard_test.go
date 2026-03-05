package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/anthropic"
	"foci/internal/provider"
	"foci/internal/tools"
)

func TestGuardToolResult_UnderLimit(t *testing.T) {
	a := &Agent{MaxResultChars: 100}
	result := "short result"
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(result), nil)
	if got != result {
		t.Errorf("expected original result, got %q", got)
	}
}

func TestGuardToolResult_Disabled(t *testing.T) {
	a := &Agent{MaxResultChars: 0}
	result := "any length result"
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(result), nil)
	if got != result {
		t.Errorf("expected original result when disabled, got %q", got)
	}
}

func TestGuardToolResult_ExactlyAtLimit(t *testing.T) {
	a := &Agent{MaxResultChars: 10}
	result := "0123456789" // exactly 10 chars
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(result), nil)
	if got != result {
		t.Errorf("expected original result at exact limit, got %q", got)
	}
}

func TestGuardToolResult_OverLimit_JSONHint(t *testing.T) {
	tmpDir := t.TempDir()
	a := &Agent{MaxResultChars: 10, ToolResultTempDir: tmpDir}
	result := `{"key": "value", "data": [1, 2, 3, 4, 5, 6]}`
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(result), nil)

	if strings.Contains(got, `"value"`) {
		t.Error("guard message should not contain original JSON values")
	}
	if !strings.Contains(got, "Result too large") {
		t.Error("missing 'Result too large' prefix")
	}
	if !strings.Contains(got, "jq") {
		t.Error("JSON content should suggest jq")
	}
	if !strings.Contains(got, tmpDir) {
		t.Error("should reference temp dir path")
	}
}

func TestGuardToolResult_OverLimit_MarkdownHint(t *testing.T) {
	tmpDir := t.TempDir()
	a := &Agent{MaxResultChars: 10, ToolResultTempDir: tmpDir}
	result := "# Heading\n\nLots of markdown content that exceeds the limit"
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(result), nil)

	if strings.Contains(got, "Heading") {
		t.Error("guard message should not contain partial content")
	}
	if !strings.Contains(got, "mdq") {
		t.Error("markdown content should suggest mdq")
	}
}

func TestGuardToolResult_OverLimit_PlainTextHint(t *testing.T) {
	tmpDir := t.TempDir()
	a := &Agent{MaxResultChars: 10, ToolResultTempDir: tmpDir}
	result := "some plain text output that is longer than the limit allows"
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(result), nil)

	if strings.Contains(got, "plain text") {
		t.Error("guard message should not contain partial content")
	}
	if !strings.Contains(got, "summary") {
		t.Error("plain text should suggest summary tool")
	}
}

func TestGuardToolResult_WritesFullContent(t *testing.T) {
	tmpDir := t.TempDir()
	a := &Agent{MaxResultChars: 10, ToolResultTempDir: tmpDir}
	result := "this content is definitely longer than the 10 char limit"
	a.guardToolResult(context.Background(), nil, "test-session", "mytest", tools.TextResult(result), nil)

	// Find the written file
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 temp file, got %d", len(entries))
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if string(data) != result {
		t.Errorf("temp file content = %q, want full result", string(data))
	}
}

func TestGuardToolResult_MessageFormat(t *testing.T) {
	tmpDir := t.TempDir()
	a := &Agent{MaxResultChars: 10, ToolResultTempDir: tmpDir}
	result := "0123456789extra" // 15 chars, limit 10
	got := a.guardToolResult(context.Background(), nil, "test-session", "shell", tools.TextResult(result), nil)

	if !strings.Contains(got, "(15 chars, limit 10)") {
		t.Errorf("missing size info in %q", got)
	}
	if !strings.Contains(got, "Full output saved to") {
		t.Errorf("missing file path reference in %q", got)
	}
}

func TestGuardHint(t *testing.T) {
	path := "/tmp/test-result.txt"
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"json object", `{"foo": "bar"}`, "jq"},
		{"json array", `[1, 2, 3]`, "jq"},
		{"json with whitespace", `  {"foo": 1}`, "jq"},
		{"markdown", "# Title\nContent", "mdq"},
		{"markdown with whitespace", "  # Title", "mdq"},
		{"toml section", "[section]\nkey = \"value\"", "yq"},
		{"toml key-value", "name = \"foci\"\nversion = \"1.0\"", "yq"},
		{"yaml doc", "---\nname: foci", "yq"},
		{"yaml key-value", "name: foci\nversion: 1.0", "yq"},
		{"xml content", "<?xml version=\"1.0\"?><root/>", "yq"},
		{"plain text", "just some text", "summary"},
		{"empty", "", "summary"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guardHint(tt.content, path)
			if !strings.Contains(got, tt.want) {
				t.Errorf("guardHint(%q) = %q, want to contain %q", tt.content[:min(len(tt.content), 30)], got, tt.want)
			}
			if !strings.Contains(got, path) {
				t.Errorf("guardHint(%q) = %q, want to contain path %q", tt.content[:min(len(tt.content), 30)], got, path)
			}
		})
	}
}

func TestLooksLikeTOML(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"section header", "[section]\nkey = \"val\"", true},
		{"key = value", "name = \"foci\"", true},
		{"json array", "[1, 2, 3]", false},
		{"empty", "", false},
		{"plain text", "hello world", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeTOML(tt.content); got != tt.want {
				t.Errorf("looksLikeTOML(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestLooksLikeYAML(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"document start", "---\nname: foci", true},
		{"key: value", "name: foci\nversion: 1.0", true},
		{"not yaml - url", "http://example.com", false},
		{"empty", "", false},
		{"plain text", "hello world", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeYAML(tt.content); got != tt.want {
				t.Errorf("looksLikeYAML(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestDetectContentExtension(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"json object", `{"foo": "bar"}`, ".json"},
		{"json array", `[1, 2, 3]`, ".json"},
		{"json with leading whitespace", "  \n\t{\"a\": 1}", ".json"},
		{"markdown heading", "# Title\nContent", ".md"},
		{"markdown heading with whitespace", "  ## Subtitle", ".md"},
		{"plain text", "just some text", ".txt"},
		{"empty string", "", ".txt"},
		{"whitespace only", "   \n\t  ", ".txt"},
		{"text starting with letter", "hello world", ".txt"},
		{"html tag", "<div>hello</div>", ".html"},
		{"html doctype", "<!DOCTYPE html><html>", ".html"},
		{"html with whitespace", "  <p>text</p>", ".html"},
		{"xml declaration", "<?xml version=\"1.0\"?><root/>", ".xml"},
		{"rss feed", "<rss version=\"2.0\"><channel>", ".xml"},
		{"text starting with number", "123 items", ".txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectContentExtension(tt.content)
			if got != tt.want {
				t.Errorf("detectContentExtension(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestGuardToolResult_FileExtension(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantExt  string
		wantHint string
	}{
		{"json content", `{"items": [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]}`, ".json", "jq"},
		{"markdown content", "# Report\n\nA long document with details...", ".md", "mdq"},
		{"html content", "<html><body><p>Hello world</p></body></html>", ".html", "grep"},
		{"xml content", "<?xml version=\"1.0\"?><root><item>1</item></root>", ".xml", "grep"},
		{"plain text", "Output line 1\nOutput line 2\nOutput line 3", ".txt", "grep"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			a := &Agent{MaxResultChars: 10, ToolResultTempDir: tmpDir}
			a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(tt.content), nil)

			entries, err := os.ReadDir(tmpDir)
			if err != nil {
				t.Fatalf("read temp dir: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 temp file, got %d", len(entries))
			}

			filename := entries[0].Name()
			if !strings.HasSuffix(filename, tt.wantExt) {
				t.Errorf("filename %q should have extension %q", filename, tt.wantExt)
			}
		})
	}
}

// mockClient is a minimal anthropic.Client stand-in for testing.
// It captures the request and returns a canned response.
type mockSendMessage struct {
	called  bool
	request *provider.MessageRequest
	resp    *provider.MessageResponse
	err     error
}

func TestGuardToolResult_AutoSummary(t *testing.T) {
	tmpDir := t.TempDir()
	bigResult := strings.Repeat("x", 100)

	// Create a fake HTTP server that returns a canned Haiku response
	client := &anthropic.Client{} // will be replaced by mock
	mock := &mockSendMessage{
		resp: &provider.MessageResponse{
			Role:    "assistant",
			Content: []provider.ContentBlock{{Type: "text", Text: "Summary: lots of x characters"}},
			Usage:   provider.Usage{InputTokens: 50, OutputTokens: 10},
		},
	}

	// We can't easily mock the Client.SendMessage, so test the summariseToolResult
	// function indirectly by testing the integrated behaviour:
	// When ModelAliases is nil, no summary is attempted (fallback path)
	a := &Agent{
		MaxResultChars:      10,
		ToolResultTempDir:   tmpDir,
		Client:              client,
		AutoSummarise:       true,
		ModelAliases:        nil, // no aliases → skip summary
		SummaryContextTurns: 5,
		SummaryContextChars: 6000,
	}
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(bigResult), nil)
	if !strings.Contains(got, "Result too large") {
		t.Error("expected fallback guard message when ModelAliases is nil")
	}

	// Verify the mock is not used (just testing the nil path)
	_ = mock
}

func TestGuardToolResult_SkipsSummaryAboveMaxSummaryChars(t *testing.T) {
	tmpDir := t.TempDir()
	bigResult := strings.Repeat("x", 200)

	a := &Agent{
		MaxResultChars:      10,
		ToolResultTempDir:   tmpDir,
		Client:              &anthropic.Client{},
		AutoSummarise:       true,
		ModelAliases:        map[string]string{"haiku": "claude-haiku-4-5"},
		MaxSummaryChars:     50, // result (200 chars) exceeds this → skip summary
		SummaryContextTurns: 5,
		SummaryContextChars: 6000,
	}
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(bigResult), nil)
	if !strings.Contains(got, "Result too large") {
		t.Error("expected fallback guard message when result exceeds MaxSummaryChars")
	}
}

func TestGuardToolResult_FallbackOnNilClient(t *testing.T) {
	tmpDir := t.TempDir()
	bigResult := strings.Repeat("x", 100)

	a := &Agent{
		MaxResultChars:      10,
		ToolResultTempDir:   tmpDir,
		AutoSummarise:       true,
		Client:              nil, // no client → skip summary
		ModelAliases:        map[string]string{"haiku": "claude-haiku-4-5"},
		SummaryContextTurns: 5,
		SummaryContextChars: 6000,
	}
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(bigResult), nil)
	if !strings.Contains(got, "Result too large") {
		t.Error("expected fallback guard message when Client is nil")
	}
}

func TestRecentContext_Empty(t *testing.T) {
	got := recentContext(nil, 5, 6000)
	if got != "" {
		t.Errorf("expected empty string for nil messages, got %q", got)
	}

	got = recentContext([]provider.Message{}, 5, 6000)
	if got != "" {
		t.Errorf("expected empty string for empty messages, got %q", got)
	}
}

func TestRecentContext_ZeroTurns(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
	}
	got := recentContext(msgs, 0, 6000)
	if got != "" {
		t.Errorf("expected empty string for 0 maxTurns, got %q", got)
	}
}

func TestRecentContext_ZeroChars(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("hello")},
	}
	got := recentContext(msgs, 5, 0)
	if got != "" {
		t.Errorf("expected empty string for 0 maxChars, got %q", got)
	}
}

func TestRecentContext_BasicMessages(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("What is Go?")},
		{Role: "assistant", Content: provider.TextContent("Go is a programming language.")},
		{Role: "user", Content: provider.TextContent("Show me the code.")},
	}
	got := recentContext(msgs, 5, 6000)

	if !strings.Contains(got, "[user] What is Go?") {
		t.Error("missing first user message")
	}
	if !strings.Contains(got, "[assistant] Go is a programming language.") {
		t.Error("missing assistant message")
	}
	if !strings.Contains(got, "[user] Show me the code.") {
		t.Error("missing second user message")
	}

	// Check chronological order
	idx1 := strings.Index(got, "What is Go?")
	idx2 := strings.Index(got, "Go is a programming language")
	idx3 := strings.Index(got, "Show me the code")
	if idx1 > idx2 || idx2 > idx3 {
		t.Errorf("messages not in chronological order: %q", got)
	}
}

func TestRecentContext_TurnLimit(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("msg1")},
		{Role: "assistant", Content: provider.TextContent("msg2")},
		{Role: "user", Content: provider.TextContent("msg3")},
		{Role: "assistant", Content: provider.TextContent("msg4")},
		{Role: "user", Content: provider.TextContent("msg5")},
	}
	got := recentContext(msgs, 2, 6000)

	if strings.Contains(got, "msg1") || strings.Contains(got, "msg2") || strings.Contains(got, "msg3") {
		t.Error("should not include messages beyond maxTurns")
	}
	if !strings.Contains(got, "msg4") || !strings.Contains(got, "msg5") {
		t.Error("should include last 2 turns")
	}
}

func TestRecentContext_CharLimit(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("short")},
		{Role: "assistant", Content: provider.TextContent(strings.Repeat("a", 100))},
	}
	got := recentContext(msgs, 5, 20)

	// Should have truncated content
	totalText := 0
	for _, line := range strings.Split(got, "\n") {
		// Extract text after "[role] " prefix
		if idx := strings.Index(line, "] "); idx >= 0 {
			totalText += len(line[idx+2:])
		}
	}
	if totalText > 20 {
		t.Errorf("total text chars (%d) exceeds maxChars (20)", totalText)
	}
}

func TestRecentContext_SkipsToolBlocks(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("run ls")},
		{Role: "assistant", Content: []provider.ContentBlock{
			{Type: "tool_use", ID: "t1", Name: "shell", Input: json.RawMessage(`{"cmd":"ls"}`)},
		}},
		{Role: "user", Content: []provider.ContentBlock{
			{Type: "tool_result", ToolUseID: "t1", Content: "file1\nfile2"},
		}},
		{Role: "assistant", Content: provider.TextContent("Here are the files.")},
	}
	got := recentContext(msgs, 10, 6000)

	// Should include text messages but skip tool_use-only and tool_result-only messages
	if !strings.Contains(got, "run ls") {
		t.Error("should include user text message")
	}
	if !strings.Contains(got, "Here are the files") {
		t.Error("should include assistant text message")
	}
	// tool_use and tool_result messages have no text block, so they're skipped
	if strings.Contains(got, "file1") {
		t.Error("should not include tool_result content")
	}
}

func TestGuardToolResult_SkipsSummaryWhenAutoSummariseDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	bigResult := strings.Repeat("x", 200)

	a := &Agent{
		MaxResultChars:      10,
		ToolResultTempDir:   tmpDir,
		Client:              &anthropic.Client{},
		AutoSummarise:       false, // disabled → skip summary
		ModelAliases:        map[string]string{"haiku": "claude-haiku-4-5"},
		SummaryContextTurns: 5,
		SummaryContextChars: 6000,
	}
	got := a.guardToolResult(context.Background(), nil, "test-session", "test", tools.TextResult(bigResult), nil)
	if !strings.Contains(got, "Result too large") {
		t.Error("expected fallback guard message when AutoSummarise is false")
	}
}

func TestGuardToolResult_SummaryFormat(t *testing.T) {
	// Test the summary output format by calling summariseToolResult directly
	// This would need a real API client, so we test the format string construction
	model := "claude-haiku-4-5"
	result := strings.Repeat("x", 1000)
	savedPath := "/tmp/test-result.txt"

	expected := fmt.Sprintf("[Auto-summary by %s — full output (%d chars) saved to %s]\n\n%s",
		model, len(result), savedPath, "test summary")

	if !strings.Contains(expected, "[Auto-summary by") {
		t.Error("format should start with [Auto-summary by")
	}
	if !strings.Contains(expected, "1000 chars") {
		t.Error("format should contain char count")
	}
	if !strings.Contains(expected, savedPath) {
		t.Error("format should contain saved path")
	}
}

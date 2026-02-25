package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardToolResult_UnderLimit(t *testing.T) {
	a := &Agent{MaxResultChars: 100}
	result := "short result"
	got := a.guardToolResult("test", result)
	if got != result {
		t.Errorf("expected original result, got %q", got)
	}
}

func TestGuardToolResult_Disabled(t *testing.T) {
	a := &Agent{MaxResultChars: 0}
	result := "any length result"
	got := a.guardToolResult("test", result)
	if got != result {
		t.Errorf("expected original result when disabled, got %q", got)
	}
}

func TestGuardToolResult_ExactlyAtLimit(t *testing.T) {
	a := &Agent{MaxResultChars: 10}
	result := "0123456789" // exactly 10 chars
	got := a.guardToolResult("test", result)
	if got != result {
		t.Errorf("expected original result at exact limit, got %q", got)
	}
}

func TestGuardToolResult_OverLimit_JSONHint(t *testing.T) {
	tmpDir := t.TempDir()
	a := &Agent{MaxResultChars: 10, ToolResultTempDir: tmpDir}
	result := `{"key": "value", "data": [1, 2, 3, 4, 5, 6]}`
	got := a.guardToolResult("test", result)

	if strings.Contains(got, "key") {
		t.Error("guard message should not contain partial content")
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
	got := a.guardToolResult("test", result)

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
	got := a.guardToolResult("test", result)

	if strings.Contains(got, "plain text") {
		t.Error("guard message should not contain partial content")
	}
	if !strings.Contains(got, "grep") {
		t.Error("plain text should suggest grep")
	}
	if !strings.Contains(got, "head") {
		t.Error("plain text should suggest head")
	}
}

func TestGuardToolResult_WritesFullContent(t *testing.T) {
	tmpDir := t.TempDir()
	a := &Agent{MaxResultChars: 10, ToolResultTempDir: tmpDir}
	result := "this content is definitely longer than the 10 char limit"
	a.guardToolResult("mytest", result)

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
	got := a.guardToolResult("exec", result)

	if !strings.Contains(got, "(15 chars, limit 10)") {
		t.Errorf("missing size info in %q", got)
	}
	if !strings.Contains(got, "Full output saved to") {
		t.Errorf("missing file path reference in %q", got)
	}
}

func TestGuardHint(t *testing.T) {
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
		{"plain text", "just some text", "grep"},
		{"empty", "", "grep"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guardHint(tt.content)
			if !strings.Contains(got, tt.want) {
				t.Errorf("guardHint(%q) = %q, want to contain %q", tt.content[:min(len(tt.content), 30)], got, tt.want)
			}
		})
	}
}

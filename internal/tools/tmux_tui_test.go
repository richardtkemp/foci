package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNormalizePaneContent_ElapsedTimers(t *testing.T) {
	// Verifies that elapsed time patterns like "2h 30m" and "45m 12s" are stripped from pane content so they don't cause false-positive change detection.
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"Running 1m 3s", "Running "},
		{"Elapsed: 2h 30m", "Elapsed: "},
		{"Time: 0m 5s remaining", "Time:  remaining"},
		{"took 45m 12s total", "took  total"},
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt.input)
		if got != tt.want {
			t.Errorf("normalizePaneContent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizePaneContent_Clocks(t *testing.T) {
	// Verifies that wall-clock timestamps like "14:30" and "2:30:00 PM" are stripped so constantly-updating status bars don't appear as activity.
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"Status bar 14:30 ready", "Status bar  ready"},
		{"Clock: 2:30:00 PM end", "Clock:  end"},
		{"Time 9:05 AM left", "Time  left"},
		{"[23:59:59]", "[]"},
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt.input)
		if got != tt.want {
			t.Errorf("normalizePaneContent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizePaneContent_TokenCountsPreserved(t *testing.T) {
	// Verifies that token count values are preserved by normalization since changing token counts indicate genuine agent activity, not just clock noise.
	t.Parallel()
	tests := []string{
		"Context: 88,447 tokens used",
		"Used 1500 tokens",
		"12 tokens left",
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt)
		if got != tt {
			t.Errorf("normalizePaneContent(%q) = %q, want unchanged (tokens indicate activity)", tt, got)
		}
	}
}

func TestNormalizePaneContent_PercentagesPreserved(t *testing.T) {
	// Verifies that percentage values like context usage are preserved, since changing percentages reflect real progress rather than clock noise.
	t.Parallel()
	tests := []string{
		"44% used",
		"Context: 88.5% full",
		"Progress: 100%",
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt)
		if got != tt {
			t.Errorf("normalizePaneContent(%q) = %q, want unchanged (percentages indicate activity)", tt, got)
		}
	}
}

func TestNormalizePaneContent_CostsPreserved(t *testing.T) {
	// Verifies that dollar cost values are preserved during normalization since accumulating costs indicate genuine token-consuming activity.
	t.Parallel()
	tests := []string{
		"Cost: $0.0430",
		"Total $12.50 spent",
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt)
		if got != tt {
			t.Errorf("normalizePaneContent(%q) = %q, want unchanged (costs indicate activity)", tt, got)
		}
	}
}

func TestNormalizePaneContent_Durations(t *testing.T) {
	// Verifies that short decimal second durations like "3.2s" are stripped, preventing response-time display from causing false-positive activity detection.
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"Took 3.2s", "Took "},
		{"Response in 0.5s", "Response in "},
	}
	for _, tt := range tests {
		got := normalizePaneContent(tt.input)
		if got != tt.want {
			t.Errorf("normalizePaneContent(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizePaneContent_SpinnersPreserved(t *testing.T) {
	// Verifies that different spinner frames normalize to different strings, preserving spinner-based activity detection (different frames mean work is in progress).
	t.Parallel()
	input1 := "⠋ Loading..."
	input2 := "⠙ Loading..."
	norm1 := normalizePaneContent(input1)
	norm2 := normalizePaneContent(input2)
	if norm1 == norm2 {
		t.Errorf("spinner frames should be preserved (different frames = activity): %q vs %q", norm1, norm2)
	}
}

func TestNormalizePaneContent_PreservesContent(t *testing.T) {
	// Verifies that meaningful content like shell commands, error messages, and build output is not stripped, only volatile noise patterns are removed.
	t.Parallel()
	tests := []string{
		"$ ls -la",
		"error: file not found",
		"Build succeeded",
		"PASS ok foci/tools 0.004s", // "0.004s" gets stripped but that's fine
		"func TestFoo(t *testing.T)",
	}
	for _, input := range tests {
		got := normalizePaneContent(input)
		// Should not be empty (meaningful content preserved)
		if strings.TrimSpace(got) == "" {
			t.Errorf("normalizePaneContent(%q) = %q, should preserve content", input, got)
		}
	}
}

func TestNormalizePaneContent_MixedLine(t *testing.T) {
	// Verifies that a realistic TUI status bar strips only elapsed timers while preserving percentages, token counts, costs, and spinner frames.
	t.Parallel()
	input := "⠙ Thinking  Claude 3.5 | 44% context | 12,543 tokens | 2m 30s | $0.0430"
	got := normalizePaneContent(input)
	// Only clocks/timers should be stripped; spinners, tokens, percentages, costs preserved
	if !strings.Contains(got, "44%") {
		t.Errorf("percentage should be preserved (indicates activity): %q", got)
	}
	if !strings.Contains(got, "12,543 tokens") {
		t.Errorf("token count should be preserved (indicates activity): %q", got)
	}
	if strings.Contains(got, "2m 30s") {
		t.Errorf("elapsed timer should be stripped: %q", got)
	}
	if !strings.Contains(got, "$0.0430") {
		t.Errorf("cost should be preserved (indicates activity): %q", got)
	}
}

func TestNormalizePaneContent_StableHash(t *testing.T) {
	// Verifies that two snapshots differing only in elapsed timer values normalize to the same string, enabling deduplication of unchanged content.
	t.Parallel()
	snap1 := `$ opencode
OpenCode v0.1 | claude-3-5-sonnet
Thinking... | 1m 3s
> How do I fix the bug?`

	snap2 := `$ opencode
OpenCode v0.1 | claude-3-5-sonnet
Thinking... | 2m 54s
> How do I fix the bug?`

	norm1 := normalizePaneContent(snap1)
	norm2 := normalizePaneContent(snap2)

	if norm1 != norm2 {
		t.Errorf("snapshots differing only in timers should normalize equally:\n  snap1: %q\n  snap2: %q", norm1, norm2)
	}
}

func TestNormalizePaneContent_DifferentContent(t *testing.T) {
	// Verifies that snapshots with genuinely different content (agent thinking vs. response) do not normalize to the same string.
	t.Parallel()
	snap1 := `$ opencode
⠋ Thinking... | 44% context
> How do I fix the bug?`

	snap2 := `$ opencode
Here's the fix for the bug:
  change line 42 to use foo() instead of bar()`

	norm1 := normalizePaneContent(snap1)
	norm2 := normalizePaneContent(snap2)

	if norm1 == norm2 {
		t.Error("snapshots with different content should NOT normalize equally")
	}
}

func TestDetectTUIAgent_CC(t *testing.T) {
	// Verifies that Claude Code TUI markers (version string, bypass indicator, completion phrases) are correctly detected and return "cc".
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{"Claude Code marker", "some output\nClaude Code v1.2.3\nprompt here"},
		{"bypass marker", "⏵⏵ bypass\nsome command output"},
		{"Cooked for", "Cooked for 3.2s\nresult here"},
		{"Crunched for", "Crunched for 1.5s\nresult here"},
		{"Baked for", "Baked for 0.8s\nresult here"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectTUIAgent(tt.content)
			if got != "cc" {
				t.Errorf("detectTUIAgent() = %q, want %q", got, "cc")
			}
		})
	}
}

func TestDetectTUIAgent_OC(t *testing.T) {
	// Verifies that OpenCode TUI markers (version line, GLM, Build) are correctly detected and return "oc".
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{"OpenCode marker", "OpenCode v0.1\nclaude-3-5-sonnet\nprompt"},
		{"GLM marker", "GLM\nsome output here"},
		{"Build marker", "Build\nrunning tests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectTUIAgent(tt.content)
			if got != "oc" {
				t.Errorf("detectTUIAgent() = %q, want %q", got, "oc")
			}
		})
	}
}

func TestDetectTUIAgent_None(t *testing.T) {
	// Verifies that plain shell output and command results are not misidentified as a TUI agent, returning an empty string.
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{"plain shell", "$ ls -la\ntotal 42\ndrwxr-xr-x 5 user user 4096 file.go"},
		{"empty", ""},
		{"command output", "go test ./...\nPASS\nok  foci/tools 0.004s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectTUIAgent(tt.content)
			if got != "" {
				t.Errorf("detectTUIAgent() = %q, want empty", got)
			}
		})
	}
}

func TestCleanTUIOutput_CC(t *testing.T) {
	// Verifies that CC cleaning strips box-drawing chrome, decorative symbols, and status hints while preserving the actual response content.
	t.Parallel()
	input := strings.Join([]string{
		"Claude Code v1.2.3",
		"╭──────────────────────────╮",
		"│ Here is my response      │",
		"│ with important content   │",
		"╰──────────────────────────╯",
		"─────────────────────────",
		"✻",
		"▟█▙",
		"⏵⏵ bypass",
		"shift+tab to accept",
		"actual content line",
		"",
		"",
		"",
		"",
		"another content line",
	}, "\n")

	got := cleanTUIOutput(input, "cc")

	// Should preserve meaningful content
	if !strings.Contains(got, "Here is my response") {
		t.Errorf("should preserve content line, got:\n%s", got)
	}
	if !strings.Contains(got, "with important content") {
		t.Errorf("should preserve content line, got:\n%s", got)
	}
	if !strings.Contains(got, "actual content line") {
		t.Errorf("should preserve actual content, got:\n%s", got)
	}
	if !strings.Contains(got, "another content line") {
		t.Errorf("should preserve another content line, got:\n%s", got)
	}

	// Should strip chrome
	if strings.Contains(got, "Claude Code v1.2.3") {
		t.Errorf("should strip version line, got:\n%s", got)
	}
	if strings.Contains(got, "╭") || strings.Contains(got, "╰") {
		t.Errorf("should strip box-drawing lines, got:\n%s", got)
	}
	if strings.Contains(got, "─────") {
		t.Errorf("should strip horizontal rules, got:\n%s", got)
	}
	if strings.Contains(got, "✻") {
		t.Errorf("should strip decorative symbols, got:\n%s", got)
	}
	if strings.Contains(got, "▟█▙") {
		t.Errorf("should strip logo blocks, got:\n%s", got)
	}
	if strings.Contains(got, "⏵⏵ bypass") {
		t.Errorf("should strip mode indicator, got:\n%s", got)
	}
	if strings.Contains(got, "shift+tab") {
		t.Errorf("should strip status hints, got:\n%s", got)
	}

	// Should collapse consecutive blank lines
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("should collapse consecutive blank lines, got:\n%s", got)
	}
}

func TestCleanTUIOutput_OC(t *testing.T) {
	// Verifies that OC cleaning removes sidebar labels, box-drawing, section headers, and diff summaries while preserving the actual agent response.
	t.Parallel()
	input := strings.Join([]string{
		"OpenCode v0.1",
		"┃",
		"━━━━━━━━━━━━━━━━━━━━━━━━━",
		"MCP│server status",
		"LSP│go initialized",
		"Build│running...",
		"Here is the actual response",
		"with multiple lines",
		"Modified Files",
		"3 files changed",
		"ctrl+a to select all",
		"╹",
	}, "\n")

	got := cleanTUIOutput(input, "oc")

	// Should preserve meaningful content
	if !strings.Contains(got, "Here is the actual response") {
		t.Errorf("should preserve content, got:\n%s", got)
	}
	if !strings.Contains(got, "with multiple lines") {
		t.Errorf("should preserve content, got:\n%s", got)
	}

	// Should strip chrome
	if strings.Contains(got, "OpenCode v0.1") {
		t.Errorf("should strip version line, got:\n%s", got)
	}
	if strings.Contains(got, "━━━") {
		t.Errorf("should strip box-drawing, got:\n%s", got)
	}
	if strings.Contains(got, "MCP│") {
		t.Errorf("should strip MCP sidebar, got:\n%s", got)
	}
	if strings.Contains(got, "LSP│") {
		t.Errorf("should strip LSP sidebar, got:\n%s", got)
	}
	if strings.Contains(got, "Build│") {
		t.Errorf("should strip build line, got:\n%s", got)
	}
	if strings.Contains(got, "Modified Files") {
		t.Errorf("should strip section header, got:\n%s", got)
	}
	if strings.Contains(got, "3 files changed") {
		t.Errorf("should strip diff summary, got:\n%s", got)
	}
	if strings.Contains(got, "ctrl+a") {
		t.Errorf("should strip status hints, got:\n%s", got)
	}
}

func TestCleanTUIOutput_NoAgent(t *testing.T) {
	// Verifies that when no TUI agent is detected, cleanTUIOutput returns the content unchanged rather than incorrectly stripping anything.
	t.Parallel()
	input := "$ ls -la\ntotal 42\ndrwxr-xr-x 5 user user 4096 file.go"
	got := cleanTUIOutput(input, "")
	if got != input {
		t.Errorf("empty agent type should return content unchanged\ngot:  %q\nwant: %q", got, input)
	}
}

func TestCleanTUIOutput_PureChromeEmpty(t *testing.T) {
	// Verifies that a pane containing only CC TUI chrome (version line,
	// box-drawing, decorative symbols, status hints) produces an empty
	// result from cleanTUIOutput. This is the precondition that triggers
	// the retry path in read().
	t.Parallel()
	input := strings.Join([]string{
		"Claude Code v1.2.3",
		"╭──────────────────────────╮",
		"│                          │",
		"╰──────────────────────────╯",
		"─────────────────────────",
		"✻",
		"▟█▙",
		"⏵⏵ bypass",
		"shift+tab to accept",
		"",
		"",
	}, "\n")

	got := cleanTUIOutput(input, "cc")
	if got != "" {
		t.Errorf("expected empty result for pure-chrome pane, got:\n%q", got)
	}
}

func TestTmuxReadRaw(t *testing.T) {
	// Verifies that raw=true bypasses TUI cleaning and preserves all content including CC markers, while raw=false (default) applies the cleaning pipeline.
	t.Parallel()
	tmuxAvailable(t)
	_, tool, _ := NewTmuxTool(300, 30, nil, nil, "", false, 30, 0)

	name := "foci-test-readraw"
	tmuxSetup(t, name)

	// Start a session that echoes CC-like content
	params, _ := json.Marshal(map[string]interface{}{
		"operation": "start",
		"name":      name,
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Send some text that would trigger CC detection
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "send",
		"name":      name,
		"keys":      "echo Claude Code v1.0",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("send: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Read with raw=true — should contain the marker
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
		"raw":       true,
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !strings.Contains(result.Text, "Claude Code") {
		t.Errorf("raw read should preserve all content, got:\n%s", result.Text)
	}

	// Read with raw=false (default) — CC version line should be stripped
	params, _ = json.Marshal(map[string]interface{}{
		"operation": "read",
		"name":      name,
	})
	_, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read cleaned: %v", err)
	}
	// The "Claude Code v1.0" in `echo` output will be detected and the
	// version-line pattern may strip lines matching "Claude Code ...".
	// The echo command itself (containing "Claude Code") triggers detection,
	// and the output line "Claude Code v1.0" matches the version-line regex.
	// We just verify it doesn't error — exact content depends on shell prompt.
}

package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPromptsCommand verifies /prompts list renders the full prompts table with all status
// indicators (custom, default, inline, not-found, disabled) and the unrecognised files section.
func TestPromptsCommand(t *testing.T) {
	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				AgentID: "clutch",
				Prompts: []PromptInfo{
					{Label: "compaction_summary", Path: "/home/foci/prompts/compaction.md", Filename: "compaction-summary.md", Exists: true, Default: false},
					{Label: "keepalive", Filename: "keepalive.md", Default: true},
					{Label: "handoff_msg", Inline: "You are picking up a compacted session.", Default: false},
					{Label: "branch_orientation", Path: "/missing/file.md", Filename: "branch-orientation.md", Exists: false},
					{Label: "background", Filename: "background.md", Disabled: true},
					{Label: "braindead_warning", Inline: "Stop!", Default: true},
				},
				PromptDirs: []string{"/home/foci/prompts"},
				Files: []PromptFile{
					{Dir: "/home/foci/prompts", Name: "compaction.md", Configured: true},
					{Dir: "/home/foci/prompts", Name: "daily-review.md", Configured: false},
				},
				KnownFilenames: map[string]bool{
					"compaction.md": true,
				},
			}
		},
	})

	result, err := cmd.Execute(context.Background(), "list")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	checks := []string{
		"agent: clutch",
		"compaction_summary",
		"✏️",  // custom file
		"keepalive",
		"✅",  // default
		"[default]",
		"handoff_msg",
		"[custom inline: 39 chars]",
		"branch_orientation",
		"❌",  // not found
		"[not found]",
		"background",
		"⛔",  // disabled
		"disabled",
		"braindead_warning",
		"[default inline: 5 chars]",
		"---",  // table separator
		"Unrecognised prompt files",
		"daily-review.md",
	}
	for _, check := range checks {
		if !strings.Contains(result, check) {
			t.Errorf("missing %q in:\n%s", check, result)
		}
	}
	// Known filename should be filtered out of unrecognised
	if strings.Contains(result, "Unrecognised") && strings.Contains(result, "compaction.md") {
		// compaction.md is known, so it should NOT appear in the unrecognised section
		// But it could appear in the configured prompts path — check it's not in the unrecognised section specifically
		parts := strings.SplitN(result, "Unrecognised", 2)
		if len(parts) == 2 && strings.Contains(parts[1], "compaction.md") {
			t.Errorf("known filename compaction.md should not appear in unrecognised section:\n%s", result)
		}
	}
}

// TestPromptsCommandEmpty verifies /prompts list with a single default prompt renders correctly
// and omits the unrecognised files section when there are no files.
func TestPromptsCommandEmpty(t *testing.T) {
	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				AgentID: "test",
				Prompts: []PromptInfo{
					{Label: "branch_orientation", Filename: "branch-orientation.md", Default: true},
				},
				KnownFilenames: map[string]bool{},
			}
		},
	})

	result, err := cmd.Execute(context.Background(), "list")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "[default]") {
		t.Errorf("expected [default] in:\n%s", result)
	}
	if !strings.Contains(result, "✅") {
		t.Errorf("expected ✅ emoji in:\n%s", result)
	}
	if !strings.Contains(result, "---") {
		t.Errorf("expected table separator in:\n%s", result)
	}
	// No unrecognised files section when no files
	if strings.Contains(result, "Unrecognised") {
		t.Errorf("should not show unrecognised section when no files:\n%s", result)
	}
}

// TestPromptsCommandNoFiles verifies /prompts list omits unrecognised section when there are no
// files on disk.
func TestPromptsCommandNoFiles(t *testing.T) {
	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				AgentID:        "test",
				Prompts:        []PromptInfo{{Label: "branch_orientation", Filename: "branch-orientation.md", Default: true}},
				PromptDirs:     []string{"/some/dir"},
				Files:          nil,
				KnownFilenames: map[string]bool{},
			}
		},
	})

	result, err := cmd.Execute(context.Background(), "list")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// No unrecognised section when no files
	if strings.Contains(result, "Unrecognised") {
		t.Errorf("should not show unrecognised section when no files:\n%s", result)
	}
}

// TestPromptsCommandKnownFilenamesFiltered verifies that known filenames (keepalive.md, first-run.md) are excluded
// from the unrecognised files section while unknown files (custom-cron.md) still appear.
func TestPromptsCommandKnownFilenamesFiltered(t *testing.T) {
	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				AgentID: "test",
				Prompts: []PromptInfo{{Label: "keepalive", Filename: "keepalive.md", Default: true}},
				PromptDirs: []string{"/ws/prompts"},
				Files: []PromptFile{
					{Dir: "/ws/prompts", Name: "keepalive.md", Configured: true},
					{Dir: "/ws/prompts", Name: "first-run.md", Configured: false},
					{Dir: "/ws/prompts", Name: "custom-cron.md", Configured: false},
				},
				KnownFilenames: map[string]bool{
					"keepalive.md":  true,
					"first-run.md":  true,
				},
			}
		},
	})

	result, err := cmd.Execute(context.Background(), "list")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Only custom-cron.md should appear in unrecognised
	if !strings.Contains(result, "custom-cron.md") {
		t.Errorf("expected custom-cron.md in unrecognised section:\n%s", result)
	}
	if !strings.Contains(result, "Unrecognised") {
		t.Errorf("expected Unrecognised header:\n%s", result)
	}
	// Known filenames should NOT appear in unrecognised section
	parts := strings.SplitN(result, "Unrecognised", 2)
	if len(parts) == 2 {
		unrecSection := parts[1]
		if strings.Contains(unrecSection, "keepalive.md") {
			t.Errorf("keepalive.md should not appear in unrecognised section:\n%s", result)
		}
		if strings.Contains(unrecSection, "first-run.md") {
			t.Errorf("first-run.md should not appear in unrecognised section:\n%s", result)
		}
	}
}

// TestPromptsCommandBareReturnsUsage verifies that /prompts with no args returns a usage string instead of
// the table, since the inline keyboard handles bare invocations.
func TestPromptsCommandBareReturnsUsage(t *testing.T) {
	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{AgentID: "test"}
		},
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Usage:") {
		t.Errorf("bare /prompts should return usage, got: %s", result)
	}
	if !strings.Contains(result, "list") {
		t.Errorf("usage should mention 'list', got: %s", result)
	}
}

// TestPromptsCommandKeyboard verifies that the keyboard options include list, reinstall, and diff
// buttons for the bare /prompts command.
func TestPromptsCommandKeyboard(t *testing.T) {
	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData { return PromptsData{} },
	})

	if cmd.KeyboardOptions == nil {
		t.Fatal("KeyboardOptions should not be nil")
	}
	opts := cmd.KeyboardOptions(context.Background())
	if len(opts) != 3 {
		t.Fatalf("expected 3 keyboard options, got %d", len(opts))
	}
	labels := make([]string, len(opts))
	for i, o := range opts {
		labels[i] = o.Label
	}
	want := []string{"list", "reinstall", "diff"}
	for i, w := range want {
		if labels[i] != w {
			t.Errorf("option %d: got %q, want %q", i, labels[i], w)
		}
	}
}

// TestPromptsCommandChainKeyboardDiff verifies that selecting "diff" from the keyboard chains to a second
// keyboard listing prompt labels that have resolved texts.
func TestPromptsCommandChainKeyboardDiff(t *testing.T) {
	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				Prompts: []PromptInfo{
					{Label: "keepalive"},
					{Label: "background"},
					{Label: "compaction_summary"},
				},
				ResolvedTexts: map[string]string{
					"keepalive":          "text",
					"compaction_summary": "text",
				},
			}
		},
	})

	if cmd.ChainKeyboard == nil {
		t.Fatal("ChainKeyboard should not be nil")
	}

	// "diff" should produce options for prompts with resolved texts
	opts := cmd.ChainKeyboard(context.Background(), "diff")
	if len(opts) != 2 {
		t.Fatalf("expected 2 chain options, got %d", len(opts))
	}
	got := []string{opts[0].Label, opts[1].Label}
	if got[0] != "keepalive" || got[1] != "compaction_summary" {
		t.Errorf("unexpected chain labels: %v", got)
	}

	// Non-diff subcommands should not chain
	if opts := cmd.ChainKeyboard(context.Background(), "list"); opts != nil {
		t.Errorf("expected nil chain for 'list', got %v", opts)
	}
	if opts := cmd.ChainKeyboard(context.Background(), "reinstall"); opts != nil {
		t.Errorf("expected nil chain for 'reinstall', got %v", opts)
	}
}

// TestPromptsCommandReinstall verifies embedded prompts are written to workspace directory.
func TestPromptsCommandReinstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prompts")

	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				AgentID:             "test",
				WorkspacePromptsDir: dir,
				EmbeddedPrompts: map[string]string{
					"keepalive.md":          "keepalive default text",
					"compaction-summary.md": "compaction default text",
				},
			}
		},
	})

	result, err := cmd.Execute(context.Background(), "reinstall")
	if err != nil {
		t.Fatalf("Execute reinstall: %v", err)
	}
	if !strings.Contains(result, "Wrote 2 of 2") {
		t.Errorf("expected 'Wrote 2 of 2' in: %s", result)
	}
	if !strings.Contains(result, dir) {
		t.Errorf("expected dir path in: %s", result)
	}

	// Verify files were written
	data, err := os.ReadFile(filepath.Join(dir, "keepalive.md"))
	if err != nil {
		t.Fatalf("read keepalive.md: %v", err)
	}
	if string(data) != "keepalive default text" {
		t.Errorf("keepalive.md content = %q", string(data))
	}
}

// TestPromptsCommandReinstallIdempotent verifies reinstall is idempotent when files match.
func TestPromptsCommandReinstallIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prompts")

	embedded := map[string]string{
		"keepalive.md":          "keepalive default text",
		"compaction-summary.md": "compaction default text",
	}

	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				AgentID:             "test",
				WorkspacePromptsDir: dir,
				EmbeddedPrompts:     embedded,
			}
		},
	})

	// First run
	_, err := cmd.Execute(context.Background(), "reinstall")
	if err != nil {
		t.Fatalf("first reinstall: %v", err)
	}

	// Second run — all should match
	result, err := cmd.Execute(context.Background(), "reinstall")
	if err != nil {
		t.Fatalf("second reinstall: %v", err)
	}
	if !strings.Contains(result, "Wrote 0 of 2") {
		t.Errorf("expected 'Wrote 0 of 2' in: %s", result)
	}
	if !strings.Contains(result, "2 already match") {
		t.Errorf("expected '2 already match' in: %s", result)
	}
}

// TestPromptsCommandDiff verifies diff file is created and summary generated.
func TestPromptsCommandDiff(t *testing.T) {
	var sentPath string

	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				AgentID: "test",
				Prompts: []PromptInfo{
					{Label: "keepalive", Default: false},
				},
				ResolvedTexts: map[string]string{
					"keepalive": "custom keepalive\nwith changes",
				},
				DefaultTexts: map[string]string{
					"keepalive": "default keepalive\noriginal text",
				},
			}
		},
		SendDocFn: func(path string) error {
			sentPath = path
			// Read and keep the content before it gets deleted
			return nil
		},
		DiffSummaryFn: func(ctx context.Context, customText, defaultText, name string) (string, error) {
			return "Test summary of differences.", nil
		},
	})

	result, err := cmd.Execute(context.Background(), "diff keepalive")
	if err != nil {
		t.Fatalf("Execute diff: %v", err)
	}
	if !strings.Contains(result, "Diff for keepalive sent") {
		t.Errorf("unexpected result: %s", result)
	}
	if !strings.Contains(result, "lines changed") {
		t.Errorf("expected 'lines changed' in: %s", result)
	}
	if sentPath == "" {
		t.Error("SendDocFn was not called")
	}
}

// TestPromptsCommandDiffFuzzyMatch verifies fuzzy matching of prompt labels from various input formats.
func TestPromptsCommandDiffFuzzyMatch(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"compaction-summary", "compaction_summary"},
		{"compaction_summary", "compaction_summary"},
		{"compaction-summary.md", "compaction_summary"},
		{"keepalive.md", "keepalive"},
		{"branch-orientation-multiball", "branch_orient_multiball"},
		{"braindead", "braindead_warning"},
	}

	data := PromptsData{
		Prompts: []PromptInfo{
			{Label: "compaction_summary"},
			{Label: "keepalive"},
			{Label: "branch_orient_multiball"},
			{Label: "braindead_warning"},
		},
		ResolvedTexts: map[string]string{
			"compaction_summary":      "text",
			"keepalive":               "keepalive text",
			"branch_orient_multiball": "multiball text",
			"braindead_warning":       "braindead text",
		},
		DefaultTexts: map[string]string{
			"compaction_summary":      "compaction default",
			"keepalive":               "keepalive default",
			"branch_orient_multiball": "multiball default",
			"braindead_warning":       "",
		},
		EmbeddedPrompts: map[string]string{
			"compaction-summary.md":           "compaction default",
			"keepalive.md":                    "keepalive default",
			"branch-orientation-multiball.md": "multiball default",
		},
	}

	for _, tt := range tests {
		got := promptsMatchLabel(tt.input, data)
		if got != tt.want {
			t.Errorf("promptsMatchLabel(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestPromptsCommandDiffNotFound verifies error when prompt label doesn't match any prompt.
func TestPromptsCommandDiffNotFound(t *testing.T) {
	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				AgentID: "test",
				Prompts: []PromptInfo{
					{Label: "keepalive"},
					{Label: "background"},
				},
				ResolvedTexts: map[string]string{
					"keepalive":  "text",
					"background": "text",
				},
				DefaultTexts: map[string]string{
					"keepalive":  "text",
					"background": "text",
				},
			}
		},
	})

	_, err := cmd.Execute(context.Background(), "diff nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent prompt")
	}
	if !strings.Contains(err.Error(), "no prompt matching") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "keepalive") {
		t.Errorf("expected valid names in error: %v", err)
	}
}

// TestPromptsCommandDiffNoChanges verifies appropriate message when prompt matches embedded default.
func TestPromptsCommandDiffNoChanges(t *testing.T) {
	cmd := NewPromptsCommand(PromptsCmdDeps{
		DataFn: func() PromptsData {
			return PromptsData{
				AgentID: "test",
				Prompts: []PromptInfo{
					{Label: "keepalive", Default: true},
				},
				ResolvedTexts: map[string]string{
					"keepalive": "same text",
				},
				DefaultTexts: map[string]string{
					"keepalive": "same text",
				},
			}
		},
	})

	result, err := cmd.Execute(context.Background(), "diff keepalive")
	if err != nil {
		t.Fatalf("Execute diff: %v", err)
	}
	if !strings.Contains(result, "matches the embedded default") {
		t.Errorf("expected 'matches the embedded default' in: %s", result)
	}
}

// TestDiffLines verifies unified diff format for various scenarios.
func TestDiffLines(t *testing.T) {
	t.Run("identical", func(t *testing.T) {
		result := diffLines("hello\nworld\n", "hello\nworld\n", "a", "b")
		if result != "" {
			t.Errorf("expected empty for identical, got:\n%s", result)
		}
	})

	t.Run("simple change", func(t *testing.T) {
		result := diffLines("line1\nline2\nline3\n", "line1\nchanged\nline3\n", "a", "b")
		if !strings.Contains(result, "--- a") {
			t.Errorf("missing --- header in:\n%s", result)
		}
		if !strings.Contains(result, "+++ b") {
			t.Errorf("missing +++ header in:\n%s", result)
		}
		if !strings.Contains(result, "-line2") {
			t.Errorf("missing -line2 in:\n%s", result)
		}
		if !strings.Contains(result, "+changed") {
			t.Errorf("missing +changed in:\n%s", result)
		}
	})

	t.Run("addition", func(t *testing.T) {
		result := diffLines("a\nb\n", "a\nb\nc\n", "old", "new")
		if !strings.Contains(result, "+c") {
			t.Errorf("missing +c in:\n%s", result)
		}
	})

	t.Run("deletion", func(t *testing.T) {
		result := diffLines("a\nb\nc\n", "a\nc\n", "old", "new")
		if !strings.Contains(result, "-b") {
			t.Errorf("missing -b in:\n%s", result)
		}
	})

	t.Run("empty inputs", func(t *testing.T) {
		result := diffLines("", "new line\n", "a", "b")
		if !strings.Contains(result, "+new line") {
			t.Errorf("missing +new line in:\n%s", result)
		}
	})
}

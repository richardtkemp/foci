package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// promptsCC returns a CommandContext with the given PromptsData injected via PromptsDataFn.
func promptsCC(data PromptsData) CommandContext {
	return CommandContext{
		PromptsDataFn: func(_ CommandContext) PromptsData { return data },
	}
}

// TestPromptsCommand verifies /prompts list renders the full prompts table with all status
// indicators (custom, default, inline, not-found, disabled) and the unrecognised files section.
func TestPromptsCommand(t *testing.T) {
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
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
	})

	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	checks := []string{
		"agent: clutch",
		"compaction_summary",
		"✏️", // custom file
		"keepalive",
		"✅", // default
		"[default]",
		"handoff_msg",
		"[custom inline: 39 chars]",
		"branch_orientation",
		"❌", // not found
		"[not found]",
		"background",
		"⛔", // disabled
		"disabled",
		"braindead_warning",
		"[default inline: 5 chars]",
		"---", // table separator
		"Unrecognised prompt files",
		"daily-review.md",
	}
	for _, check := range checks {
		if !strings.Contains(result.Text, check) {
			t.Errorf("missing %q in:\n%s", check, result.Text)
		}
	}
	// Known filename should be filtered out of unrecognised
	if strings.Contains(result.Text, "Unrecognised") && strings.Contains(result.Text, "compaction.md") {
		parts := strings.SplitN(result.Text, "Unrecognised", 2)
		if len(parts) == 2 && strings.Contains(parts[1], "compaction.md") {
			t.Errorf("known filename compaction.md should not appear in unrecognised section:\n%s", result.Text)
		}
	}
}

// TestPromptsCommandEmpty verifies /prompts list with a single default prompt renders correctly
// and omits the unrecognised files section when there are no files.
func TestPromptsCommandEmpty(t *testing.T) {
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID: "test",
		Prompts: []PromptInfo{
			{Label: "branch_orientation", Filename: "branch-orientation.md", Default: true},
		},
		KnownFilenames: map[string]bool{},
	})

	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "[default]") {
		t.Errorf("expected [default] in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "✅") {
		t.Errorf("expected ✅ emoji in:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "---") {
		t.Errorf("expected table separator in:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "Unrecognised") {
		t.Errorf("should not show unrecognised section when no files:\n%s", result.Text)
	}
}

// TestPromptsCommandNoFiles verifies /prompts list omits unrecognised section when there are no
// files on disk.
func TestPromptsCommandNoFiles(t *testing.T) {
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID:        "test",
		Prompts:        []PromptInfo{{Label: "branch_orientation", Filename: "branch-orientation.md", Default: true}},
		PromptDirs:     []string{"/some/dir"},
		Files:          nil,
		KnownFilenames: map[string]bool{},
	})

	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(result.Text, "Unrecognised") {
		t.Errorf("should not show unrecognised section when no files:\n%s", result.Text)
	}
}

// TestPromptsCommandKnownFilenamesFiltered verifies that known filenames (keepalive.md, first-run.md) are excluded
// from the unrecognised files section while unknown files (custom-cron.md) still appear.
func TestPromptsCommandKnownFilenamesFiltered(t *testing.T) {
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID:    "test",
		Prompts:    []PromptInfo{{Label: "keepalive", Filename: "keepalive.md", Default: true}},
		PromptDirs: []string{"/ws/prompts"},
		Files: []PromptFile{
			{Dir: "/ws/prompts", Name: "keepalive.md", Configured: true},
			{Dir: "/ws/prompts", Name: "first-run.md", Configured: false},
			{Dir: "/ws/prompts", Name: "custom-cron.md", Configured: false},
		},
		KnownFilenames: map[string]bool{
			"keepalive.md": true,
			"first-run.md": true,
		},
	})

	result, err := cmd.Execute(context.Background(), Request{Args: "list"}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "custom-cron.md") {
		t.Errorf("expected custom-cron.md in unrecognised section:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "Unrecognised") {
		t.Errorf("expected Unrecognised header:\n%s", result.Text)
	}
	parts := strings.SplitN(result.Text, "Unrecognised", 2)
	if len(parts) == 2 {
		unrecSection := parts[1]
		if strings.Contains(unrecSection, "keepalive.md") {
			t.Errorf("keepalive.md should not appear in unrecognised section:\n%s", result.Text)
		}
		if strings.Contains(unrecSection, "first-run.md") {
			t.Errorf("first-run.md should not appear in unrecognised section:\n%s", result.Text)
		}
	}
}

// TestPromptsCommandBareReturnsUsage verifies that /prompts with no args returns a usage string instead of
// the table, since the inline keyboard handles bare invocations.
func TestPromptsCommandBareReturnsUsage(t *testing.T) {
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{AgentID: "test"})

	result, err := cmd.Execute(context.Background(), Request{}, cc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Usage:") {
		t.Errorf("bare /prompts should return usage, got: %s", result.Text)
	}
	if !strings.Contains(result.Text, "list") {
		t.Errorf("usage should mention 'list', got: %s", result.Text)
	}
}

// TestPromptsCommandKeyboard verifies that the keyboard options include list, reinstall, and diff
// buttons for the bare /prompts command.
func TestPromptsCommandKeyboard(t *testing.T) {
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{})

	if cmd.KeyboardOptions == nil {
		t.Fatal("KeyboardOptions should not be nil")
	}
	opts := cmd.KeyboardOptions(context.Background(), cc)
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
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		Prompts: []PromptInfo{
			{Label: "keepalive"},
			{Label: "background"},
			{Label: "compaction_summary"},
		},
		ResolvedTexts: map[string]string{
			"keepalive":          "text",
			"compaction_summary": "text",
		},
	})

	if cmd.ChainKeyboard == nil {
		t.Fatal("ChainKeyboard should not be nil")
	}

	// "diff" should produce options for prompts with resolved texts
	opts := cmd.ChainKeyboard(context.Background(), "diff", cc)
	if len(opts) != 2 {
		t.Fatalf("expected 2 chain options, got %d", len(opts))
	}
	got := []string{opts[0].Label, opts[1].Label}
	if got[0] != "keepalive" || got[1] != "compaction_summary" {
		t.Errorf("unexpected chain labels: %v", got)
	}

	// Non-diff subcommands should not chain
	if opts := cmd.ChainKeyboard(context.Background(), "list", cc); opts != nil {
		t.Errorf("expected nil chain for 'list', got %v", opts)
	}
	if opts := cmd.ChainKeyboard(context.Background(), "reinstall", cc); opts != nil {
		t.Errorf("expected nil chain for 'reinstall', got %v", opts)
	}
}

// TestPromptsCommandReinstallNoModified verifies that when no prompts are modified on disk,
// reinstall auto-skips everything and reports "All prompts reviewed" with no keyboard.
func TestPromptsCommandReinstallNoModified(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prompts")

	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID:             "test",
		WorkspacePromptsDir: dir,
		EmbeddedPrompts: map[string]string{
			"keepalive.md":          "keepalive default text",
			"compaction-summary.md": "compaction default text",
		},
	})

	result, err := cmd.Execute(context.Background(), Request{Args: "reinstall"}, cc)
	if err != nil {
		t.Fatalf("Execute reinstall: %v", err)
	}
	if !strings.Contains(result.Text, "All prompts reviewed") {
		t.Errorf("expected 'All prompts reviewed' in: %s", result.Text)
	}
	// Both should show as default
	if !strings.Contains(result.Text, "✅ compaction-summary.md — default") {
		t.Errorf("expected compaction-summary.md default marker in: %s", result.Text)
	}
	if !strings.Contains(result.Text, "✅ keepalive.md — default") {
		t.Errorf("expected keepalive.md default marker in: %s", result.Text)
	}
	if len(result.Keyboard) != 0 {
		t.Errorf("expected no keyboard, got %d options", len(result.Keyboard))
	}
}

// TestPromptsCommandReinstallModifiedStops verifies that reinstall stops at the first modified
// prompt, shows the modification info, and returns keyboard buttons for agent/shared/skip.
func TestPromptsCommandReinstallModifiedStops(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prompts")
	// Write a modified version of keepalive.md to disk
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keepalive.md"), []byte("custom keepalive"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID:             "test",
		WorkspacePromptsDir: dir,
		EmbeddedPrompts: map[string]string{
			"compaction-summary.md": "compaction default text",
			"keepalive.md":          "keepalive default text",
		},
	})

	result, err := cmd.Execute(context.Background(), Request{Args: "reinstall"}, cc)
	if err != nil {
		t.Fatalf("Execute reinstall: %v", err)
	}

	// compaction-summary.md is alphabetically first and unmodified
	if !strings.Contains(result.Text, "✅ compaction-summary.md — default") {
		t.Errorf("expected compaction-summary.md default marker in: %s", result.Text)
	}
	// keepalive.md is modified
	if !strings.Contains(result.Text, "✏️ keepalive.md — modified") {
		t.Errorf("expected keepalive.md modified marker in: %s", result.Text)
	}
	// Should have 3 keyboard buttons
	if len(result.Keyboard) != 3 {
		t.Fatalf("expected 3 keyboard options, got %d", len(result.Keyboard))
	}
	labels := []string{result.Keyboard[0].Label, result.Keyboard[1].Label, result.Keyboard[2].Label}
	if labels[0] != "agent" || labels[1] != "shared" || labels[2] != "skip" {
		t.Errorf("unexpected keyboard labels: %v", labels)
	}
}

// TestPromptsCommandReinstallAgentAction verifies the "agent" action writes the prompt to the
// workspace prompts directory and advances to the next prompt.
func TestPromptsCommandReinstallAgentAction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write modified versions of both prompts
	if err := os.WriteFile(filepath.Join(dir, "background.md"), []byte("custom bg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keepalive.md"), []byte("custom ka"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID:             "test",
		WorkspacePromptsDir: dir,
		EmbeddedPrompts: map[string]string{
			"background.md": "bg default",
			"keepalive.md":  "ka default",
		},
	})

	// First call stops at background.md (index 0)
	result, err := cmd.Execute(context.Background(), Request{Args: "reinstall"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Keyboard) == 0 {
		t.Fatal("expected keyboard for modified prompt")
	}

	// Choose "agent" for background.md (index 0)
	result, err = cmd.Execute(context.Background(), Request{Args: "reinstall 0 agent"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Wrote background.md → agent dir") {
		t.Errorf("expected write confirmation in: %s", result.Text)
	}

	// Verify file was overwritten with default
	data, err := os.ReadFile(filepath.Join(dir, "background.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "bg default" {
		t.Errorf("background.md content = %q, want %q", data, "bg default")
	}

	// Should now be showing keepalive.md as next modified prompt
	if !strings.Contains(result.Text, "✏️ keepalive.md — modified") {
		t.Errorf("expected keepalive.md modified marker in: %s", result.Text)
	}
}

// TestPromptsCommandReinstallSharedAction verifies the "shared" action writes to the shared
// prompts directory.
func TestPromptsCommandReinstallSharedAction(t *testing.T) {
	wsDir := filepath.Join(t.TempDir(), "agent", "prompts")
	sharedDir := filepath.Join(t.TempDir(), "shared", "prompts")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a modified version in workspace dir
	if err := os.WriteFile(filepath.Join(wsDir, "keepalive.md"), []byte("custom"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID:             "test",
		WorkspacePromptsDir: wsDir,
		SharedPromptsDir:    sharedDir,
		EmbeddedPrompts: map[string]string{
			"keepalive.md": "ka default",
		},
	})

	// Choose "shared" for keepalive.md (index 0)
	result, err := cmd.Execute(context.Background(), Request{Args: "reinstall 0 shared"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Wrote keepalive.md → shared dir") {
		t.Errorf("expected shared write confirmation in: %s", result.Text)
	}

	// Verify file was written to shared dir
	data, err := os.ReadFile(filepath.Join(sharedDir, "keepalive.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ka default" {
		t.Errorf("keepalive.md content = %q, want %q", data, "ka default")
	}
}

// TestPromptsCommandReinstallSkipAction verifies the "skip" action skips the prompt without
// writing anything.
func TestPromptsCommandReinstallSkipAction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keepalive.md"), []byte("custom"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID:             "test",
		WorkspacePromptsDir: dir,
		EmbeddedPrompts: map[string]string{
			"keepalive.md": "ka default",
		},
	})

	// Skip keepalive.md (index 0)
	result, err := cmd.Execute(context.Background(), Request{Args: "reinstall 0 skip"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Skipped keepalive.md") {
		t.Errorf("expected skip confirmation in: %s", result.Text)
	}
	if !strings.Contains(result.Text, "All prompts reviewed") {
		t.Errorf("expected completion message in: %s", result.Text)
	}

	// Verify file was NOT overwritten
	data, err := os.ReadFile(filepath.Join(dir, "keepalive.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "custom" {
		t.Errorf("keepalive.md should still be custom, got %q", data)
	}
}

// TestPromptsCommandReinstallSequentialProgression verifies that the interactive reinstall flow
// progresses through all modified prompts in alphabetical order, handling each action correctly.
func TestPromptsCommandReinstallSequentialProgression(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write modified versions of a.md and c.md; b.md is default
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("custom a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.md"), []byte("custom c"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		AgentID:             "test",
		WorkspacePromptsDir: dir,
		EmbeddedPrompts: map[string]string{
			"a.md": "default a",
			"b.md": "default b",
			"c.md": "default c",
		},
	})

	// Step 1: Start — a.md is first modified prompt (b.md is not on disk so default)
	result, err := cmd.Execute(context.Background(), Request{Args: "reinstall"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "✏️ a.md — modified") {
		t.Errorf("step 1: expected a.md modified, got: %s", result.Text)
	}
	if len(result.Keyboard) != 3 {
		t.Fatalf("step 1: expected 3 keyboard options, got %d", len(result.Keyboard))
	}

	// Step 2: Skip a.md — b.md is unmodified, c.md is next modified
	result, err = cmd.Execute(context.Background(), Request{Args: "reinstall 0 skip"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Skipped a.md") {
		t.Errorf("step 2: expected skip confirmation, got: %s", result.Text)
	}
	if !strings.Contains(result.Text, "✅ b.md — default") {
		t.Errorf("step 2: expected b.md default marker, got: %s", result.Text)
	}
	if !strings.Contains(result.Text, "✏️ c.md — modified") {
		t.Errorf("step 2: expected c.md modified, got: %s", result.Text)
	}

	// Step 3: Write c.md to agent — done
	result, err = cmd.Execute(context.Background(), Request{Args: "reinstall 2 agent"}, cc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Wrote c.md → agent dir") {
		t.Errorf("step 3: expected write confirmation, got: %s", result.Text)
	}
	if !strings.Contains(result.Text, "All prompts reviewed") {
		t.Errorf("step 3: expected completion message, got: %s", result.Text)
	}
	if len(result.Keyboard) != 0 {
		t.Errorf("step 3: expected no keyboard, got %d options", len(result.Keyboard))
	}
}

// TestPromptsCommandDiff verifies diff computes correctly, returns result text,
// and populates DocPath with a real temp file so the platform layer can send it.
func TestPromptsCommandDiff(t *testing.T) {
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
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
	})
	// cc.Client is nil — no summary generated, but the diff + temp file still land.

	result, err := cmd.Execute(context.Background(), Request{Args: "diff keepalive"}, cc)
	if err != nil {
		t.Fatalf("Execute diff: %v", err)
	}
	if !strings.Contains(result.Text, "Diff for keepalive sent") {
		t.Errorf("unexpected result: %s", result.Text)
	}
	if !strings.Contains(result.Text, "lines changed") {
		t.Errorf("expected 'lines changed' in: %s", result.Text)
	}
	if result.DocPath == "" {
		t.Fatal("expected DocPath to be set so platform can send the diff")
	}
	t.Cleanup(func() { _ = os.Remove(result.DocPath) })
	body, err := os.ReadFile(result.DocPath)
	if err != nil {
		t.Fatalf("read DocPath: %v", err)
	}
	if !strings.Contains(string(body), "# Prompt diff: keepalive") {
		t.Errorf("temp file missing header, got:\n%s", body)
	}
	if !strings.Contains(string(body), "```diff") {
		t.Errorf("temp file missing diff fence, got:\n%s", body)
	}
}

// TestPromptsCommandChainKeyboardDiffDataIncludesSubcommand verifies that buttons
// produced for /prompts diff encode the "diff " prefix, so clicking one re-dispatches
// "/prompts diff <label>" rather than "/prompts <label>" (which would just match nothing
// and return usage).
func TestPromptsCommandChainKeyboardDiffDataIncludesSubcommand(t *testing.T) {
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
		Prompts: []PromptInfo{{Label: "reflection"}, {Label: "keepalive"}},
		ResolvedTexts: map[string]string{
			"reflection": "t",
			"keepalive":  "t",
		},
	})

	opts := cmd.ChainKeyboard(context.Background(), "diff", cc)
	if len(opts) != 2 {
		t.Fatalf("expected 2 opts, got %d", len(opts))
	}
	for _, o := range opts {
		if !strings.HasPrefix(o.Data, "diff ") {
			t.Errorf("button Data should start with 'diff ', got %q", o.Data)
		}
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
		{"branch-orientation-facet", "branch_orient_facet"},
		{"braindead", "braindead_warning"},
	}

	data := PromptsData{
		Prompts: []PromptInfo{
			{Label: "compaction_summary"},
			{Label: "keepalive"},
			{Label: "branch_orient_facet"},
			{Label: "braindead_warning"},
		},
		ResolvedTexts: map[string]string{
			"compaction_summary":  "text",
			"keepalive":           "keepalive text",
			"branch_orient_facet": "facet text",
			"braindead_warning":   "braindead text",
		},
		DefaultTexts: map[string]string{
			"compaction_summary":  "compaction default",
			"keepalive":           "keepalive default",
			"branch_orient_facet": "facet default",
			"braindead_warning":   "",
		},
		EmbeddedPrompts: map[string]string{
			"compaction-summary.md":       "compaction default",
			"keepalive.md":                "keepalive default",
			"branch-orientation-facet.md": "facet default",
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
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
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
	})

	_, err := cmd.Execute(context.Background(), Request{Args: "diff nonexistent"}, cc)
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
	cmd := PromptsCommand()
	cc := promptsCC(PromptsData{
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
	})

	result, err := cmd.Execute(context.Background(), Request{Args: "diff keepalive"}, cc)
	if err != nil {
		t.Fatalf("Execute diff: %v", err)
	}
	if !strings.Contains(result.Text, "matches the embedded default") {
		t.Errorf("expected 'matches the embedded default' in: %s", result.Text)
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

package cctmux

import (
	"strings"
	"testing"
)

// TestShellQuote_Simple verifies that plain strings are wrapped in single quotes.
func TestShellQuote_Simple(t *testing.T) {
	got := shellQuote("hello")
	want := "'hello'"
	if got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "hello", got, want)
	}
}

// TestShellQuote_Empty verifies that an empty string produces a pair of single quotes.
func TestShellQuote_Empty(t *testing.T) {
	got := shellQuote("")
	want := "''"
	if got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "", got, want)
	}
}

// TestShellQuote_EmbeddedSingleQuote verifies that single quotes inside the
// string are escaped using the standard shell idiom: end single-quoted segment,
// insert a double-quoted single quote, resume single-quoted segment.
func TestShellQuote_EmbeddedSingleQuote(t *testing.T) {
	got := shellQuote("it's")
	want := "'it'\"'\"'s'"
	if got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "it's", got, want)
	}
}

// TestShellQuote_MultipleSingleQuotes verifies that multiple embedded single
// quotes are each escaped independently.
func TestShellQuote_MultipleSingleQuotes(t *testing.T) {
	got := shellQuote("a'b'c")
	// Each ' becomes '"'"'
	want := "'a'\"'\"'b'\"'\"'c'"
	if got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "a'b'c", got, want)
	}
}

// TestShellQuote_SpecialChars verifies that spaces, double quotes, semicolons,
// and other shell-special characters are safely contained by single quoting
// (they don't need extra escaping inside single quotes).
func TestShellQuote_SpecialChars(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"hello world", "'hello world'"},
		{`say "hi"`, `'say "hi"'`},
		{"a;b&&c|d", "'a;b&&c|d'"},
		{"$HOME", "'$HOME'"},
		{"`whoami`", "'`whoami`'"},
		{"foo\nbar", "'foo\nbar'"},
	}
	for _, tc := range cases {
		got := shellQuote(tc.input)
		if got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestExtractPermissionPrompt_BasicPrompt verifies parsing of a standard CC
// permission prompt with description, question, and numbered choices.
func TestExtractPermissionPrompt_BasicPrompt(t *testing.T) {
	pane := `Some previous output
───────────────────
  Edit file
  src/main.go
  ╌╌╌╌╌╌╌╌╌
  + new line
  ╌╌╌╌╌╌╌╌╌

  Do you want to proceed with this action?

  ❯ 1. Yes
    2. Yes, allow all edits in src/ during this session (shift+tab)
    3. No

  Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got == nil {
		t.Fatal("expected a permission prompt, got nil")
	}
	if len(got.Choices) != 3 {
		t.Fatalf("got %d choices, want 3", len(got.Choices))
	}
	if got.Choices[0].Number != "1" || got.Choices[0].Label != "Yes" {
		t.Errorf("choice 0 = %+v, want {1, Yes}", got.Choices[0])
	}
	if got.Choices[2].Number != "3" || got.Choices[2].Label != "No" {
		t.Errorf("choice 2 = %+v, want {3, No}", got.Choices[2])
	}
}

// TestExtractPermissionPrompt_EmptyInput verifies that empty pane content
// returns nil (no prompt).
func TestExtractPermissionPrompt_EmptyInput(t *testing.T) {
	got := extractPermissionPrompt("")
	if got != nil {
		t.Errorf("expected nil for empty input, got %+v", got)
	}
}

// TestExtractPermissionPrompt_MissingEscToCancel verifies that a pane with
// "Do you want to" but without "Esc to cancel" is not a valid prompt.
func TestExtractPermissionPrompt_MissingEscToCancel(t *testing.T) {
	pane := `Do you want to proceed with this action?
  1. Yes
  3. No
`
	got := extractPermissionPrompt(pane)
	if got != nil {
		t.Errorf("expected nil without 'Esc to cancel', got %+v", got)
	}
}

// TestExtractPermissionPrompt_MissingDoYouWantTo verifies that "Esc to cancel"
// alone is not sufficient for a valid prompt.
func TestExtractPermissionPrompt_MissingDoYouWantTo(t *testing.T) {
	pane := `Some random text
  Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got != nil {
		t.Errorf("expected nil without 'Do you want to', got %+v", got)
	}
}

// TestExtractPermissionPrompt_NoChoices verifies that when both markers are
// present but no numbered choices exist, the function returns nil (avoids
// false positives from scrollback containing the marker strings).
func TestExtractPermissionPrompt_NoChoices(t *testing.T) {
	pane := `Some output that mentions Do you want to do something?
And then later Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got != nil {
		t.Errorf("expected nil without numbered choices, got %+v", got)
	}
}

// TestExtractPermissionPrompt_MultiplePromptsInScrollback verifies that when
// scrollback contains multiple prompts, the most recent one is extracted.
// This simulates the real scenario where CC shows several prompts in sequence.
func TestExtractPermissionPrompt_MultiplePromptsInScrollback(t *testing.T) {
	pane := `───────────────────
  Old description

  Do you want to proceed?

  ❯ 1. Yes
    3. No

  Esc to cancel

Some output between prompts

───────────────────
  New description

  Do you want to proceed?

  1. Yes
  2. Yes, allow all
  3. No

  Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got == nil {
		t.Fatal("expected a permission prompt, got nil")
	}
	// Should match the most recent prompt (3 choices, not 2).
	if len(got.Choices) != 3 {
		t.Fatalf("got %d choices, want 3 (from latest prompt)", len(got.Choices))
	}
	if got.Choices[1].Label != "Yes, allow all" {
		t.Errorf("choice 1 label = %q, want %q", got.Choices[1].Label, "Yes, allow all")
	}
}

// TestExtractPermissionPrompt_BashCommand verifies extraction of a Bash command
// permission prompt and that the description is correctly parsed.
func TestExtractPermissionPrompt_BashCommand(t *testing.T) {
	pane := `───────────────────
  Bash command

     cd /tmp && rm -rf test

  Do you want to run this command?

  ❯ 1. Yes
    2. Yes, allow all bash commands during this session (shift+tab)
    3. No

  Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got == nil {
		t.Fatal("expected a permission prompt, got nil")
	}
	if got.Description == "" {
		t.Error("expected non-empty description")
	}
	// Description should contain "Bash command" from above the question.
	if got.Summary == "" {
		t.Error("expected non-empty summary")
	}
}

// TestExtractPermissionPrompt_CursorPrefix verifies that the ❯ cursor prefix
// on a choice line is correctly stripped so the choice label is clean.
func TestExtractPermissionPrompt_CursorPrefix(t *testing.T) {
	pane := `───────────────────
  Edit file
  foo.go

  Do you want to edit?

  ❯ 1. Yes
    3. No

  Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got == nil {
		t.Fatal("expected a permission prompt, got nil")
	}
	if got.Choices[0].Label != "Yes" {
		t.Errorf("choice 0 label = %q, want %q (cursor prefix should be stripped)", got.Choices[0].Label, "Yes")
	}
}

// TestExtractPermissionPrompt_DescriptionBeforeHRule verifies that content
// before the horizontal rule is NOT included in the description.
func TestExtractPermissionPrompt_DescriptionBeforeHRule(t *testing.T) {
	pane := `This should not be in the description
───────────────────
  Edit file
  foo.go

  Do you want to edit?

  1. Yes
  3. No

  Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got == nil {
		t.Fatal("expected a permission prompt, got nil")
	}
	if strings.Contains(got.Description, "should not be in") {
		t.Errorf("description should not include text before hrule, got: %q", got.Description)
	}
}

// TestExtractPermissionPrompt_NoHRule verifies that when there is no horizontal
// rule, the description starts from the "Do you want to" line area.
func TestExtractPermissionPrompt_NoHRule(t *testing.T) {
	pane := `Some earlier output
  Edit file
  bar.go

  Do you want to edit?

  1. Yes
  3. No

  Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got == nil {
		t.Fatal("expected a permission prompt, got nil")
	}
	// Even without hrule, parsing should succeed.
	if len(got.Choices) != 2 {
		t.Errorf("got %d choices, want 2", len(got.Choices))
	}
}

// TestBuildPermissionSummary_EditFile verifies that Edit file descriptions
// produce a summary like "Edit file filename".
func TestBuildPermissionSummary_EditFile(t *testing.T) {
	desc := "Edit file\n memory/2026-03-27.md\n╌╌╌╌╌╌╌╌╌\n+ added line\n╌╌╌╌╌╌╌╌╌"
	got := buildPermissionSummary(desc)
	want := "Edit file memory/2026-03-27.md"
	if got != want {
		t.Errorf("buildPermissionSummary = %q, want %q", got, want)
	}
}

// TestBuildPermissionSummary_CreateFile verifies that Create file descriptions
// produce a summary like "Create file filename" with relative path prefixes stripped.
func TestBuildPermissionSummary_CreateFile(t *testing.T) {
	desc := "Create file\n ../../../tmp/test.txt\n╌╌╌╌╌╌╌╌╌\n content here\n╌╌╌╌╌╌╌╌╌"
	got := buildPermissionSummary(desc)
	want := "Create file tmp/test.txt"
	if got != want {
		t.Errorf("buildPermissionSummary = %q, want %q", got, want)
	}
}

// TestBuildPermissionSummary_ReadFile verifies that Read file descriptions
// produce a summary like "Read file filename".
func TestBuildPermissionSummary_ReadFile(t *testing.T) {
	desc := "Read file\n path/to/file"
	got := buildPermissionSummary(desc)
	want := "Read file path/to/file"
	if got != want {
		t.Errorf("buildPermissionSummary = %q, want %q", got, want)
	}
}

// TestBuildPermissionSummary_WriteFile verifies that Write operations are
// treated like file operations (summary includes filename).
func TestBuildPermissionSummary_WriteFile(t *testing.T) {
	desc := "Write file\n output.txt\n╌╌╌╌╌╌╌╌╌\nsome data\n╌╌╌╌╌╌╌╌╌"
	got := buildPermissionSummary(desc)
	want := "Write file output.txt"
	if got != want {
		t.Errorf("buildPermissionSummary = %q, want %q", got, want)
	}
}

// TestBuildPermissionSummary_BashCommand verifies that Bash descriptions use
// the last clean line as the summary (CC puts the human description there).
// Lines must be contiguous — the function stops at the first blank line after
// content starts, so there is no blank line between header and command lines.
func TestBuildPermissionSummary_BashCommand(t *testing.T) {
	desc := "Bash command\n   cd /tmp && go vet\n   Run go vet on backend"
	got := buildPermissionSummary(desc)
	want := "Run go vet on backend"
	if got != want {
		t.Errorf("buildPermissionSummary = %q, want %q", got, want)
	}
}

// TestBuildPermissionSummary_BashSingleLine verifies Bash with only one content
// line (no description below the command). The command line immediately follows
// the header with no blank line gap.
func TestBuildPermissionSummary_BashSingleLine(t *testing.T) {
	desc := "Bash command\n   ls -la"
	got := buildPermissionSummary(desc)
	want := "ls -la"
	if got != want {
		t.Errorf("buildPermissionSummary = %q, want %q", got, want)
	}
}

// TestBuildPermissionSummary_UnknownTool verifies that unrecognized tool headers
// fall through to the "header — target" format.
func TestBuildPermissionSummary_UnknownTool(t *testing.T) {
	desc := "Custom tool\n   some detail"
	got := buildPermissionSummary(desc)
	want := "Custom tool — some detail"
	if got != want {
		t.Errorf("buildPermissionSummary = %q, want %q", got, want)
	}
}

// TestBuildPermissionSummary_Empty verifies that an empty description returns
// an empty summary.
func TestBuildPermissionSummary_Empty(t *testing.T) {
	got := buildPermissionSummary("")
	if got != "" {
		t.Errorf("buildPermissionSummary(%q) = %q, want empty", "", got)
	}
}

// TestBuildPermissionSummary_OnlyDividers verifies that a description with only
// dividers and whitespace returns an empty summary.
func TestBuildPermissionSummary_OnlyDividers(t *testing.T) {
	desc := "╌╌╌╌╌╌╌╌╌\n\n╌╌╌╌╌╌╌╌╌"
	got := buildPermissionSummary(desc)
	if got != "" {
		t.Errorf("buildPermissionSummary = %q, want empty", got)
	}
}

// TestBuildPermissionSummary_HeaderOnly verifies that when only a header line
// is present (no second line), the header itself is the summary.
func TestBuildPermissionSummary_HeaderOnly(t *testing.T) {
	got := buildPermissionSummary("Edit file")
	want := "Edit file"
	if got != want {
		t.Errorf("buildPermissionSummary = %q, want %q", got, want)
	}
}

// TestBuildPermissionSummary_RelativePathStripping verifies that up to three
// levels of "../" prefix are stripped from filenames.
func TestBuildPermissionSummary_RelativePathStripping(t *testing.T) {
	cases := []struct {
		desc string
		want string
	}{
		{"Edit file\n ../foo.go", "Edit file foo.go"},
		{"Edit file\n ../../bar.go", "Edit file bar.go"},
		{"Edit file\n ../../../baz.go", "Edit file baz.go"},
		// Four levels: only three are stripped.
		{"Edit file\n ../../../../deep.go", "Edit file ../deep.go"},
	}
	for _, tc := range cases {
		got := buildPermissionSummary(tc.desc)
		if got != tc.want {
			t.Errorf("buildPermissionSummary(%q) = %q, want %q", tc.desc, got, tc.want)
		}
	}
}

// TestBuildPermissionSummary_DividerStopsBash verifies that for Bash,
// content after a divider is ignored (the diff body). Lines before the
// divider are collected contiguously with no blank line gap.
func TestBuildPermissionSummary_DividerStopsBash(t *testing.T) {
	desc := "Bash command\n   echo hello\n   Print hello\n╌╌╌╌╌╌╌╌╌\nsome diff content\n╌╌╌╌╌╌╌╌╌"
	got := buildPermissionSummary(desc)
	// Should use the last clean line before the divider.
	want := "Print hello"
	if got != want {
		t.Errorf("buildPermissionSummary = %q, want %q", got, want)
	}
}

// TestExtractPermissionPrompt_SummaryIntegration verifies that the Summary
// field on the returned prompt is correctly populated from the description.
func TestExtractPermissionPrompt_SummaryIntegration(t *testing.T) {
	pane := `───────────────────
  Edit file
  src/main.go
  ╌╌╌╌╌╌╌╌╌
  + new line
  ╌╌╌╌╌╌╌╌╌

  Do you want to proceed?

  1. Yes
  3. No

  Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got == nil {
		t.Fatal("expected a permission prompt, got nil")
	}
	if got.Summary != "Edit file src/main.go" {
		t.Errorf("Summary = %q, want %q", got.Summary, "Edit file src/main.go")
	}
}

// TestExtractPermissionPrompt_RawContainsFullBlock verifies that the Raw field
// contains the full block text from description through choices.
func TestExtractPermissionPrompt_RawContainsFullBlock(t *testing.T) {
	pane := `previous output
───────────────────
  Edit file
  foo.go

  Do you want to edit?

  1. Yes
  3. No

  Esc to cancel
trailing output
`
	got := extractPermissionPrompt(pane)
	if got == nil {
		t.Fatal("expected a permission prompt, got nil")
	}
	if got.Raw == "" {
		t.Error("Raw should not be empty")
	}
	// Raw should contain both the description and the question.
	if !strings.Contains(got.Raw, "Edit file") {
		t.Error("Raw should contain the description")
	}
	if !strings.Contains(got.Raw, "Do you want to edit?") {
		t.Error("Raw should contain the question")
	}
}

// TestExtractPermissionPrompt_ChoiceWithShiftTabHint verifies that the full
// choice label (including parenthetical hints) is preserved.
func TestExtractPermissionPrompt_ChoiceWithShiftTabHint(t *testing.T) {
	pane := `───────────────────
  Edit file
  test.go

  Do you want to edit?

  1. Yes
  2. Yes, allow all edits in test/ during this session (shift+tab)
  3. No

  Esc to cancel
`
	got := extractPermissionPrompt(pane)
	if got == nil {
		t.Fatal("expected a permission prompt, got nil")
	}
	if len(got.Choices) != 3 {
		t.Fatalf("got %d choices, want 3", len(got.Choices))
	}
	wantLabel := "Yes, allow all edits in test/ during this session (shift+tab)"
	if got.Choices[1].Label != wantLabel {
		t.Errorf("choice 1 label = %q, want %q", got.Choices[1].Label, wantLabel)
	}
}

// TestExtractPermissionPrompt_MarkersInToolOutput verifies that "Do you want to"
// and "Esc to cancel" appearing in tool output (without numbered choices) does
// not produce a false positive.
func TestExtractPermissionPrompt_MarkersInToolOutput(t *testing.T) {
	pane := `The commit message says: "Do you want to refactor?"
And later: press Esc to cancel the operation.
`
	got := extractPermissionPrompt(pane)
	if got != nil {
		t.Errorf("expected nil for markers in tool output, got %+v", got)
	}
}

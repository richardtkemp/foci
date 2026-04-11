package nudge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/agent/turnevent"
	"foci/internal/platform"
)

func TestParseExtractionResponse(t *testing.T) {
	// Verifies JSON array parsing from model output.
	t.Parallel()

	input := `[
		{
			"text": "Verify facts before answering",
			"source_file": "CRAFT.md",
			"source_text": "Always verify",
			"trigger": {"type": "pre_answer"},
			"priority": "high"
		},
		{
			"text": "Check tool output",
			"source_file": "SOUL.md",
			"source_text": "Read carefully",
			"trigger": {"type": "every_n_tools", "n": 5},
			"priority": "medium"
		}
	]`

	rules, err := ParseExtractionResponse(input)
	if err != nil {
		t.Fatalf("ParseExtractionResponse: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if rules[0].Text != "Verify facts before answering" {
		t.Errorf("rule 0 text: %q", rules[0].Text)
	}
	if rules[0].Trigger.Type != "pre_answer" {
		t.Errorf("rule 0 trigger: %q", rules[0].Trigger.Type)
	}
	if rules[1].Trigger.N != 5 {
		t.Errorf("rule 1 trigger N: %d", rules[1].Trigger.N)
	}
}

func TestParseExtractionResponseCodeFence(t *testing.T) {
	// Handles markdown-wrapped JSON.
	t.Parallel()

	input := "```json\n" + `[{"text": "test", "source_file": "X.md", "source_text": "x", "trigger": {"type": "every_n_tools", "n": 3}, "priority": "low"}]` + "\n```"

	rules, err := ParseExtractionResponse(input)
	if err != nil {
		t.Fatalf("ParseExtractionResponse: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Text != "test" {
		t.Errorf("rule text: %q", rules[0].Text)
	}
}

func TestParseExtractionResponseEmpty(t *testing.T) {
	// Handles empty array.
	t.Parallel()

	rules, err := ParseExtractionResponse("[]")
	if err != nil {
		t.Fatalf("ParseExtractionResponse: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules, got %d", len(rules))
	}
}

func TestParseExtractionResponsePreambleWithFences(t *testing.T) {
	// Handles preamble text before code-fenced JSON.
	t.Parallel()

	input := "Looking through the character files, here are the rules I found:\n\n```json\n" +
		`[{"text": "test", "source_file": "X.md", "source_text": "x", "trigger": {"type": "every_n_tools", "n": 3}, "priority": "low"}]` +
		"\n```"

	rules, err := ParseExtractionResponse(input)
	if err != nil {
		t.Fatalf("ParseExtractionResponse: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Text != "test" {
		t.Errorf("rule text: %q", rules[0].Text)
	}
}

func TestParseExtractionResponsePreambleRawJSON(t *testing.T) {
	// Handles preamble text before raw JSON (no code fences).
	t.Parallel()

	input := "Here are the rules:\n" +
		`[{"text": "test", "source_file": "X.md", "source_text": "x", "trigger": {"type": "every_n_tools", "n": 3}, "priority": "low"}]`

	rules, err := ParseExtractionResponse(input)
	if err != nil {
		t.Fatalf("ParseExtractionResponse: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Text != "test" {
		t.Errorf("rule text: %q", rules[0].Text)
	}
}

func TestParseExtractionResponseEmptyResponse(t *testing.T) {
	// Returns empty rules for empty or whitespace-only response.
	t.Parallel()

	for _, input := range []string{"", "  ", "\n\t\n"} {
		rules, err := ParseExtractionResponse(input)
		if err != nil {
			t.Fatalf("ParseExtractionResponse(%q): %v", input, err)
		}
		if len(rules) != 0 {
			t.Errorf("expected 0 rules for %q, got %d", input, len(rules))
		}
	}
}

func TestParseExtractionResponseTruncatedJSON(t *testing.T) {
	// Returns empty rules for truncated JSON (opening bracket, no closing).
	t.Parallel()

	input := `[{"text": "test", "source_file": "X.md", "source_text": "x", "trigger": {"type": "per`

	rules, err := ParseExtractionResponse(input)
	if err != nil {
		t.Fatalf("ParseExtractionResponse: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules for truncated JSON, got %d", len(rules))
	}
}

func TestNeedsExtraction(t *testing.T) {
	// Verifies hash comparison logic.
	t.Parallel()

	dir := t.TempDir()
	// Write a character file
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Be careful"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExtractor(dir, []string{"SOUL.md"}, 0640)

	// First time: no rules file → needs extraction
	hash1, needed := e.NeedsExtraction()
	if !needed {
		t.Error("expected NeedsExtraction=true on first run")
	}
	if hash1 == "" {
		t.Error("expected non-empty hash")
	}

	// Save rules with the current hash → should NOT need extraction
	rs := &RuleSet{ContentHash: hash1, Rules: nil}
	if err := SaveRules(RulesPath(dir), rs, 0640); err != nil {
		t.Fatal(err)
	}
	_, needed = e.NeedsExtraction()
	if needed {
		t.Error("expected NeedsExtraction=false when hash matches")
	}

	// Change file → should need extraction
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Be very careful"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash2, needed := e.NeedsExtraction()
	if !needed {
		t.Error("expected NeedsExtraction=true after file change")
	}
	if hash2 == hash1 {
		t.Error("hash should change after file modification")
	}
}

func TestNeedsExtractionNoFiles(t *testing.T) {
	// Returns false when no character files exist.
	t.Parallel()

	dir := t.TempDir()
	e := NewExtractor(dir, []string{"NONEXISTENT.md"}, 0640)

	_, needed := e.NeedsExtraction()
	if needed {
		t.Error("expected NeedsExtraction=false with no files")
	}
}

// mockHandler implements BranchHandler for testing.
type mockHandler struct {
	response string
	err      error
}

func (m *mockHandler) HandleMessage(ctx context.Context, _ string, _ []string, _ []platform.Attachment) error {
	if m.err != nil {
		return m.err
	}
	// Write the response into any turnevent.Sink on the context so Extract's
	// BufferSink picks it up.
	turnevent.Emit(ctx, turnevent.TurnComplete{FinalText: m.response})
	return nil
}

func TestExtractEndToEnd(t *testing.T) {
	// Verifies extraction writes rules to disk.
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Always verify"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExtractor(dir, []string{"SOUL.md"}, 0640)
	handler := &mockHandler{
		response: `[{"text": "Verify first", "source_file": "SOUL.md", "source_text": "Always verify", "trigger": {"type": "pre_answer"}, "priority": "high"}]`,
	}

	if err := e.Extract(context.Background(), handler, "test/session"); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Verify rules were saved
	rs, err := LoadRules(RulesPath(dir))
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if rs == nil {
		t.Fatal("expected non-nil RuleSet")
	}
	if len(rs.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rs.Rules))
	}
	if rs.Rules[0].Text != "Verify first" {
		t.Errorf("rule text: %q", rs.Rules[0].Text)
	}

	// Second extraction: hash matches → should skip
	if err := e.Extract(context.Background(), handler, "test/session"); err != nil {
		t.Fatalf("second Extract: %v", err)
	}
}

// mockRunner implements OneShotRunner for testing.
type mockRunner struct {
	response    string
	err         error
	gotPrompt   string
	gotSysPrompt string
}

func (m *mockRunner) RunOnce(_ context.Context, prompt string, systemPrompt string) (string, error) {
	m.gotPrompt = prompt
	m.gotSysPrompt = systemPrompt
	return m.response, m.err
}

func TestExtractViaRunOnce(t *testing.T) {
	// Verifies ExtractViaRunOnce uses OneShotRunner and saves rules.
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CRAFT.md"), []byte("Check before acting"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExtractor(dir, []string{"CRAFT.md"}, 0640)
	runner := &mockRunner{
		response: `[{"text": "Check first", "source_file": "CRAFT.md", "source_text": "Check before acting", "trigger": {"type": "pre_answer"}, "priority": "high"}]`,
	}

	if err := e.ExtractViaRunOnce(context.Background(), runner); err != nil {
		t.Fatalf("ExtractViaRunOnce: %v", err)
	}

	// Verify the prompt was the extraction prompt.
	if runner.gotPrompt != ExtractionPrompt {
		t.Errorf("expected ExtractionPrompt, got %q", runner.gotPrompt[:50])
	}
	// Verify empty system prompt for extraction.
	if runner.gotSysPrompt != "" {
		t.Errorf("expected empty system prompt, got %q", runner.gotSysPrompt)
	}

	// Verify rules were saved.
	rs, err := LoadRules(RulesPath(dir))
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if rs == nil || len(rs.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %v", rs)
	}
	if rs.Rules[0].Text != "Check first" {
		t.Errorf("rule text: %q", rs.Rules[0].Text)
	}

	// Second call: hash matches → should skip.
	runner2 := &mockRunner{response: "should not be called"}
	if err := e.ExtractViaRunOnce(context.Background(), runner2); err != nil {
		t.Fatalf("second ExtractViaRunOnce: %v", err)
	}
	if runner2.gotPrompt != "" {
		t.Error("expected RunOnce not to be called on unchanged files")
	}
}

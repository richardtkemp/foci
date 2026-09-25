package nudge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/turnevent"
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

	e := NewExtractor("test", dir, []string{"SOUL.md"}, 0640, true, true)

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
	e := NewExtractor("test", dir, []string{"NONEXISTENT.md"}, 0640, true, true)

	_, needed := e.NeedsExtraction()
	if needed {
		t.Error("expected NeedsExtraction=false with no files")
	}
}

func TestNeedsExtractionUnsupportedTriggers(t *testing.T) {
	// When character files are unchanged (hash matches) but the stored rules
	// contain trigger types this backend can't evaluate, re-extraction is
	// forced so the file self-heals after a backend switch.
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Be careful"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileOrder := []string{"SOUL.md"}

	// Extractor with full caps to compute the current hash.
	full := NewExtractor("test", dir, fileOrder, 0640, true, true)
	hash, _ := full.NeedsExtraction()

	// Store rules (matching the current hash) that include post-tool and
	// pre-answer triggers — legal under a claude-code backend.
	rs := &RuleSet{
		ContentHash: hash,
		Rules: []Rule{
			{Text: "regex", Trigger: Trigger{Type: "regex", Pattern: "(?i)x"}},
			{Text: "tool", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "Bash"}},
			{Text: "pre", Trigger: Trigger{Type: "pre_answer"}},
		},
	}
	if err := SaveRules(RulesPath(dir), rs, 0640); err != nil {
		t.Fatal(err)
	}

	// Full-caps backend: all triggers supported → no re-extraction despite
	// the hash matching.
	if _, needed := full.NeedsExtraction(); needed {
		t.Error("full-caps backend should not need extraction when hash matches")
	}

	// Opencode-style backend (no post-tool, no pre-answer): the stored
	// tool_pattern/pre_answer rules are unsupported → force re-extraction
	// even though the hash is unchanged.
	limited := NewExtractor("test", dir, fileOrder, 0640, false, false)
	if _, needed := limited.NeedsExtraction(); !needed {
		t.Error("limited-caps backend should force re-extraction for unsupported triggers")
	}

	// A backend missing only pre-answer still self-heals a pre_answer rule.
	noPre := NewExtractor("test", dir, fileOrder, 0640, true, false)
	if _, needed := noPre.NeedsExtraction(); !needed {
		t.Error("post-tool-only backend should re-extract for a pre_answer rule")
	}
}

func TestNeedsExtractionAllSupportedNoReextract(t *testing.T) {
	// A limited backend whose stored rules use only supported triggers must
	// NOT loop-force extraction on every load.
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("Be careful"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileOrder := []string{"SOUL.md"}

	limited := NewExtractor("test", dir, fileOrder, 0640, false, false)
	hash, _ := limited.NeedsExtraction()

	rs := &RuleSet{
		ContentHash: hash,
		Rules: []Rule{
			{Text: "regex", Trigger: Trigger{Type: "regex", Pattern: "(?i)x"}},
			{Text: "turns", Trigger: Trigger{Type: "every_n_turns", N: 3}},
		},
	}
	if err := SaveRules(RulesPath(dir), rs, 0640); err != nil {
		t.Fatal(err)
	}

	if _, needed := limited.NeedsExtraction(); needed {
		t.Error("limited backend with only supported triggers should not re-extract")
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

	e := NewExtractor("test", dir, []string{"SOUL.md"}, 0640, true, true)
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

// mockRunner implements BatchRunner for testing.
type mockRunner struct {
	response string
	err      error
	gotReq   delegator.BatchRequest
	called   bool
}

func (m *mockRunner) RunBatch(_ context.Context, req delegator.BatchRequest) (string, error) {
	m.called = true
	m.gotReq = req
	return m.response, m.err
}

func TestExtractViaBatch(t *testing.T) {
	// Verifies ExtractViaBatch runs the extraction as a labelled batch and
	// saves rules.
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CRAFT.md"), []byte("Check before acting"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExtractor("test", dir, []string{"CRAFT.md"}, 0640, true, true)
	runner := &mockRunner{
		response: `[{"text": "Check first", "source_file": "CRAFT.md", "source_text": "Check before acting", "trigger": {"type": "pre_answer"}, "priority": "high"}]`,
	}

	if err := e.ExtractViaBatch(context.Background(), runner, "test/c1"); err != nil {
		t.Fatalf("ExtractViaBatch: %v", err)
	}

	// Verify the prompt was the extraction prompt.
	if runner.gotReq.Prompt != e.buildExtractionPrompt() {
		t.Errorf("expected extraction prompt, got %q", runner.gotReq.Prompt)
	}
	// Verify the character files ARE the system prompt — the CLI's default
	// system prompt must be replaced, or the model extracts rules from the
	// harness's own instructions instead of the character files (#1307).
	if !strings.Contains(runner.gotReq.SystemPrompt, "===== CRAFT.md =====") {
		t.Errorf("system prompt missing CRAFT.md header: %q", runner.gotReq.SystemPrompt)
	}
	if !strings.Contains(runner.gotReq.SystemPrompt, "Check before acting") {
		t.Errorf("system prompt missing character file content: %q", runner.gotReq.SystemPrompt)
	}
	// Labelled and attributed (#1962); no model override → backend default.
	if runner.gotReq.Purpose != delegator.BatchPurposeNudgeExtraction {
		t.Errorf("purpose = %q, want %q", runner.gotReq.Purpose, delegator.BatchPurposeNudgeExtraction)
	}
	if runner.gotReq.OwnerSessionKey != "test/c1" {
		t.Errorf("owner = %q, want test/c1", runner.gotReq.OwnerSessionKey)
	}
	if runner.gotReq.Model != "" {
		t.Errorf("model = %q, want empty (the backend's batch default)", runner.gotReq.Model)
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
	if err := e.ExtractViaBatch(context.Background(), runner2, "test/c1"); err != nil {
		t.Fatalf("second ExtractViaBatch: %v", err)
	}
	if runner2.called {
		t.Error("expected RunBatch not to be called on unchanged files")
	}
}

func TestExtractViaBatchWithModelOverride(t *testing.T) {
	// #1309: Extractor.Model is carried onto the batch request.
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CRAFT.md"), []byte("Check before acting"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := NewExtractor("test", dir, []string{"CRAFT.md"}, 0640, true, true)
	e.Model = "haiku"
	runner := &mockRunner{
		response: `[{"text": "Check first", "source_file": "CRAFT.md", "source_text": "Check before acting", "trigger": {"type": "pre_answer"}, "priority": "high"}]`,
	}

	if err := e.ExtractViaBatch(context.Background(), runner, "test/c1"); err != nil {
		t.Fatalf("ExtractViaBatch: %v", err)
	}
	if runner.gotReq.Model != "haiku" {
		t.Errorf("expected model=haiku on the batch request, got %q", runner.gotReq.Model)
	}
	if runner.gotReq.Prompt == "" {
		t.Error("expected the batch request to carry the extraction prompt")
	}
}

func TestCharacterSystemPrompt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CRAFT.md"), []byte("craft rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("memory rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	// SOUL.md is deliberately absent — missing files are skipped, and file
	// order is preserved for the ones that exist.
	e := NewExtractor("test", dir, []string{"CRAFT.md", "SOUL.md", "MEMORY.md"}, 0640, true, true)
	got := e.characterSystemPrompt()

	craftIdx := strings.Index(got, "===== CRAFT.md =====\n\ncraft rules")
	memIdx := strings.Index(got, "===== MEMORY.md =====\n\nmemory rules")
	if craftIdx < 0 || memIdx < 0 {
		t.Fatalf("missing file sections in system prompt: %q", got)
	}
	if craftIdx > memIdx {
		t.Errorf("file order not preserved: %q", got)
	}
	if strings.Contains(got, "SOUL.md") {
		t.Errorf("absent file should be skipped: %q", got)
	}
}

func TestExtractionPromptCapabilities(t *testing.T) {
	// Verifies the prompt only includes trigger types the backend supports.
	t.Parallel()

	full := NewExtractor("test", "", nil, 0640, true, true).buildExtractionPrompt()
	restricted := NewExtractor("test", "", nil, 0640, false, false).buildExtractionPrompt()

	// Full prompt includes all trigger types.
	for _, trigger := range []string{"every_n_tools", "pre_answer", "after_error", "regex", "tool_pattern"} {
		if !strings.Contains(full, trigger) {
			t.Errorf("full prompt missing trigger %q", trigger)
		}
	}

	// Restricted prompt (opencode backend) only includes regex.
	for _, trigger := range []string{"every_n_tools", "pre_answer", "after_error", "tool_pattern"} {
		if strings.Contains(restricted, trigger) {
			t.Errorf("restricted prompt should not include trigger %q", trigger)
		}
	}
	if !strings.Contains(restricted, "regex") {
		t.Errorf("restricted prompt must include regex trigger")
	}

	// The input_pattern false-positive guidance rides with tool_pattern:
	// present when post-tool triggers are offered, absent otherwise.
	if !strings.Contains(full, "false-positive shapes") {
		t.Errorf("full prompt missing input_pattern false-positive guidance")
	}
	if strings.Contains(restricted, "false-positive shapes") {
		t.Errorf("restricted prompt should not carry tool_pattern guidance")
	}
}

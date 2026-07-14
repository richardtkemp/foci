package nudge

import (
	"strings"
	"testing"
)

func TestDefaultRulesFiltering(t *testing.T) {
	// Verifies that only registered tools appear in the generated rule text,
	// and that unregistered tools are excluded.
	t.Parallel()

	rules := DefaultRules([]string{"shell", "read", "spawn"}, nil)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	r := rules[0]
	if r.Trigger.Type != "every_n_turns" {
		t.Errorf("expected trigger type every_n_turns, got %q", r.Trigger.Type)
	}
	if r.Category != CategoryDefault {
		t.Errorf("expected category %q, got %q", CategoryDefault, r.Category)
	}
	if !strings.Contains(r.Text, "shell (run commands)") {
		t.Error("expected shell in text")
	}
	if !strings.Contains(r.Text, "read (read file contents)") {
		t.Error("expected read in text")
	}
	if !strings.Contains(r.Text, "spawn (create sub-agents") {
		t.Error("expected spawn in text")
	}
	if strings.Contains(r.Text, "tmux") {
		t.Error("tmux should not appear — not registered")
	}
	if strings.Contains(r.Text, "browser") {
		t.Error("browser should not appear — not registered")
	}
}

func TestDefaultRulesStableOrder(t *testing.T) {
	// Verifies tools appear in the defined stable order, not alphabetical
	// or random map iteration order.
	t.Parallel()

	rules := DefaultRules([]string{"spawn", "shell", "read", "write"}, nil)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	text := rules[0].Text
	shellIdx := strings.Index(text, "shell")
	readIdx := strings.Index(text, "read")
	writeIdx := strings.Index(text, "write")
	spawnIdx := strings.Index(text, "spawn")

	if shellIdx > readIdx || readIdx > writeIdx || writeIdx > spawnIdx {
		t.Errorf("tools not in expected order: shell=%d read=%d write=%d spawn=%d", shellIdx, readIdx, writeIdx, spawnIdx)
	}
}

func TestDefaultRulesWithSkills(t *testing.T) {
	// Verifies that skills appear in the generated text with descriptions.
	t.Parallel()

	skills := []SkillSummary{
		{Name: "bouncer", Description: "security scanner"},
		{Name: "research", Description: "web research via Perplexity"},
	}
	rules := DefaultRules([]string{"shell"}, skills)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	text := rules[0].Text
	if !strings.Contains(text, "Skills available:") {
		t.Error("expected 'Skills available:' in text")
	}
	if !strings.Contains(text, "bouncer (security scanner)") {
		t.Error("expected bouncer skill with description")
	}
	if !strings.Contains(text, "research (web research via Perplexity)") {
		t.Error("expected research skill with description")
	}
}

func TestDefaultRulesSkillsOnly(t *testing.T) {
	// Verifies that skills-only (no tools) still generates a rule.
	t.Parallel()

	rules := DefaultRules(nil, []SkillSummary{{Name: "test", Description: "a test skill"}})
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if !strings.Contains(rules[0].Text, "Skills available:") {
		t.Error("expected skills section")
	}
	if strings.Contains(rules[0].Text, "Tool reminder") {
		t.Error("should not have tool reminder with no tools")
	}
}

func TestDefaultRulesEmpty(t *testing.T) {
	// Verifies that empty inputs produce no rules.
	t.Parallel()

	rules := DefaultRules(nil, nil)
	if len(rules) != 0 {
		t.Errorf("expected 0 rules, got %d", len(rules))
	}

	rules = DefaultRules([]string{}, []SkillSummary{})
	if len(rules) != 0 {
		t.Errorf("expected 0 rules for empty slices, got %d", len(rules))
	}
}

func TestDefaultRulesUnknownToolsIncluded(t *testing.T) {
	// Verifies that tools not in toolDescriptions (e.g. MCP tools)
	// are still included in the output, just without a description.
	t.Parallel()

	rules := DefaultRules([]string{"shell", "custom_mcp_tool"}, nil)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if !strings.Contains(rules[0].Text, "custom_mcp_tool") {
		t.Error("expected custom_mcp_tool in text")
	}
}

func TestBraindeadRuleBasic(t *testing.T) {
	// Verifies BraindeadRule builds a single every_n_tools "braindead" rule with
	// the default prompt. Threshold/prompt come from live settings at fire time.
	t.Parallel()

	rules := BraindeadRule()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	r := rules[0]
	if r.Trigger.Type != "every_n_tools" {
		t.Errorf("expected trigger type every_n_tools, got %q", r.Trigger.Type)
	}
	if r.Category != CategoryBraindead {
		t.Errorf("expected category %q, got %q", CategoryBraindead, r.Category)
	}
	if !strings.Contains(r.Text, "consecutive tool calls") {
		t.Error("expected default braindead prompt text")
	}
	if r.Priority != "high" {
		t.Errorf("expected priority high, got %q", r.Priority)
	}
	if r.SourceFile != "builtin" {
		t.Errorf("expected source_file builtin, got %q", r.SourceFile)
	}
}

func TestScratchpadRuleBasic(t *testing.T) {
	// Verifies ScratchpadRule builds an every_n_turns "scratchpad" rule with the
	// condition attached. Frequency comes from live settings at fire time.
	t.Parallel()

	called := false
	condition := func() bool { called = true; return true }
	rules := ScratchpadRule(condition)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	r := rules[0]
	if r.Trigger.Type != "every_n_turns" {
		t.Errorf("expected trigger type every_n_turns, got %q", r.Trigger.Type)
	}
	if r.Category != CategoryScratchpad {
		t.Errorf("expected category %q, got %q", CategoryScratchpad, r.Category)
	}
	if r.SourceFile != "builtin" {
		t.Errorf("expected source_file builtin, got %q", r.SourceFile)
	}
	if r.Condition == nil {
		t.Fatal("expected Condition to be set")
	}
	r.Condition()
	if !called {
		t.Error("expected condition function to be called")
	}
	if !strings.Contains(r.Text, "Scratchpad") {
		t.Error("expected rule text to mention scratchpad")
	}
}

func TestScratchpadRuleNilCondition(t *testing.T) {
	// A nil condition (no scratchpad store) is the only case that skips the rule.
	t.Parallel()

	if rules := ScratchpadRule(nil); rules != nil {
		t.Errorf("expected nil for nil condition, got %d rules", len(rules))
	}
}

func TestDefaultRulesSourceFile(t *testing.T) {
	// Verifies the source_file is set to "builtin".
	t.Parallel()

	rules := DefaultRules([]string{"shell"}, nil)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].SourceFile != "builtin" {
		t.Errorf("expected source_file 'builtin', got %q", rules[0].SourceFile)
	}
}

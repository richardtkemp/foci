package nudge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContentHash(t *testing.T) {
	// Verifies that identical inputs produce the same hash
	// and different inputs produce different hashes.
	t.Parallel()

	h1 := ContentHash([]string{"hello", "world"})
	h2 := ContentHash([]string{"hello", "world"})
	h3 := ContentHash([]string{"hello", "earth"})

	if h1 != h2 {
		t.Errorf("same inputs produced different hashes: %s vs %s", h1, h2)
	}
	if h1 == h3 {
		t.Error("different inputs produced same hash")
	}

	// Empty produces deterministic hash
	h4 := ContentHash(nil)
	h5 := ContentHash(nil)
	if h4 != h5 {
		t.Errorf("nil inputs produced different hashes: %s vs %s", h4, h5)
	}
}

func TestContentHashSeparator(t *testing.T) {
	// Ensures that concatenation boundaries are respected:
	// ["ab", "c"] ≠ ["a", "bc"].
	t.Parallel()

	h1 := ContentHash([]string{"ab", "c"})
	h2 := ContentHash([]string{"a", "bc"})
	if h1 == h2 {
		t.Error("different content boundaries produced same hash — separator not effective")
	}
}

func TestSaveAndLoadRules(t *testing.T) {
	// Round-trips a RuleSet through JSON serialization.
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nudge-rules.json")

	original := &RuleSet{
		ContentHash: "abc123",
		Rules: []Rule{
			{
				Text:       "Verify before answering",
				SourceFile: "CRAFT.md",
				SourceText: "Always verify your answers",
				Trigger:    Trigger{Type: "pre_answer"},
				Priority:   "high",
			},
			{
				Text:       "Check tool results",
				SourceFile: "SOUL.md",
				SourceText: "Read tool output carefully",
				Trigger:    Trigger{Type: "every_n_tools", N: 3},
				Priority:   "medium",
			},
		},
	}

	if err := SaveRules(path, original); err != nil {
		t.Fatalf("SaveRules: %v", err)
	}

	loaded, err := LoadRules(path)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}

	if loaded.ContentHash != original.ContentHash {
		t.Errorf("ContentHash mismatch: %s vs %s", loaded.ContentHash, original.ContentHash)
	}
	if len(loaded.Rules) != len(original.Rules) {
		t.Fatalf("Rules count mismatch: %d vs %d", len(loaded.Rules), len(original.Rules))
	}
	for i, r := range loaded.Rules {
		if r.Text != original.Rules[i].Text {
			t.Errorf("rule %d text: got %q, want %q", i, r.Text, original.Rules[i].Text)
		}
		if r.Trigger.Type != original.Rules[i].Trigger.Type {
			t.Errorf("rule %d trigger type: got %q, want %q", i, r.Trigger.Type, original.Rules[i].Trigger.Type)
		}
	}
}

func TestLoadRulesNotExist(t *testing.T) {
	// Returns nil when the file does not exist.
	t.Parallel()

	rs, err := LoadRules("/nonexistent/path/rules.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rs != nil {
		t.Error("expected nil for nonexistent file")
	}
}

func TestRulesPathCharacterDir(t *testing.T) {
	// Prefers the character/ subdirectory if it exists.
	t.Parallel()

	dir := t.TempDir()

	// Without character/ dir: rules file in workspace root
	path1 := RulesPath(dir)
	if filepath.Dir(path1) != dir {
		t.Errorf("expected rules in workspace root, got %s", path1)
	}

	// With character/ dir: rules file in character/
	charDir := filepath.Join(dir, "character")
	if err := os.Mkdir(charDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path2 := RulesPath(dir)
	if filepath.Dir(path2) != charDir {
		t.Errorf("expected rules in character dir, got %s", path2)
	}
}

func TestSaveRulesCreatesDir(t *testing.T) {
	// Ensures SaveRules creates parent directories.
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "nudge-rules.json")

	rs := &RuleSet{ContentHash: "test", Rules: nil}
	if err := SaveRules(path, rs); err != nil {
		t.Fatalf("SaveRules: %v", err)
	}

	loaded, err := LoadRules(path)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if loaded.ContentHash != "test" {
		t.Errorf("unexpected hash: %s", loaded.ContentHash)
	}
}

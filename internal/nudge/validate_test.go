package nudge

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"foci/internal/log"
)

// allCaps is a backend that runs every trigger type.
var allCaps = ValidateOpts{CanPostTool: true, CanPreAnswer: true}

// findingsWith returns the findings whose rendered line contains every one
// of want.
func findingsWith(fs []Finding, want ...string) []Finding {
	var out []Finding
	for _, f := range fs {
		ok := true
		for _, w := range want {
			if !strings.Contains(f.String(), w) {
				ok = false
			}
		}
		if ok {
			out = append(out, f)
		}
	}
	return out
}

// TestValidateFile_OneRuleOfEachTriggerTypeIsClean proves a file holding one
// valid rule of every trigger type yields no finding at all on a backend
// that runs them all: the validator rejects nothing the scheduler can fire.
func TestValidateFile_OneRuleOfEachTriggerTypeIsClean(t *testing.T) {
	data := []byte(`{
	"content_hash": "",
	"rules": [
		{"text": "a", "source_file": "SOUL.md", "source_text": "x", "priority": "high", "trigger": {"type": "every_n_tools", "n": 3}},
		{"text": "b", "priority": "medium", "trigger": {"type": "every_n_turns", "n": 2}},
		{"text": "c", "priority": "low", "trigger": {"type": "after_error"}},
		{"text": "d", "priority": "low", "trigger": {"type": "regex", "pattern": "(?i)deploy"}},
		{"text": "e", "priority": "low", "trigger": {"type": "pre_answer"}},
		{"text": "f", "priority": "low", "trigger": {"type": "tool_pattern", "tool_pattern": "^Bash$", "input_pattern": "git push", "consecutive": 2}}
	]
}`)
	if fs := ValidateFile(data, allCaps); len(fs) != 0 {
		t.Fatalf("want no findings, got %v", fs)
	}
}

// TestValidateFile_EachError covers every error of the strict list, one
// fixture per error: each must be reported as an error that names the rule
// by index and text (file-level ones name no rule) and says what is wrong.
func TestValidateFile_EachError(t *testing.T) {
	rule := func(body string) string { return `{"rules": [` + body + `]}` }
	cases := []struct {
		name string
		data string
		want []string // substrings of the one error line
	}{
		{"invalid JSON", `{"rules": [`, []string{"invalid JSON"}},
		{"unknown top-level key", `{"rulez": [], "rules": []}`, []string{`unknown key "rulez" at top level`}},
		{"unknown rule key", rule(`{"text": "verify first", "priority": "high", "trigger": {"type": "after_error"}, "prio": "x"}`), []string{`rule 0 ("verify first")`, `unknown key "prio"`}},
		{"unknown trigger key", rule(`{"text": "verify first", "priority": "high", "trigger": {"type": "every_n_turns", "n": 2, "every": 3}}`), []string{"rule 0", `unknown key "every" in trigger`}},
		{"unknown trigger type", rule(`{"text": "t", "priority": "high", "trigger": {"type": "every_turn"}}`), []string{"rule 0", `trigger type "every_turn" is not one of`}},
		{"n missing every_n_tools", rule(`{"text": "t", "priority": "high", "trigger": {"type": "every_n_tools"}}`), []string{"every_n_tools needs \"n\" greater than 0"}},
		{"n zero every_n_turns", rule(`{"text": "t", "priority": "high", "trigger": {"type": "every_n_turns", "n": 0}}`), []string{"every_n_turns needs \"n\" greater than 0"}},
		{"n negative", rule(`{"text": "t", "priority": "high", "trigger": {"type": "every_n_turns", "n": -1}}`), []string{"got -1"}},
		{"empty regex pattern", rule(`{"text": "t", "priority": "high", "trigger": {"type": "regex"}}`), []string{`empty "pattern"`}},
		{"bad regex pattern", rule(`{"text": "t", "priority": "high", "trigger": {"type": "regex", "pattern": "(unclosed"}}`), []string{`"pattern" does not compile`}},
		{"bad tool_pattern", rule(`{"text": "t", "priority": "high", "trigger": {"type": "tool_pattern", "tool_pattern": "[Bash"}}`), []string{`"tool_pattern" does not compile`}},
		{"bad input_pattern", rule(`{"text": "t", "priority": "high", "trigger": {"type": "tool_pattern", "input_pattern": "*x"}}`), []string{`"input_pattern" does not compile`}},
		{"empty text", rule(`{"text": "  ", "priority": "high", "trigger": {"type": "after_error"}}`), []string{`"text" is empty`}},
		{"bad priority", rule(`{"text": "t", "priority": "urgent", "trigger": {"type": "after_error"}}`), []string{`priority "urgent" is not one of high, medium, low`}},
		{"missing priority", rule(`{"text": "t", "trigger": {"type": "after_error"}}`), []string{`priority "" is not one of`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := ValidateFile([]byte(tc.data), allCaps)
			if len(fs) != 1 {
				t.Fatalf("want exactly 1 finding, got %d: %v", len(fs), fs)
			}
			if fs[0].Severity != SeverityError {
				t.Errorf("severity = %s, want error", fs[0].Severity)
			}
			for _, w := range tc.want {
				if !strings.Contains(fs[0].String(), w) {
					t.Errorf("finding %q lacks %q", fs[0].String(), w)
				}
			}
		})
	}
}

// TestValidateFile_RuleIndexAndTruncatedText proves a finding names the rule
// by its index in the file and by its text cut to a short label, so a long
// rule does not flood the line.
func TestValidateFile_RuleIndexAndTruncatedText(t *testing.T) {
	long := strings.Repeat("x", 200)
	data := []byte(`{"rules": [
		{"text": "fine", "priority": "high", "trigger": {"type": "after_error"}},
		{"text": "` + long + `", "priority": "high", "trigger": {"type": "nope"}}
	]}`)
	fs := ValidateFile(data, allCaps)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %v", fs)
	}
	line := fs[0].String()
	if !strings.HasPrefix(line, `error: rule 1 ("`+strings.Repeat("x", ruleTextLabelLen)+`...")`) {
		t.Errorf("line = %q, want it to name rule 1 by a truncated text", line)
	}
}

// TestValidateFile_CapabilityWarnings proves a trigger the agent's backend
// cannot run is a warning (the gateway skips the rule), not an error, for
// both kinds of mid-turn injection, and that a capable backend gets none.
func TestValidateFile_CapabilityWarnings(t *testing.T) {
	data := []byte(`{"rules": [
		{"text": "tools", "priority": "high", "trigger": {"type": "every_n_tools", "n": 3}},
		{"text": "answer", "priority": "high", "trigger": {"type": "pre_answer"}},
		{"text": "turns", "priority": "high", "trigger": {"type": "every_n_turns", "n": 3}}
	]}`)
	fs := ValidateFile(data, ValidateOpts{})
	if len(fs) != 2 {
		t.Fatalf("want 2 warnings, got %v", fs)
	}
	for _, f := range fs {
		if f.Severity != SeverityWarning {
			t.Errorf("%q: want a warning", f)
		}
	}
	if len(findingsWith(fs, "rule 0", "post-tool")) != 1 || len(findingsWith(fs, "rule 1", "pre-answer")) != 1 {
		t.Errorf("want a post-tool warning on rule 0 and a pre-answer one on rule 1, got %v", fs)
	}
	if fs := ValidateFile(data, allCaps); len(fs) != 0 {
		t.Errorf("a capable backend: want no findings, got %v", fs)
	}
}

// TestValidateFile_ContentHashWarning proves content_hash is judged exactly
// as the extractor judges it: a hash that NeedsExtraction would accept gives
// no warning, a stale one gives one warning (the gateway will overwrite the
// file), and a workspace with no character files gives none (nothing is
// ever extracted there).
func TestValidateFile_ContentHashWarning(t *testing.T) {
	ws := t.TempDir()
	order := []string{"SOUL.md", "MISSING.md", "CRAFT.md"}
	writeFile(t, filepath.Join(ws, "SOUL.md"), "soul")
	writeFile(t, filepath.Join(ws, "CRAFT.md"), "craft")

	ext := NewExtractor("a", ws, order, 0o644, true, true)
	current, needed := ext.NeedsExtraction()
	if !needed || current == "" {
		t.Fatalf("setup: NeedsExtraction = (%q, %v), want a hash and true (no rules file yet)", current, needed)
	}
	opts := ValidateOpts{CanPostTool: true, CanPreAnswer: true, WorkspaceDir: ws, FileOrder: order}
	body := func(hash string) []byte {
		return []byte(`{"content_hash": "` + hash + `", "rules": []}`)
	}

	if fs := ValidateFile(body(current), opts); len(fs) != 0 {
		t.Errorf("current hash: want no findings, got %v", fs)
	}
	// Cross-check with the extractor itself: with this hash on disk it does
	// not re-extract.
	writeFile(t, RulesPath(ws), string(body(current)))
	if _, needed := ext.NeedsExtraction(); needed {
		t.Fatal("extractor wants to re-extract a file the validator calls current")
	}

	fs := ValidateFile(body("stale"), opts)
	if len(fs) != 1 || fs[0].Severity != SeverityWarning || !strings.Contains(fs[0].Msg, "extract the rules again") {
		t.Errorf("stale hash: want one re-extraction warning, got %v", fs)
	}

	empty := ValidateOpts{CanPostTool: true, CanPreAnswer: true, WorkspaceDir: t.TempDir(), FileOrder: order}
	if fs := ValidateFile(body("stale"), empty); len(fs) != 0 {
		t.Errorf("no character files: want no findings, got %v", fs)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNewSchedulerOpts_WarnsOnInvalidCharRule proves the gateway half of
// #2223: building the scheduler logs one WARN on the nudge:<agent> logger
// for a character rule the validator rejects (here a regex that does not
// compile), naming the agent, the rule by its index among the character
// rules, and the reason — while a built-in rule with the same fault is not
// validated, and the rule set itself is unchanged (the char rule is kept,
// as before).
func TestNewSchedulerOpts_WarnsOnInvalidCharRule(t *testing.T) {
	log.SetWarnHook(func(log.Level, string, string) {}) // drain any buffered warnings first
	var mu sync.Mutex
	var components, msgs []string
	log.SetWarnHook(func(_ log.Level, component, msg string) {
		mu.Lock()
		defer mu.Unlock()
		components = append(components, component)
		msgs = append(msgs, msg)
	})
	defer log.SetWarnHook(nil)

	rs := &RuleSet{Rules: []Rule{
		{Text: "builtin bad", Priority: "high", Trigger: Trigger{Type: "regex", Pattern: "(builtin"}, Category: CategoryBraindead},
		{Text: "good char", Priority: "high", Trigger: Trigger{Type: "after_error"}, Category: CategoryChar},
		{Text: "bad char", Priority: "high", Trigger: Trigger{Type: "regex", Pattern: "(unclosed"}, Category: CategoryChar},
	}}
	s := NewSchedulerOpts(rs, SchedulerOpts{CanPostTool: true, CanPreAnswer: true, AgentID: "clutch"})

	mu.Lock()
	defer mu.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("want 1 WARN (the char rule only), got %d: %v", len(msgs), msgs)
	}
	if components[0] != "nudge:clutch" {
		t.Errorf("component = %q, want nudge:clutch", components[0])
	}
	for _, want := range []string{"clutch", `rule 1 ("bad char")`, `"pattern" does not compile`} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("WARN %q lacks %q", msgs[0], want)
		}
	}
	if len(s.rules) != 3 {
		t.Errorf("rules kept = %d, want 3: the WARN must not change which rules are kept", len(s.rules))
	}
}

// TestNewSchedulerOpts_BuiltinRulesNotWarned pins (characterisation) that
// the built-in rules the gateway ships pass with no validation WARN: only
// character rules are validated.
func TestNewSchedulerOpts_BuiltinRulesNotWarned(t *testing.T) {
	log.SetWarnHook(func(log.Level, string, string) {})
	var mu sync.Mutex
	var msgs []string
	log.SetWarnHook(func(_ log.Level, _ string, msg string) {
		mu.Lock()
		defer mu.Unlock()
		msgs = append(msgs, msg)
	})
	defer log.SetWarnHook(nil)

	rules := append(BraindeadRule(), DefaultRules([]string{"Bash"}, nil)...)
	rules = append(rules, ScratchpadRule(func() bool { return true })...)
	NewSchedulerOpts(&RuleSet{Rules: rules}, SchedulerOpts{CanPostTool: true, CanPreAnswer: true, AgentID: "clutch"})

	mu.Lock()
	defer mu.Unlock()
	if len(msgs) != 0 {
		t.Errorf("built-in rules: want no WARN, got %v", msgs)
	}
}

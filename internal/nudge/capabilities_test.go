package nudge

import (
	"strings"
	"testing"

	"foci/internal/log"
)

func TestNewSchedulerOpts_FiltersUnsupportedRules(t *testing.T) {
	rs := &RuleSet{
		Rules: []Rule{
			{Text: "every-n-tools", Trigger: Trigger{Type: "every_n_tools", N: 3}},
			{Text: "after-error", Trigger: Trigger{Type: "after_error"}},
			{Text: "tool-pattern", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "Bash"}},
			{Text: "pre-answer", Trigger: Trigger{Type: "pre_answer"}},
			{Text: "every-n-turns", Trigger: Trigger{Type: "every_n_turns", N: 2}},
			{Text: "regex-rule", Trigger: Trigger{Type: "regex", Pattern: "(?i)debug"}},
		},
	}

	t.Run("opencode caps filters mid-turn rules", func(t *testing.T) {
		s := NewSchedulerOpts(rs, SchedulerOpts{
			Cooldown:     1,
			MaxPerBatch:  5,
			CanPostTool:  false,
			CanPreAnswer: false,
		})
		if s == nil {
			t.Fatal("expected non-nil scheduler")
		}

		// Only every_n_turns and regex should survive
		if len(s.rules) != 2 {
			t.Fatalf("expected 2 active rules, got %d", len(s.rules))
		}
		for _, r := range s.rules {
			if r.Trigger.Type != "every_n_turns" && r.Trigger.Type != "regex" {
				t.Errorf("unexpected rule type %q survived filtering", r.Trigger.Type)
			}
		}
	})

	t.Run("ccstream caps keeps all rules", func(t *testing.T) {
		s := NewSchedulerOpts(rs, SchedulerOpts{
			Cooldown:     1,
			MaxPerBatch:  5,
			CanPostTool:  true,
			CanPreAnswer: true,
		})
		if s == nil {
			t.Fatal("expected non-nil scheduler")
		}
		if len(s.rules) != 6 {
			t.Fatalf("expected 6 active rules (all), got %d", len(s.rules))
		}
	})

	t.Run("NewScheduler defaults to all-true caps", func(t *testing.T) {
		s := NewScheduler(rs, 1, 5)
		if s == nil {
			t.Fatal("expected non-nil scheduler")
		}
		if len(s.rules) != 6 {
			t.Fatalf("NewScheduler should keep all rules, got %d", len(s.rules))
		}
	})
}

func TestNewSchedulerOpts_SkipWarningNamesAgent(t *testing.T) {
	log.SetWarnHook(func(log.Level, string, string) {}) // drain any buffered warnings first
	var components, msgs []string
	log.SetWarnHook(func(_ log.Level, component, msg string) {
		components = append(components, component)
		msgs = append(msgs, msg)
	})
	defer log.SetWarnHook(nil)

	rs := &RuleSet{Rules: []Rule{
		{Text: "user rule", Trigger: Trigger{Type: "every_n_tools", N: 3}, Category: CategoryChar},
		{Text: "builtin braindead", Trigger: Trigger{Type: "every_n_tools", N: 3}, Category: CategoryBraindead},
	}}
	NewSchedulerOpts(rs, SchedulerOpts{CanPostTool: false, CanPreAnswer: false, AgentID: "clutch"})

	// Only the user-controllable (char) rule warns; the built-in is skipped silently.
	if len(msgs) != 1 {
		t.Fatalf("expected 1 skip warning (char rule only), got %d: %v", len(msgs), msgs)
	}
	// The agent id is carried in the log component ([nudge:clutch]), not the message.
	if components[0] != "nudge:clutch" {
		t.Errorf("skip warning should name the agent in the component, got %q", components[0])
	}
	if !strings.Contains(msgs[0], "user rule") {
		t.Errorf("warning should be for the char rule, got %q", msgs[0])
	}
}

func TestNewSchedulerOpts_NilRuleSet(t *testing.T) {
	s := NewSchedulerOpts(nil, SchedulerOpts{CanPostTool: true})
	if s != nil {
		t.Error("expected nil scheduler for nil RuleSet")
	}
}

func TestTriggerRequiresPostTool(t *testing.T) {
	tests := []struct {
		trigger string
		want    bool
	}{
		{"every_n_tools", true},
		{"after_error", true},
		{"tool_pattern", true},
		{"pre_answer", false},
		{"every_n_turns", false},
		{"regex", false},
		{"unknown", false},
	}
	for _, tt := range tests {
		t.Run(tt.trigger, func(t *testing.T) {
			if got := TriggerRequiresPostTool(tt.trigger); got != tt.want {
				t.Errorf("TriggerRequiresPostTool(%q) = %v, want %v", tt.trigger, got, tt.want)
			}
		})
	}
}

func TestTriggerRequiresPreAnswer(t *testing.T) {
	tests := []struct {
		trigger string
		want    bool
	}{
		{"pre_answer", true},
		{"every_n_tools", false},
		{"after_error", false},
		{"every_n_turns", false},
		{"regex", false},
	}
	for _, tt := range tests {
		t.Run(tt.trigger, func(t *testing.T) {
			if got := TriggerRequiresPreAnswer(tt.trigger); got != tt.want {
				t.Errorf("TriggerRequiresPreAnswer(%q) = %v, want %v", tt.trigger, got, tt.want)
			}
		})
	}
}

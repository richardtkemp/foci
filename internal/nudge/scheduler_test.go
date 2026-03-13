package nudge

import (
	"testing"
)

func makeTestRuleSet() *RuleSet {
	return &RuleSet{
		ContentHash: "test",
		Rules: []Rule{
			{Text: "periodic-3", Trigger: Trigger{Type: "periodic", N: 3}, Priority: "high"},
			{Text: "pre-answer", Trigger: Trigger{Type: "pre_answer"}, Priority: "high"},
			{Text: "streak-2", Trigger: Trigger{Type: "after_streak", N: 2}, Priority: "medium"},
			{Text: "on-error", Trigger: Trigger{Type: "after_error"}, Priority: "medium"},
			{Text: "match-debug", Trigger: Trigger{Type: "match", Pattern: "(?i)debug"}, Priority: "low"},
		},
	}
}

func TestPeriodicTrigger(t *testing.T) {
	// Verifies periodic rules fire at the correct intervals.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 1) // cooldown=1 to avoid interference
	s.StartTurn("hello")

	// Tool calls 0, 1 should not fire (periodic N=3 fires at multiples of 3)
	if r := s.CheckAfterTools(0, 1, false); r != "" {
		t.Errorf("loop 0: unexpected reminder %q", r)
	}
	if r := s.CheckAfterTools(1, 1, false); r != "" {
		t.Errorf("loop 1: unexpected reminder %q", r)
	}

	// Tool call 2 → toolCount=3, should fire (3%3==0)
	r := s.CheckAfterTools(2, 1, false)
	if r != "periodic-3" {
		t.Errorf("loop 2: expected periodic-3, got %q", r)
	}

	// Tool calls 3,4 should not fire
	if r := s.CheckAfterTools(3, 1, false); r != "" {
		t.Errorf("loop 3: unexpected reminder %q", r)
	}
	if r := s.CheckAfterTools(4, 1, false); r != "" {
		t.Errorf("loop 4: unexpected reminder %q", r)
	}

	// Tool call 5 → toolCount=6, should fire again (6%3==0)
	r = s.CheckAfterTools(5, 1, false)
	if r != "periodic-3" {
		t.Errorf("loop 5: expected periodic-3, got %q", r)
	}
}

func TestAfterStreakTrigger(t *testing.T) {
	// Verifies streak detection fires after N same-tool calls.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5) // allow multiple per batch
	s.StartTurn("hello")

	// streak=1: should not fire
	if r := s.CheckAfterTools(0, 1, false); r != "" {
		t.Errorf("streak 1: unexpected %q", r)
	}

	// streak=2: should fire
	r := s.CheckAfterTools(1, 2, false)
	if r == "" {
		t.Error("streak 2: expected reminder")
	}
	if r != "streak-2" {
		t.Errorf("streak 2: expected streak-2, got %q", r)
	}
}

func TestAfterErrorTrigger(t *testing.T) {
	// Verifies error trigger fires when lastToolError is true.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5) // allow multiple per batch
	s.StartTurn("hello")

	// No error: should not fire after_error rule
	r := s.CheckAfterTools(0, 1, false)
	if r != "" {
		t.Errorf("no error: unexpected %q", r)
	}

	// Error: should fire after_error
	r = s.CheckAfterTools(1, 1, true)
	if r == "" {
		t.Error("error: expected reminder")
	}
	if r != "on-error" {
		t.Errorf("error: expected on-error, got %q", r)
	}
}

func TestMatchTrigger(t *testing.T) {
	// Verifies regex match fires when user message matches.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Please debug this issue")

	r := s.CheckAfterTools(0, 1, false)
	if r == "" {
		t.Error("match: expected reminder for 'debug' message")
	}
	if r != "match-debug" {
		t.Errorf("match: expected match-debug, got %q", r)
	}
}

func TestMatchTriggerNoMatch(t *testing.T) {
	// Verifies match doesn't fire when message doesn't match.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Hello world")

	r := s.CheckAfterTools(0, 1, false)
	if r != "" {
		t.Errorf("no match: unexpected %q", r)
	}
}

func TestPreAnswerTrigger(t *testing.T) {
	// Verifies pre_answer rules are returned by CheckPreAnswer
	// and not by CheckAfterTools.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("hello")

	// pre_answer should not fire in CheckAfterTools
	// (other rules might fire, but pre_answer specifically should not)
	s.CheckAfterTools(0, 1, false)

	// CheckPreAnswer should return the pre_answer rule
	r := s.CheckPreAnswer()
	if r != "pre-answer" {
		t.Errorf("pre-answer: expected 'pre-answer', got %q", r)
	}
}

func TestHasPreAnswerRules(t *testing.T) {
	// Checks the convenience method.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 5, 1)
	if !s.HasPreAnswerRules() {
		t.Error("expected HasPreAnswerRules=true")
	}

	s2 := NewScheduler(&RuleSet{
		Rules: []Rule{{Text: "test", Trigger: Trigger{Type: "periodic", N: 3}}},
	}, 5, 1)
	if s2.HasPreAnswerRules() {
		t.Error("expected HasPreAnswerRules=false with no pre_answer rules")
	}
}

func TestCooldownPreventsSpam(t *testing.T) {
	// Verifies the cooldown prevents firing the same rule
	// too frequently.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "check", Trigger: Trigger{Type: "after_error"}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 3, 1) // cooldown=3
	s.StartTurn("hello")

	// First error: should fire
	r := s.CheckAfterTools(0, 1, true)
	if r != "check" {
		t.Errorf("first error: expected 'check', got %q", r)
	}

	// Next 2 calls with errors: cooldown should prevent firing
	r = s.CheckAfterTools(1, 1, true)
	if r != "" {
		t.Errorf("cooldown 1: unexpected %q", r)
	}
	r = s.CheckAfterTools(2, 1, true)
	if r != "" {
		t.Errorf("cooldown 2: unexpected %q", r)
	}

	// 4th call: cooldown expired (toolCount=4, last=1, diff=3)
	r = s.CheckAfterTools(3, 1, true)
	if r != "check" {
		t.Errorf("after cooldown: expected 'check', got %q", r)
	}
}

func TestMaxPerBatchLimits(t *testing.T) {
	// Verifies only maxPerBatch rules fire per check.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "rule1", Trigger: Trigger{Type: "after_error"}, Priority: "high"},
			{Text: "rule2", Trigger: Trigger{Type: "after_error"}, Priority: "medium"},
		},
	}
	s := NewScheduler(rs, 1, 1) // maxPerBatch=1
	s.StartTurn("hello")

	// Both rules match (error), but only 1 should fire
	r := s.CheckAfterTools(0, 1, true)
	if r != "rule1" {
		t.Errorf("expected 'rule1', got %q", r)
	}
}

func TestCheckMatchFiresWithoutTools(t *testing.T) {
	// Verifies CheckMatch returns unfired match
	// rules, ensuring match triggers work even on turns with no tool calls.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Please debug this issue")

	// No CheckAfterTools called — simulate a no-tools turn.
	r := s.CheckMatch()
	if r != "match-debug" {
		t.Errorf("CheckMatch: expected match-debug, got %q", r)
	}

	// Second call should return "" — already fired.
	r = s.CheckMatch()
	if r != "" {
		t.Errorf("CheckMatch second call: expected empty, got %q", r)
	}
}

func TestCheckMatchNoopAfterToolsFired(t *testing.T) {
	// Verifies CheckMatch returns "" when
	// the match rule already fired via CheckAfterTools.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Please debug this issue")

	// Fire via tools path first.
	s.CheckAfterTools(0, 1, false)

	// CheckMatch should find nothing unfired.
	r := s.CheckMatch()
	if r != "" {
		t.Errorf("CheckMatch after tools: expected empty, got %q", r)
	}
}

func TestCheckMatchNoMatch(t *testing.T) {
	// Verifies CheckMatch returns "" when no patterns match.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Hello world")

	r := s.CheckMatch()
	if r != "" {
		t.Errorf("CheckMatch no match: expected empty, got %q", r)
	}
}

func TestNilSchedulerSafe(t *testing.T) {
	// Verifies nil scheduler doesn't panic.
	t.Parallel()

	var s *Scheduler
	s.StartTurn("hello")
	if r := s.CheckAfterTools(0, 1, true); r != "" {
		t.Errorf("nil scheduler returned %q", r)
	}
	if r := s.CheckPreAnswer(); r != "" {
		t.Errorf("nil scheduler returned %q", r)
	}
	if r := s.CheckMatch(); r != "" {
		t.Errorf("nil scheduler CheckMatch returned %q", r)
	}
	if s.HasPreAnswerRules() {
		t.Error("nil scheduler HasPreAnswerRules should be false")
	}
}

func TestStartTurnClearsState(t *testing.T) {
	// Verifies that StartTurn clears per-turn state.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "check", Trigger: Trigger{Type: "after_error"}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 5, 1) // cooldown=5
	s.StartTurn("hello")

	// Fire once
	s.CheckAfterTools(0, 1, true)

	// StartTurn should clear cooldown
	s.StartTurn("new message")
	r := s.CheckAfterTools(0, 1, true)
	if r != "check" {
		t.Errorf("after reset: expected 'check', got %q", r)
	}
}

func TestNewSchedulerNilRuleSet(t *testing.T) {
	// Returns nil scheduler.
	t.Parallel()

	s := NewScheduler(nil, 5, 1)
	if s != nil {
		t.Error("expected nil scheduler for nil RuleSet")
	}
}

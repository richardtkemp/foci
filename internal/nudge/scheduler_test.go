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
	if r := s.CheckAfterTools(0, 1, false); len(r) != 0 {
		t.Errorf("loop 0: unexpected reminder %q", r)
	}
	if r := s.CheckAfterTools(1, 1, false); len(r) != 0 {
		t.Errorf("loop 1: unexpected reminder %q", r)
	}

	// Tool call 2 → toolCount=3, should fire (3%3==0)
	r := s.CheckAfterTools(2, 1, false)
	if len(r) != 1 || r[0] != "periodic-3" {
		t.Errorf("loop 2: expected [periodic-3], got %q", r)
	}

	// Tool calls 3,4 should not fire
	if r := s.CheckAfterTools(3, 1, false); len(r) != 0 {
		t.Errorf("loop 3: unexpected reminder %q", r)
	}
	if r := s.CheckAfterTools(4, 1, false); len(r) != 0 {
		t.Errorf("loop 4: unexpected reminder %q", r)
	}

	// Tool call 5 → toolCount=6, should fire again (6%3==0)
	r = s.CheckAfterTools(5, 1, false)
	if len(r) != 1 || r[0] != "periodic-3" {
		t.Errorf("loop 5: expected [periodic-3], got %q", r)
	}
}

func TestAfterStreakTrigger(t *testing.T) {
	// Verifies streak detection fires after N same-tool calls.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5) // allow multiple per batch
	s.StartTurn("hello")

	// streak=1: should not fire
	if r := s.CheckAfterTools(0, 1, false); len(r) != 0 {
		t.Errorf("streak 1: unexpected %q", r)
	}

	// streak=2: should fire
	r := s.CheckAfterTools(1, 2, false)
	if len(r) == 0 {
		t.Error("streak 2: expected reminder")
	}
	if len(r) != 1 || r[0] != "streak-2" {
		t.Errorf("streak 2: expected [streak-2], got %q", r)
	}
}

func TestAfterErrorTrigger(t *testing.T) {
	// Verifies error trigger fires when lastToolError is true.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5) // allow multiple per batch
	s.StartTurn("hello")

	// No error: should not fire after_error rule
	r := s.CheckAfterTools(0, 1, false)
	if len(r) != 0 {
		t.Errorf("no error: unexpected %q", r)
	}

	// Error: should fire after_error
	r = s.CheckAfterTools(1, 1, true)
	if len(r) == 0 {
		t.Error("error: expected reminder")
	}
	if len(r) != 1 || r[0] != "on-error" {
		t.Errorf("error: expected [on-error], got %q", r)
	}
}

func TestMatchTrigger(t *testing.T) {
	// Verifies regex match fires when user message matches.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Please debug this issue")

	r := s.CheckAfterTools(0, 1, false)
	if len(r) == 0 {
		t.Error("match: expected reminder for 'debug' message")
	}
	if len(r) != 1 || r[0] != "match-debug" {
		t.Errorf("match: expected [match-debug], got %q", r)
	}
}

func TestMatchTriggerNoMatch(t *testing.T) {
	// Verifies match doesn't fire when message doesn't match.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Hello world")

	r := s.CheckAfterTools(0, 1, false)
	if len(r) != 0 {
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
	if len(r) != 1 || r[0] != "check" {
		t.Errorf("first error: expected [check], got %q", r)
	}

	// Next 2 calls with errors: cooldown should prevent firing
	r = s.CheckAfterTools(1, 1, true)
	if len(r) != 0 {
		t.Errorf("cooldown 1: unexpected %q", r)
	}
	r = s.CheckAfterTools(2, 1, true)
	if len(r) != 0 {
		t.Errorf("cooldown 2: unexpected %q", r)
	}

	// 4th call: cooldown expired (toolCount=4, last=1, diff=3)
	r = s.CheckAfterTools(3, 1, true)
	if len(r) != 1 || r[0] != "check" {
		t.Errorf("after cooldown: expected [check], got %q", r)
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
	if len(r) != 1 || r[0] != "rule1" {
		t.Errorf("expected [rule1], got %q", r)
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
	if len(r) != 1 || r[0] != "match-debug" {
		t.Errorf("CheckMatch: expected [match-debug], got %q", r)
	}

	// Second call should return nil — already fired.
	r = s.CheckMatch()
	if len(r) != 0 {
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
	if len(r) != 0 {
		t.Errorf("CheckMatch after tools: expected empty, got %q", r)
	}
}

func TestCheckMatchNoMatch(t *testing.T) {
	// Verifies CheckMatch returns "" when no patterns match.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Hello world")

	r := s.CheckMatch()
	if len(r) != 0 {
		t.Errorf("CheckMatch no match: expected empty, got %q", r)
	}
}

func TestPeriodicTurnTrigger(t *testing.T) {
	// Verifies periodic_turn rules fire at the correct turn intervals
	// and accumulate across turns (never reset).
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "tool-reminder", Trigger: Trigger{Type: "periodic_turn", N: 3}, Priority: "low"},
		},
	}
	s := NewScheduler(rs, 1, 5)

	// Turns 1, 2: should not fire
	s.StartTurn("msg1")
	if r := s.CheckTurnPeriodic(); len(r) != 0 {
		t.Errorf("turn 1: unexpected %q", r)
	}
	s.StartTurn("msg2")
	if r := s.CheckTurnPeriodic(); len(r) != 0 {
		t.Errorf("turn 2: unexpected %q", r)
	}

	// Turn 3: should fire (3%3==0)
	s.StartTurn("msg3")
	r := s.CheckTurnPeriodic()
	if len(r) != 1 || r[0] != "tool-reminder" {
		t.Errorf("turn 3: expected [tool-reminder], got %q", r)
	}

	// Turns 4, 5: should not fire
	s.StartTurn("msg4")
	if r := s.CheckTurnPeriodic(); len(r) != 0 {
		t.Errorf("turn 4: unexpected %q", r)
	}
	s.StartTurn("msg5")
	if r := s.CheckTurnPeriodic(); len(r) != 0 {
		t.Errorf("turn 5: unexpected %q", r)
	}

	// Turn 6: should fire again (6%3==0)
	s.StartTurn("msg6")
	r = s.CheckTurnPeriodic()
	if len(r) != 1 || r[0] != "tool-reminder" {
		t.Errorf("turn 6: expected [tool-reminder], got %q", r)
	}
}

func TestPeriodicTurnNotInCheckAfterTools(t *testing.T) {
	// Verifies periodic_turn rules do NOT fire via CheckAfterTools —
	// they only fire via CheckTurnPeriodic().
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "tool-reminder", Trigger: Trigger{Type: "periodic_turn", N: 1}, Priority: "low"},
		},
	}
	s := NewScheduler(rs, 1, 5)
	s.StartTurn("msg")

	// CheckAfterTools should never return periodic_turn rules
	r := s.CheckAfterTools(0, 1, false)
	if len(r) != 0 {
		t.Errorf("CheckAfterTools returned periodic_turn rule: %q", r)
	}
}

func TestCheckTurnPeriodicNil(t *testing.T) {
	// Verifies nil scheduler doesn't panic on CheckTurnPeriodic.
	t.Parallel()

	var s *Scheduler
	if r := s.CheckTurnPeriodic(); len(r) != 0 {
		t.Errorf("nil scheduler CheckTurnPeriodic returned %q", r)
	}
}

func TestNilSchedulerSafe(t *testing.T) {
	// Verifies nil scheduler doesn't panic.
	t.Parallel()

	var s *Scheduler
	s.StartTurn("hello")
	if r := s.CheckAfterTools(0, 1, true); len(r) != 0 {
		t.Errorf("nil scheduler returned %q", r)
	}
	if r := s.CheckPreAnswer(); r != "" {
		t.Errorf("nil scheduler returned %q", r)
	}
	if r := s.CheckMatch(); len(r) != 0 {
		t.Errorf("nil scheduler CheckMatch returned %q", r)
	}
	if r := s.CheckTurnPeriodic(); len(r) != 0 {
		t.Errorf("nil scheduler CheckTurnPeriodic returned %q", r)
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
	if len(r) != 1 || r[0] != "check" {
		t.Errorf("after reset: expected [check], got %q", r)
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

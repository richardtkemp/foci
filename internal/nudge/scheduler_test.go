package nudge

import (
	"testing"
)

func makeTestRuleSet() *RuleSet {
	return &RuleSet{
		ContentHash: "test",
		Rules: []Rule{
			{Text: "periodic-3", Trigger: Trigger{Type: "every_n_tools", N: 3}, Priority: "high"},
			{Text: "pre-answer", Trigger: Trigger{Type: "pre_answer"}, Priority: "high"},
			{Text: "on-error", Trigger: Trigger{Type: "after_error"}, Priority: "medium"},
			{Text: "regex-debug", Trigger: Trigger{Type: "regex", Pattern: "(?i)debug"}, Priority: "low"},
		},
	}
}

func TestEveryNToolsTrigger(t *testing.T) {
	// Verifies every_n_tools rules fire at the correct intervals,
	// counting individual tool calls (not loop iterations).
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 1) // cooldown=1 to avoid interference
	s.StartTurn("hello")

	// 1, 2 tool calls: should not fire (N=3 fires at multiples of 3)
	if r := s.CheckAfterTools(1, false); len(r) != 0 {
		t.Errorf("1 tool: unexpected reminder %q", r)
	}
	if r := s.CheckAfterTools(2, false); len(r) != 0 {
		t.Errorf("2 tools: unexpected reminder %q", r)
	}

	// 3 tool calls: should fire (3%3==0)
	r := s.CheckAfterTools(3, false)
	if len(r) != 1 || r[0] != "periodic-3" {
		t.Errorf("3 tools: expected [periodic-3], got %q", r)
	}

	// 4, 5 tool calls: should not fire
	if r := s.CheckAfterTools(4, false); len(r) != 0 {
		t.Errorf("4 tools: unexpected reminder %q", r)
	}
	if r := s.CheckAfterTools(5, false); len(r) != 0 {
		t.Errorf("5 tools: unexpected reminder %q", r)
	}

	// 6 tool calls: should fire again (6%3==0)
	r = s.CheckAfterTools(6, false)
	if len(r) != 1 || r[0] != "periodic-3" {
		t.Errorf("6 tools: expected [periodic-3], got %q", r)
	}
}

func TestAfterErrorTrigger(t *testing.T) {
	// Verifies error trigger fires when lastToolError is true.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5) // allow multiple per batch
	s.StartTurn("hello")

	// No error: should not fire after_error rule
	r := s.CheckAfterTools(1, false)
	if len(r) != 0 {
		t.Errorf("no error: unexpected %q", r)
	}

	// Error: should fire after_error
	r = s.CheckAfterTools(2, true)
	if len(r) == 0 {
		t.Error("error: expected reminder")
	}
	if len(r) != 1 || r[0] != "on-error" {
		t.Errorf("error: expected [on-error], got %q", r)
	}
}

func TestRegexTrigger(t *testing.T) {
	// Verifies regex fires when user message matches.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Please debug this issue")

	r := s.CheckAfterTools(1, false)
	if len(r) == 0 {
		t.Error("regex: expected reminder for 'debug' message")
	}
	if len(r) != 1 || r[0] != "regex-debug" {
		t.Errorf("regex: expected [regex-debug], got %q", r)
	}
}

func TestRegexTriggerNoMatch(t *testing.T) {
	// Verifies regex doesn't fire when message doesn't match.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Hello world")

	r := s.CheckAfterTools(1, false)
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
	s.CheckAfterTools(1, false)

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
		Rules: []Rule{{Text: "test", Trigger: Trigger{Type: "every_n_tools", N: 3}}},
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
	r := s.CheckAfterTools(1, true)
	if len(r) != 1 || r[0] != "check" {
		t.Errorf("first error: expected [check], got %q", r)
	}

	// Next 2 calls with errors: cooldown should prevent firing
	r = s.CheckAfterTools(2, true)
	if len(r) != 0 {
		t.Errorf("cooldown 1: unexpected %q", r)
	}
	r = s.CheckAfterTools(3, true)
	if len(r) != 0 {
		t.Errorf("cooldown 2: unexpected %q", r)
	}

	// 4th call: cooldown expired (toolCount=4, last=1, diff=3)
	r = s.CheckAfterTools(4, true)
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
	r := s.CheckAfterTools(1, true)
	if len(r) != 1 || r[0] != "rule1" {
		t.Errorf("expected [rule1], got %q", r)
	}
}

func TestCheckRegexFiresWithoutTools(t *testing.T) {
	// Verifies CheckRegex returns unfired regex
	// rules, ensuring regex triggers work even on turns with no tool calls.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Please debug this issue")

	// No CheckAfterTools called — simulate a no-tools turn.
	r := s.CheckRegex()
	if len(r) != 1 || r[0] != "regex-debug" {
		t.Errorf("CheckRegex: expected [regex-debug], got %q", r)
	}

	// Second call should return nil — already fired.
	r = s.CheckRegex()
	if len(r) != 0 {
		t.Errorf("CheckRegex second call: expected empty, got %q", r)
	}
}

func TestCheckRegexNoopAfterToolsFired(t *testing.T) {
	// Verifies CheckRegex returns nil when
	// the regex rule already fired via CheckAfterTools.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Please debug this issue")

	// Fire via tools path first.
	s.CheckAfterTools(1, false)

	// CheckRegex should find nothing unfired.
	r := s.CheckRegex()
	if len(r) != 0 {
		t.Errorf("CheckRegex after tools: expected empty, got %q", r)
	}
}

func TestCheckRegexNoMatch(t *testing.T) {
	// Verifies CheckRegex returns nil when no patterns match.
	t.Parallel()

	s := NewScheduler(makeTestRuleSet(), 1, 5)
	s.StartTurn("Hello world")

	r := s.CheckRegex()
	if len(r) != 0 {
		t.Errorf("CheckRegex no match: expected empty, got %q", r)
	}
}

func TestEveryNTurnsTrigger(t *testing.T) {
	// Verifies every_n_turns rules fire at the correct turn intervals
	// and accumulate across turns (never reset).
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "tool-reminder", Trigger: Trigger{Type: "every_n_turns", N: 3}, Priority: "low"},
		},
	}
	s := NewScheduler(rs, 1, 5)

	// Turns 1, 2: should not fire
	s.StartTurn("msg1")
	if r := s.CheckTurnInterval(); len(r) != 0 {
		t.Errorf("turn 1: unexpected %q", r)
	}
	s.StartTurn("msg2")
	if r := s.CheckTurnInterval(); len(r) != 0 {
		t.Errorf("turn 2: unexpected %q", r)
	}

	// Turn 3: should fire (3%3==0)
	s.StartTurn("msg3")
	r := s.CheckTurnInterval()
	if len(r) != 1 || r[0] != "tool-reminder" {
		t.Errorf("turn 3: expected [tool-reminder], got %q", r)
	}

	// Turns 4, 5: should not fire
	s.StartTurn("msg4")
	if r := s.CheckTurnInterval(); len(r) != 0 {
		t.Errorf("turn 4: unexpected %q", r)
	}
	s.StartTurn("msg5")
	if r := s.CheckTurnInterval(); len(r) != 0 {
		t.Errorf("turn 5: unexpected %q", r)
	}

	// Turn 6: should fire again (6%3==0)
	s.StartTurn("msg6")
	r = s.CheckTurnInterval()
	if len(r) != 1 || r[0] != "tool-reminder" {
		t.Errorf("turn 6: expected [tool-reminder], got %q", r)
	}
}

func TestEveryNTurnsNotInCheckAfterTools(t *testing.T) {
	// Verifies every_n_turns rules do NOT fire via CheckAfterTools —
	// they only fire via CheckTurnInterval().
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "tool-reminder", Trigger: Trigger{Type: "every_n_turns", N: 1}, Priority: "low"},
		},
	}
	s := NewScheduler(rs, 1, 5)
	s.StartTurn("msg")

	// CheckAfterTools should never return every_n_turns rules
	r := s.CheckAfterTools(1, false)
	if len(r) != 0 {
		t.Errorf("CheckAfterTools returned every_n_turns rule: %q", r)
	}
}

func TestCheckTurnIntervalNil(t *testing.T) {
	// Verifies nil scheduler doesn't panic on CheckTurnInterval.
	t.Parallel()

	var s *Scheduler
	if r := s.CheckTurnInterval(); len(r) != 0 {
		t.Errorf("nil scheduler CheckTurnInterval returned %q", r)
	}
}

func TestNilSchedulerSafe(t *testing.T) {
	// Verifies nil scheduler doesn't panic.
	t.Parallel()

	var s *Scheduler
	s.StartTurn("hello")
	if r := s.CheckAfterTools(1, true); len(r) != 0 {
		t.Errorf("nil scheduler returned %q", r)
	}
	if r := s.CheckPreAnswer(); r != "" {
		t.Errorf("nil scheduler returned %q", r)
	}
	if r := s.CheckRegex(); len(r) != 0 {
		t.Errorf("nil scheduler CheckRegex returned %q", r)
	}
	if r := s.CheckTurnInterval(); len(r) != 0 {
		t.Errorf("nil scheduler CheckTurnInterval returned %q", r)
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
	s.CheckAfterTools(1, true)

	// StartTurn should clear cooldown
	s.StartTurn("new message")
	r := s.CheckAfterTools(1, true)
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

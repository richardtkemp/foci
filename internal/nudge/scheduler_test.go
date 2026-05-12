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

func TestConditionGatesEveryNTurns(t *testing.T) {
	// Verifies that a rule with a Condition function only fires via
	// CheckTurnInterval when the condition returns true.
	t.Parallel()

	conditionResult := false
	rs := &RuleSet{
		Rules: []Rule{
			{
				Text:      "scratchpad-check",
				Trigger:   Trigger{Type: "every_n_turns", N: 2},
				Priority:  "low",
				Condition: func() bool { return conditionResult },
			},
		},
	}
	s := NewScheduler(rs, 1, 5)

	// Turn 2: interval matches but condition is false — should not fire
	s.StartTurn("msg1")
	s.StartTurn("msg2")
	r := s.CheckTurnInterval()
	if len(r) != 0 {
		t.Errorf("condition false: expected no reminders, got %q", r)
	}

	// Turn 4: interval matches and condition is true — should fire
	conditionResult = true
	s.StartTurn("msg3")
	s.StartTurn("msg4")
	r = s.CheckTurnInterval()
	if len(r) != 1 || r[0] != "scratchpad-check" {
		t.Errorf("condition true: expected [scratchpad-check], got %q", r)
	}
}

func TestConditionGatesAfterTools(t *testing.T) {
	// Verifies that a rule with a Condition function only fires via
	// CheckAfterTools when the condition returns true.
	t.Parallel()

	conditionResult := false
	rs := &RuleSet{
		Rules: []Rule{
			{
				Text:      "conditional-error",
				Trigger:   Trigger{Type: "after_error"},
				Priority:  "high",
				Condition: func() bool { return conditionResult },
			},
		},
	}
	s := NewScheduler(rs, 1, 5)
	s.StartTurn("msg")

	// Error present but condition is false — should not fire
	r := s.CheckAfterTools(1, true)
	if len(r) != 0 {
		t.Errorf("condition false: expected no reminders, got %q", r)
	}

	// Error present and condition is true — should fire
	conditionResult = true
	r = s.CheckAfterTools(2, true)
	if len(r) != 1 || r[0] != "conditional-error" {
		t.Errorf("condition true: expected [conditional-error], got %q", r)
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

// ---------------------------------------------------------------------------
// tool_pattern trigger tests
//
// These cover the tool-context nudges that fire based on the ring buffer of
// recent (toolName, toolInput) pairs maintained by RecordToolCall. The
// scheduler treats an empty ToolPattern / InputPattern as "no constraint",
// and Consecutive defaults to 1.
// ---------------------------------------------------------------------------

func TestToolPatternToolNameMatch(t *testing.T) {
	// A bare tool_pattern with only ToolPattern fires when the most recent
	// tool name matches.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "saw-bash", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "^Bash$"}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("hello")

	s.RecordToolCall("Read", `{"file_path":"/x"}`)
	if got := s.CheckAfterTools(1, false); len(got) != 0 {
		t.Errorf("Read tool: unexpected fire %v", got)
	}

	s.RecordToolCall("Bash", `{"command":"ls"}`)
	got := s.CheckAfterTools(2, false)
	if len(got) != 1 || got[0] != "saw-bash" {
		t.Errorf("Bash tool: expected [saw-bash], got %v", got)
	}
}

func TestToolPatternInputMatch(t *testing.T) {
	// A tool_pattern with only InputPattern fires when the input JSON matches,
	// regardless of tool name.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "danger", Trigger: Trigger{Type: "tool_pattern", InputPattern: `rm -rf`}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("hello")

	s.RecordToolCall("Bash", `{"command":"ls"}`)
	if got := s.CheckAfterTools(1, false); len(got) != 0 {
		t.Errorf("benign command: unexpected fire %v", got)
	}

	s.RecordToolCall("Bash", `{"command":"rm -rf /tmp/foo"}`)
	got := s.CheckAfterTools(2, false)
	if len(got) != 1 || got[0] != "danger" {
		t.Errorf("rm -rf command: expected [danger], got %v", got)
	}
}

func TestToolPatternConsecutive(t *testing.T) {
	// Consecutive: N requires the N most-recent events to all match.
	// One non-match breaks the streak.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "stop-reading", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "^(Read|Grep|Glob)$", Consecutive: 3}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("hello")

	// Two consecutive Reads — not enough.
	s.RecordToolCall("Read", "")
	if got := s.CheckAfterTools(1, false); len(got) != 0 {
		t.Errorf("1 Read: unexpected fire %v", got)
	}
	s.RecordToolCall("Read", "")
	if got := s.CheckAfterTools(2, false); len(got) != 0 {
		t.Errorf("2 Reads: unexpected fire %v", got)
	}

	// Third consecutive — fires.
	s.RecordToolCall("Grep", "")
	got := s.CheckAfterTools(3, false)
	if len(got) != 1 || got[0] != "stop-reading" {
		t.Errorf("3 reads: expected [stop-reading], got %v", got)
	}

	// A non-Read in the middle breaks the streak. Next single Read won't fire.
	s.RecordToolCall("Bash", "")
	if got := s.CheckAfterTools(4, false); len(got) != 0 {
		t.Errorf("Bash after Reads: unexpected fire %v", got)
	}
	s.RecordToolCall("Read", "")
	// Cooldown applies (cooldown=1 means need 1 tool between fires; 3→4 is 1, but
	// the streak was broken so it shouldn't fire anyway).
	if got := s.CheckAfterTools(5, false); len(got) != 0 {
		t.Errorf("single Read after break: unexpected fire %v", got)
	}
}

func TestToolPatternBothFields(t *testing.T) {
	// When both ToolPattern and InputPattern are set, both must match.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "edit-character", Trigger: Trigger{
				Type:         "tool_pattern",
				ToolPattern:  "^(Write|Edit)$",
				InputPattern: `/character/[^/]+\.md`,
			}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("hello")

	// Edit but wrong path → no fire.
	s.RecordToolCall("Edit", `{"file_path":"/tmp/x.md"}`)
	if got := s.CheckAfterTools(1, false); len(got) != 0 {
		t.Errorf("non-character edit: unexpected fire %v", got)
	}

	// Read of character file → no fire (wrong tool).
	s.RecordToolCall("Read", `{"file_path":"/home/foci/clutch/character/SOUL.md"}`)
	if got := s.CheckAfterTools(2, false); len(got) != 0 {
		t.Errorf("Read of character: unexpected fire %v", got)
	}

	// Edit of character file → fires.
	s.RecordToolCall("Edit", `{"file_path":"/home/foci/clutch/character/SOUL.md"}`)
	got := s.CheckAfterTools(3, false)
	if len(got) != 1 || got[0] != "edit-character" {
		t.Errorf("character edit: expected [edit-character], got %v", got)
	}
}

func TestToolPatternCompileFailureDoesNotFire(t *testing.T) {
	// Malformed regex → rule never fires, scheduler stays functional.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "broken", Trigger: Trigger{Type: "tool_pattern", ToolPattern: `[unclosed`}, Priority: "high"},
			{Text: "ok", Trigger: Trigger{Type: "tool_pattern", ToolPattern: `^Bash$`}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("hello")

	s.RecordToolCall("Bash", "")
	got := s.CheckAfterTools(1, false)
	// "ok" should fire; "broken" must not.
	if len(got) != 1 || got[0] != "ok" {
		t.Errorf("expected only [ok], got %v", got)
	}
}

func TestRingBufferDepthBound(t *testing.T) {
	// Verifies the recent ring buffer caps at recentBufferDepth and tail
	// matching keeps working after wraparound.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "bash-tail", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "^Bash$", Consecutive: 2}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("hello")

	// Fill the buffer with non-matching Reads beyond capacity.
	for i := 0; i < recentBufferDepth*2; i++ {
		s.RecordToolCall("Read", "")
	}
	if got := s.CheckAfterTools(1, false); len(got) != 0 {
		t.Errorf("Reads only: unexpected fire %v", got)
	}

	// Two Bash calls become the tail — fires.
	s.RecordToolCall("Bash", "")
	s.RecordToolCall("Bash", "")
	got := s.CheckAfterTools(2, false)
	if len(got) != 1 || got[0] != "bash-tail" {
		t.Errorf("tail Bash×2: expected [bash-tail], got %v", got)
	}
}

func TestStartTurnClearsRecentBuffer(t *testing.T) {
	// tool_pattern shouldn't match across turn boundaries.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "two-reads", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "^Read$", Consecutive: 2}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("turn-1")

	// One Read in turn 1.
	s.RecordToolCall("Read", "")
	if got := s.CheckAfterTools(1, false); len(got) != 0 {
		t.Errorf("turn1 single Read: unexpected fire %v", got)
	}

	// New turn — buffer should be cleared.
	s.StartTurn("turn-2")
	// One Read in turn 2 alone — must not fire (buffer reset).
	s.RecordToolCall("Read", "")
	if got := s.CheckAfterTools(1, false); len(got) != 0 {
		t.Errorf("turn2 first Read should not fire (buffer should have cleared): %v", got)
	}

	// Second Read in turn 2 — now fires.
	s.RecordToolCall("Read", "")
	got := s.CheckAfterTools(2, false)
	if len(got) != 1 || got[0] != "two-reads" {
		t.Errorf("turn2 two Reads: expected [two-reads], got %v", got)
	}
}

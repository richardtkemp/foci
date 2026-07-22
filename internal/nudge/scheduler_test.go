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

func TestSchedulerLiveGating(t *testing.T) {
	// The braindead + default rules are always present; Configure gates their
	// firing and supplies the live interval/text with no rebuild (#1228).
	t.Parallel()

	rs := &RuleSet{Rules: []Rule{
		{Text: "builtin-braindead", Trigger: Trigger{Type: "every_n_tools"}, Category: CategoryBraindead},
		{Text: "builtin-default", Trigger: Trigger{Type: "every_n_turns"}, Category: CategoryDefault},
	}}
	s := NewScheduler(rs, 1, 5)

	// Unconfigured: braindead threshold 0, default disabled → silent.
	s.StartTurn("hi")
	if r := s.CheckAfterTools(2, false); len(r) != 0 {
		t.Errorf("braindead off: unexpected %v", r)
	}

	// Enable braindead at threshold 2 with a live prompt override.
	s.Configure(Settings{Cooldown: 1, MaxPerBatch: 5, BraindeadThreshold: 2, BraindeadPrompt: "STOP"})
	s.StartTurn("hi")
	if r := s.CheckAfterTools(2, false); len(r) != 1 || r[0] != "STOP" {
		t.Errorf("braindead on: expected [STOP], got %v", r)
	}

	// Enable default rules at frequency 2 → fires when turnCount is a multiple.
	s.Configure(Settings{Cooldown: 1, MaxPerBatch: 5, DefaultEnable: true, DefaultFreq: 2})
	s.StartTurn("odd") // odd turnCount
	if r := s.CheckTurnInterval(); len(r) != 0 {
		t.Errorf("odd turn: unexpected %v", r)
	}
	s.StartTurn("even") // next turnCount → multiple of 2
	if r := s.CheckTurnInterval(); len(r) != 1 || r[0] != "builtin-default" {
		t.Errorf("even turn freq=2: expected [builtin-default], got %v", r)
	}

	// Disable default rules → silent even when the interval would match.
	s.Configure(Settings{Cooldown: 1, MaxPerBatch: 5})
	s.StartTurn("t")
	s.StartTurn("t")
	if r := s.CheckTurnInterval(); len(r) != 0 {
		t.Errorf("default disabled: unexpected %v", r)
	}
}

// ---------------------------------------------------------------------------
// #1309 — cross-rule tool_pattern cooldown, after_error benign-exit
// exemption, regex/max-per-turn budget.
// ---------------------------------------------------------------------------

func TestToolPatternCrossRuleCooldown(t *testing.T) {
	// Reproduces the "Edit/Write quartet fires on every edit" bug: four
	// independent tool_pattern rules all matching Edit, each individually
	// respecting its own per-rule cooldown, previously round-robined so a
	// DIFFERENT rule fired on every single matching tool call. The shared
	// lastFiredByType cooldown should now throttle the whole tool_pattern
	// category together, so only the FIRST matching edit fires anything
	// within one cooldown window — not one nudge per edit.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "rule-a", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "^(Edit|Write)$"}, Priority: "high"},
			{Text: "rule-b", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "^(Edit|Write)$"}, Priority: "medium"},
			{Text: "rule-c", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "^(Edit|Write)$"}, Priority: "medium"},
			{Text: "rule-d", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "^(Edit|Write)$"}, Priority: "low"},
		},
	}
	// cooldown=5 (the config default), maxPerBatch=1 (also default).
	s := NewScheduler(rs, 5, 1)
	s.StartTurn("hello")

	var allFired []string
	for i := 1; i <= 4; i++ {
		s.RecordToolCall("Edit", `{"file_path":"/tmp/x.go"}`)
		allFired = append(allFired, s.CheckAfterTools(i, false)...)
	}

	// Before the fix, each of the 4 edits fired a DIFFERENT rule (4 total).
	// With the shared cooldown, only the first edit should fire anything —
	// the next 3 stay silent until toolCount-lastFiredByType >= cooldown(5).
	if len(allFired) != 1 {
		t.Errorf("expected exactly 1 nudge across 4 consecutive edits (cross-rule cooldown), got %d: %v", len(allFired), allFired)
	}

	// A 5th edit, past the cooldown window (toolCount=6, last fired at
	// toolCount=1, diff=5) should be allowed to fire again.
	s.RecordToolCall("Edit", `{"file_path":"/tmp/y.go"}`)
	s.CheckAfterTools(5, false) // toolCount=5, diff=4 — still within cooldown
	s.RecordToolCall("Edit", `{"file_path":"/tmp/z.go"}`)
	r := s.CheckAfterTools(6, false) // toolCount=6, diff=5 — cooldown elapsed
	if len(r) != 1 {
		t.Errorf("expected the group to fire again once cooldown elapsed, got %v", r)
	}
}

func TestToolPatternCrossRuleCooldownDoesNotAffectOtherTypes(t *testing.T) {
	// The cross-rule cooldown is scoped to tool_pattern only — an
	// every_n_tools or after_error rule must still fire on its own schedule
	// even while a tool_pattern rule is on its shared cooldown.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "tp", Trigger: Trigger{Type: "tool_pattern", ToolPattern: "^Edit$"}, Priority: "high"},
			{Text: "periodic", Trigger: Trigger{Type: "every_n_tools", N: 2}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 5, 5) // maxPerBatch=5 so both can fire in one batch
	s.StartTurn("hello")

	s.RecordToolCall("Edit", "")
	r := s.CheckAfterTools(1, false)
	if len(r) != 1 || r[0] != "tp" {
		t.Errorf("tool call 1: expected [tp], got %v", r)
	}

	// Tool call 2: tp is on its own + group cooldown, but periodic (N=2)
	// should still fire independently.
	s.RecordToolCall("Edit", "")
	r = s.CheckAfterTools(2, false)
	if len(r) != 1 || r[0] != "periodic" {
		t.Errorf("tool call 2: expected [periodic] (tp on cooldown), got %v", r)
	}
}

func TestAfterErrorExemptsBenignGrepNoMatch(t *testing.T) {
	// #1309: grep/rg/ack/test/[ finding "no match" (exit 1) sets is_error
	// via the raw shell exit code, indistinguishable from a real failure by
	// exit code alone — after_error must not fire on it.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "check-errors", Trigger: Trigger{Type: "after_error"}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("hello")

	for _, cmd := range []string{
		`grep foo bar.txt`,
		`rg --hidden pattern .`,
		`test -f /tmp/missing`,
		`[ -z "$VAR" ]`,
	} {
		s.StartTurn("hello") // reset per-turn cooldown state between cases
		s.RecordToolCall("Bash", `{"command":"`+cmd+`"}`)
		if r := s.CheckAfterTools(1, true); len(r) != 0 {
			t.Errorf("benign no-match command %q: expected no fire, got %v", cmd, r)
		}
	}
}

func TestAfterErrorStillFiresForRealErrors(t *testing.T) {
	// A genuine failure (not a grep/test-style command) must still fire.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "check-errors", Trigger: Trigger{Type: "after_error"}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("hello")

	s.RecordToolCall("Bash", `{"command":"go build ./..."}`)
	r := s.CheckAfterTools(1, true)
	if len(r) != 1 || r[0] != "check-errors" {
		t.Errorf("real build failure: expected [check-errors], got %v", r)
	}
}

func TestAfterErrorExemptionRequiresRecentContext(t *testing.T) {
	// With no recorded tool call (a caller that doesn't call RecordToolCall),
	// the exemption must default to NOT exempting — preserves prior
	// behaviour for callers without tool context.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "check-errors", Trigger: Trigger{Type: "after_error"}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 1, 1)
	s.StartTurn("hello")

	r := s.CheckAfterTools(1, true)
	if len(r) != 1 || r[0] != "check-errors" {
		t.Errorf("no tool context: expected [check-errors] (fail open), got %v", r)
	}
}

func TestCheckRegexCapsAtMaxPerBatch(t *testing.T) {
	// #1309: a single message matching several independent regex rules
	// previously stacked ALL of them into one turn-start injection. Capped
	// at MaxPerBatch like the post-tool path.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "r1", Trigger: Trigger{Type: "regex", Pattern: "(?i)debug"}, Priority: "high"},
			{Text: "r2", Trigger: Trigger{Type: "regex", Pattern: "(?i)issue"}, Priority: "medium"},
			{Text: "r3", Trigger: Trigger{Type: "regex", Pattern: "(?i)problem"}, Priority: "low"},
		},
	}
	s := NewScheduler(rs, 1, 1) // maxPerBatch=1
	s.StartTurn("debug this issue, there's a problem")

	r := s.CheckRegex()
	if len(r) != 1 {
		t.Errorf("expected exactly 1 regex reminder (maxPerBatch=1), got %d: %v", len(r), r)
	}
}

func TestCheckRegexMaxPerBatchUnlimitedWhenHigh(t *testing.T) {
	// Sanity: raising MaxPerBatch allows more than one regex match through.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "r1", Trigger: Trigger{Type: "regex", Pattern: "(?i)debug"}, Priority: "high"},
			{Text: "r2", Trigger: Trigger{Type: "regex", Pattern: "(?i)issue"}, Priority: "medium"},
		},
	}
	s := NewScheduler(rs, 1, 5) // maxPerBatch=5
	s.StartTurn("debug this issue")

	r := s.CheckRegex()
	if len(r) != 2 {
		t.Errorf("expected both regex reminders with maxPerBatch=5, got %d: %v", len(r), r)
	}
}

func TestMaxPerTurnBudget(t *testing.T) {
	// #1309: a per-turn injection budget bounds the TOTAL nudges across
	// every trigger type in one turn, not just per-batch/per-rule. 0 (the
	// zero value) means unlimited — must not change behaviour by default.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "turn-reminder", Trigger: Trigger{Type: "every_n_turns", N: 1}, Priority: "low"},
			{Text: "regex-hit", Trigger: Trigger{Type: "regex", Pattern: "(?i)debug"}, Priority: "low"},
			{Text: "after-err", Trigger: Trigger{Type: "after_error"}, Priority: "high"},
		},
	}
	// Cooldown=10 (not 1): a regex rule's cooldown check is shared with
	// CheckAfterTools's tool-count-based reevaluation — at cooldown=1 the
	// very next tool call (diff=1) would be eligible to refire the SAME
	// regex rule a second time, which is a separate, pre-existing quirk
	// unrelated to the per-turn budget under test here. A higher cooldown
	// keeps this test isolated to MaxPerTurn.
	s := NewScheduler(rs, 10, 5)
	s.Configure(Settings{Cooldown: 10, MaxPerBatch: 5, MaxPerTurn: 2})
	s.StartTurn("debug this")

	var total []string
	total = append(total, s.CheckTurnInterval()...)
	total = append(total, s.CheckRegex()...)
	s.RecordToolCall("Bash", `{"command":"go build ./..."}`)
	total = append(total, s.CheckAfterTools(1, true)...)

	if len(total) != 2 {
		t.Errorf("expected exactly 2 nudges total (MaxPerTurn=2), got %d: %v", len(total), total)
	}
}

func TestMaxPerTurnZeroMeansUnlimited(t *testing.T) {
	// The zero value (unset config) must not throttle anything — preserves
	// every existing caller's behaviour.
	t.Parallel()

	rs := &RuleSet{
		Rules: []Rule{
			{Text: "turn-reminder", Trigger: Trigger{Type: "every_n_turns", N: 1}, Priority: "low"},
			{Text: "regex-hit", Trigger: Trigger{Type: "regex", Pattern: "(?i)debug"}, Priority: "low"},
			{Text: "after-err", Trigger: Trigger{Type: "after_error"}, Priority: "high"},
		},
	}
	s := NewScheduler(rs, 10, 5) // see TestMaxPerTurnBudget for why cooldown isn't 1
	s.StartTurn("debug this")

	var total []string
	total = append(total, s.CheckTurnInterval()...)
	total = append(total, s.CheckRegex()...)
	s.RecordToolCall("Bash", `{"command":"go build ./..."}`)
	total = append(total, s.CheckAfterTools(1, true)...)

	if len(total) != 3 {
		t.Errorf("expected all 3 nudges with MaxPerTurn unset (0=unlimited), got %d: %v", len(total), total)
	}
}

package nudge

import (
	"regexp"
	"sync"
)

// recentBufferDepth bounds the ring buffer of recent tool events the
// scheduler keeps for tool_pattern evaluation. Long enough to detect
// "Consecutive: 5" patterns; short enough to keep memory tiny. Tuneable
// per Scheduler via NewSchedulerOpts in future if needed.
const recentBufferDepth = 16

// toolEvent records a single tool invocation in the recent-tools ring
// buffer. ToolInput is the raw JSON (truncated at the helper layer);
// scheduler regexes match against it directly without re-parsing.
type toolEvent struct {
	Name  string
	Input string
}

// Scheduler tracks per-turn state and evaluates nudge triggers.
// Create one per agent; call StartTurn() at the start of each turn.
type Scheduler struct {
	rules       []Rule
	cooldown    int // min tool calls between repeating same rule
	maxPerBatch int // max reminders per tool batch

	mu                 sync.Mutex
	lastFired          map[int]int            // rule index → tool call count when last fired
	regexResults       map[int]bool           // rule index → whether regex trigger matches current message
	compiledRegex      map[int]*regexp.Regexp // user-message regex (Trigger.Pattern)
	compiledToolRegex  map[int]*regexp.Regexp // tool-name regex (Trigger.ToolPattern)
	compiledInputRegex map[int]*regexp.Regexp // tool-input regex (Trigger.InputPattern)
	toolCount          int
	turnCount          int // lifetime turn counter (never reset)

	// recent is a ring buffer of the most recent tool events (newest at
	// the highest index). Used by tool_pattern triggers to evaluate
	// consecutive matches. Sized to recentBufferDepth; grows append-only
	// up to that bound, then overwrites oldest entries.
	recent []toolEvent
}

// NewScheduler creates a Scheduler from a RuleSet.
// cooldown is the minimum tool calls between repeating the same reminder.
// maxPerBatch is the maximum reminders injected per tool batch.
func NewScheduler(rs *RuleSet, cooldown, maxPerBatch int) *Scheduler {
	if rs == nil {
		return nil
	}
	if cooldown <= 0 {
		cooldown = 5
	}
	if maxPerBatch <= 0 {
		maxPerBatch = 1
	}
	s := &Scheduler{
		rules:              rs.Rules,
		cooldown:           cooldown,
		maxPerBatch:        maxPerBatch,
		lastFired:          make(map[int]int),
		regexResults:       make(map[int]bool),
		compiledRegex:      make(map[int]*regexp.Regexp),
		compiledToolRegex:  make(map[int]*regexp.Regexp),
		compiledInputRegex: make(map[int]*regexp.Regexp),
	}
	// Pre-compile regex patterns. Compile failures degrade gracefully:
	// the rule retains its trigger config but the missing compiled regex
	// causes shouldFire's tool_pattern branch to treat the unmatched
	// pattern as "no match" — the rule simply never fires.
	for i, r := range s.rules {
		if r.Trigger.Type == "regex" && r.Trigger.Pattern != "" {
			if re, err := regexp.Compile(r.Trigger.Pattern); err == nil {
				s.compiledRegex[i] = re
			}
		}
		if r.Trigger.Type == "tool_pattern" {
			if r.Trigger.ToolPattern != "" {
				if re, err := regexp.Compile(r.Trigger.ToolPattern); err == nil {
					s.compiledToolRegex[i] = re
				}
			}
			if r.Trigger.InputPattern != "" {
				if re, err := regexp.Compile(r.Trigger.InputPattern); err == nil {
					s.compiledInputRegex[i] = re
				}
			}
		}
	}
	return s
}

// StartTurn clears per-turn state and evaluates regex triggers against the
// user message. Call at the start of each agent turn.
func (s *Scheduler) StartTurn(userMessage string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnCount++
	s.toolCount = 0
	s.lastFired = make(map[int]int)
	s.regexResults = make(map[int]bool)
	// Clear the recent-tools buffer between turns so tool_pattern
	// triggers evaluate "consecutive tools within this turn" rather than
	// matching across turn boundaries (which would conflate work blocks).
	s.recent = s.recent[:0]
	for i, re := range s.compiledRegex {
		s.regexResults[i] = re.MatchString(userMessage)
	}
}

// CheckTurnInterval evaluates every_n_turns rules against the lifetime turn
// counter. Returns reminder texts for rules whose turn interval has elapsed.
// Called once per turn, after StartTurn().
func (s *Scheduler) CheckTurnInterval() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []string
	for _, r := range s.rules {
		if r.Trigger.Type != "every_n_turns" {
			continue
		}
		n := r.Trigger.N
		if n <= 0 {
			n = 50
		}
		if s.turnCount > 0 && s.turnCount%n == 0 {
			if r.Condition != nil && !r.Condition() {
				continue
			}
			result = append(result, r.Text)
		}
	}
	return result
}

// CheckAfterTools evaluates triggers after a tool batch and returns up to
// maxPerBatch reminder texts. Returns nil if nothing fires.
//
// toolCount is the cumulative number of individual tool calls executed so far
// this turn. lastToolError indicates the most recent tool call returned an
// error.
//
// Callers that want tool_pattern triggers to evaluate must call RecordToolCall
// (one per tool) before CheckAfterTools so the recent-tools ring buffer is
// up to date. The delegated/CC transport does this per tool; the API
// transport does it per tool_use block within a batch.
func (s *Scheduler) CheckAfterTools(toolCount int, lastToolError bool) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolCount = toolCount

	var result []string
	fired := 0
	for i, r := range s.rules {
		if fired >= s.maxPerBatch {
			break
		}
		if !s.shouldFire(i, r, lastToolError) {
			continue
		}
		s.lastFired[i] = s.toolCount
		result = append(result, r.Text)
		fired++
	}
	return result
}

// RecordToolCall appends a tool invocation to the recent-tools ring buffer
// used by tool_pattern triggers. Cheap; safe to call with empty strings
// when the caller has no tool context.
func (s *Scheduler) RecordToolCall(name, input string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := toolEvent{Name: name, Input: input}
	if len(s.recent) < recentBufferDepth {
		s.recent = append(s.recent, ev)
		return
	}
	// Shift-and-overwrite. Buffer depth is small (16) so copy cost is
	// negligible vs maintaining a head index.
	copy(s.recent, s.recent[1:])
	s.recent[len(s.recent)-1] = ev
}

// CheckPreAnswer returns the text of all pre_answer rules (joined), or "".
// Called when the model wants to end the turn.
func (s *Scheduler) CheckPreAnswer() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var result string
	for _, r := range s.rules {
		if r.Trigger.Type != "pre_answer" {
			continue
		}
		if result != "" {
			result += "\n"
		}
		result += r.Text
	}
	return result
}

// CheckRegex returns the text of regex rules that matched the user message
// but haven't fired yet. Ensures regex triggers fire even on turns without
// tool calls, where CheckAfterTools is never reached.
func (s *Scheduler) CheckRegex() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []string
	for i, r := range s.rules {
		if r.Trigger.Type != "regex" {
			continue
		}
		if !s.regexResults[i] {
			continue
		}
		if _, fired := s.lastFired[i]; fired {
			continue
		}
		s.lastFired[i] = s.toolCount
		result = append(result, r.Text)
	}
	return result
}

// HasPreAnswerRules returns true if any rules have a pre_answer trigger.
func (s *Scheduler) HasPreAnswerRules() bool {
	if s == nil {
		return false
	}
	for _, r := range s.rules {
		if r.Trigger.Type == "pre_answer" {
			return true
		}
	}
	return false
}

// shouldFire checks if rule i should fire right now. Caller must hold s.mu.
func (s *Scheduler) shouldFire(i int, r Rule, lastToolError bool) bool {
	// Cooldown check
	if last, ok := s.lastFired[i]; ok {
		if s.toolCount-last < s.cooldown {
			return false
		}
	}
	// Runtime condition check
	if r.Condition != nil && !r.Condition() {
		return false
	}

	switch r.Trigger.Type {
	case "every_n_tools":
		n := r.Trigger.N
		if n <= 0 {
			n = 5
		}
		return s.toolCount > 0 && s.toolCount%n == 0

	case "after_error":
		return lastToolError

	case "regex":
		return s.regexResults[i]

	case "pre_answer":
		return false // handled separately by CheckPreAnswer

	case "tool_pattern":
		return s.matchesRecentLocked(i, r.Trigger)

	default:
		return false
	}
}

// matchesRecentLocked evaluates a tool_pattern trigger against the recent
// ring buffer. Caller must hold s.mu.
//
// Semantics: require Consecutive (default 1) most-recent events to all
// match both ToolPattern and InputPattern. An empty pattern is treated
// as "no constraint" on that field. A pattern that failed to compile
// (no entry in compiledToolRegex / compiledInputRegex) counts as never
// matching — graceful degradation for malformed rules.
func (s *Scheduler) matchesRecentLocked(idx int, t Trigger) bool {
	n := t.Consecutive
	if n <= 0 {
		n = 1
	}
	if len(s.recent) < n {
		return false
	}
	// Walk the last n events (newest is recent[len-1]).
	for k := len(s.recent) - n; k < len(s.recent); k++ {
		ev := s.recent[k]
		if t.ToolPattern != "" {
			re, ok := s.compiledToolRegex[idx]
			if !ok || !re.MatchString(ev.Name) {
				return false
			}
		}
		if t.InputPattern != "" {
			re, ok := s.compiledInputRegex[idx]
			if !ok || !re.MatchString(ev.Input) {
				return false
			}
		}
	}
	return true
}

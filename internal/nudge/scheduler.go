package nudge

import (
	"regexp"
	"sync"
)

// Scheduler tracks per-turn state and evaluates nudge triggers.
// Create one per agent; call StartTurn() at the start of each turn.
type Scheduler struct {
	rules       []Rule
	cooldown    int // min tool calls between repeating same rule
	maxPerBatch int // max reminders per tool batch

	mu            sync.Mutex
	lastFired     map[int]int    // rule index → tool call count when last fired
	matchResults  map[int]bool   // rule index → whether match trigger matches current message
	compiledMatch map[int]*regexp.Regexp
	toolCount     int
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
		rules:         rs.Rules,
		cooldown:      cooldown,
		maxPerBatch:   maxPerBatch,
		lastFired:     make(map[int]int),
		matchResults:  make(map[int]bool),
		compiledMatch: make(map[int]*regexp.Regexp),
	}
	// Pre-compile match regexes
	for i, r := range s.rules {
		if r.Trigger.Type == "match" && r.Trigger.Pattern != "" {
			if re, err := regexp.Compile(r.Trigger.Pattern); err == nil {
				s.compiledMatch[i] = re
			}
		}
	}
	return s
}

// StartTurn clears per-turn state and evaluates match triggers against the
// user message. Call at the start of each agent turn.
func (s *Scheduler) StartTurn(userMessage string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolCount = 0
	s.lastFired = make(map[int]int)
	s.matchResults = make(map[int]bool)
	for i, re := range s.compiledMatch {
		s.matchResults[i] = re.MatchString(userMessage)
	}
}

// CheckAfterTools evaluates triggers after a tool batch and returns up to
// maxPerBatch reminder texts. Returns nil if nothing fires.
//
// toolCallIndex is the loop counter (0-based), sameToolStreak is the count of
// consecutive calls to the same tool, and lastToolError indicates the most
// recent tool call returned an error.
func (s *Scheduler) CheckAfterTools(toolCallIndex, sameToolStreak int, lastToolError bool) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolCount = toolCallIndex + 1

	var result []string
	fired := 0
	for i, r := range s.rules {
		if fired >= s.maxPerBatch {
			break
		}
		if !s.shouldFire(i, r, lastToolError, sameToolStreak) {
			continue
		}
		s.lastFired[i] = s.toolCount
		result = append(result, r.Text)
		fired++
	}
	return result
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

// CheckMatch returns the text of match rules that matched the user message
// but haven't fired yet. Ensures match triggers fire even on turns without
// tool calls, where CheckAfterTools is never reached.
func (s *Scheduler) CheckMatch() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []string
	for i, r := range s.rules {
		if r.Trigger.Type != "match" {
			continue
		}
		if !s.matchResults[i] {
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
func (s *Scheduler) shouldFire(i int, r Rule, lastToolError bool, sameToolStreak int) bool {
	// Cooldown check
	if last, ok := s.lastFired[i]; ok {
		if s.toolCount-last < s.cooldown {
			return false
		}
	}

	switch r.Trigger.Type {
	case "periodic":
		n := r.Trigger.N
		if n <= 0 {
			n = 5
		}
		return s.toolCount > 0 && s.toolCount%n == 0

	case "after_streak":
		n := r.Trigger.N
		if n <= 0 {
			n = 3
		}
		return sameToolStreak >= n

	case "after_error":
		return lastToolError

	case "match":
		return s.matchResults[i]

	case "pre_answer":
		return false // handled separately by CheckPreAnswer

	default:
		return false
	}
}

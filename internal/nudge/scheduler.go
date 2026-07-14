package nudge

import (
	"foci/internal/log"
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

// Settings holds the live-tunable nudge config (a config-free mirror of the
// relevant ResolvedNudge fields, so this package keeps no config import). All
// rules are built once; firing is gated on these values, read live under s.mu,
// so a [defaults.nudge] edit applies without rebuilding the scheduler or losing
// its per-session counters. Configure swaps the whole struct atomically.
type Settings struct {
	Cooldown           int    // min tool calls between repeating the same rule
	MaxPerBatch        int    // max reminders per tool batch
	Enable             bool   // fire "char" (character-derived) rules
	DefaultEnable      bool   // fire "default" + "scratchpad" rules
	DefaultFreq        int    // every_n_turns for "default" rules
	ScratchpadFreq     int    // every_n_turns for the "scratchpad" rule (0 = off)
	BraindeadThreshold int    // every_n_tools for the "braindead" rule (0 = off)
	BraindeadPrompt    string // text for the "braindead" rule ("" = built-in)
	PreAnswerGate      bool   // enable the pre-answer verification gate
	PreAnswerMinTools  int    // min tool calls before the gate fires
}

// Scheduler tracks per-turn state and evaluates nudge triggers.
// Create one per agent; call StartTurn() at the start of each turn.
type Scheduler struct {
	rules []Rule

	mu                 sync.Mutex
	settings           Settings // live-tunable config, guarded by mu (Configure swaps)
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

// SchedulerOpts configures optional Scheduler behaviour.
type SchedulerOpts struct {
	Cooldown       int
	MaxPerBatch    int
	// CanPostTool gates every_n_tools, after_error, and tool_pattern
	// triggers. Rules requiring this that are present in the RuleSet but
	// unsupported get a warning and are silently skipped at evaluation.
	CanPostTool    bool
	// CanPreAnswer gates pre_answer triggers.
	CanPreAnswer   bool
}

// NewScheduler creates a Scheduler from a RuleSet.
// cooldown is the minimum tool calls between repeating the same reminder.
// maxPerBatch is the maximum reminders injected per tool batch.
func NewScheduler(rs *RuleSet, cooldown, maxPerBatch int) *Scheduler {
	return NewSchedulerOpts(rs, SchedulerOpts{Cooldown: cooldown, MaxPerBatch: maxPerBatch, CanPostTool: true, CanPreAnswer: true})
}

// NewSchedulerOpts creates a Scheduler with full options including
// backend capability gating.
func NewSchedulerOpts(rs *RuleSet, opts SchedulerOpts) *Scheduler {
	if rs == nil {
		return nil
	}
	cooldown := opts.Cooldown
	if cooldown <= 0 {
		cooldown = 5
	}
	maxPerBatch := opts.MaxPerBatch
	if maxPerBatch <= 0 {
		maxPerBatch = 1
	}
	canPostTool := opts.CanPostTool
	canPreAnswer := opts.CanPreAnswer

	var activeRules []Rule
	for _, r := range rs.Rules {
		if TriggerRequiresPostTool(r.Trigger.Type) && !canPostTool {
			log.Warnf("nudge", "rule %q uses trigger %q which requires post-tool injection — not supported by this backend. Rule will be skipped.", truncate(r.Text, 60), r.Trigger.Type)
			continue
		}
		if TriggerRequiresPreAnswer(r.Trigger.Type) && !canPreAnswer {
			log.Warnf("nudge", "rule %q uses trigger %q which requires pre-answer injection — not supported by this backend. Rule will be skipped.", truncate(r.Text, 60), r.Trigger.Type)
			continue
		}
		activeRules = append(activeRules, r)
	}

	s := &Scheduler{
		rules:              activeRules,
		settings:           Settings{Cooldown: cooldown, MaxPerBatch: maxPerBatch},
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

// Configure atomically swaps the live-tunable settings. Called at setup and by
// the gateway's live-apply on a [defaults.nudge] edit — no rebuild, so the
// per-session counters (turnCount, lastFired) survive the change.
func (s *Scheduler) Configure(cfg Settings) {
	if s == nil {
		return
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 5
	}
	if cfg.MaxPerBatch <= 0 {
		cfg.MaxPerBatch = 1
	}
	s.mu.Lock()
	s.settings = cfg
	s.mu.Unlock()
}

// PreAnswerGate reports whether the pre-answer verification gate is enabled.
func (s *Scheduler) PreAnswerGate() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings.PreAnswerGate
}

// PreAnswerMinTools is the min tool calls before the pre-answer gate fires.
func (s *Scheduler) PreAnswerMinTools() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings.PreAnswerMinTools
}

// categoryEnabled reports whether rules of the given category may fire per live
// settings. Untagged rules (cat == "", e.g. tests) are always active — only the
// built-in categories are config-gated. Caller must hold s.mu.
func (s *Scheduler) categoryEnabled(cat string) bool {
	switch cat {
	case CategoryChar:
		return s.settings.Enable
	case CategoryDefault:
		return s.settings.DefaultEnable
	case CategoryScratchpad:
		return s.settings.DefaultEnable && s.settings.ScratchpadFreq > 0
	case CategoryBraindead:
		return s.settings.BraindeadThreshold > 0
	default:
		return true
	}
}

// effectiveN returns the live interval for a rule: config-driven for the
// built-in categories, else the rule's baked Trigger.N. Caller holds s.mu.
func (s *Scheduler) effectiveN(r Rule) int {
	switch r.Category {
	case CategoryDefault:
		return s.settings.DefaultFreq
	case CategoryScratchpad:
		return s.settings.ScratchpadFreq
	case CategoryBraindead:
		return s.settings.BraindeadThreshold
	default:
		return r.Trigger.N
	}
}

// effectiveText returns the live text for a rule: the configured braindead
// prompt overrides the built-in default, else the rule's own text. Holds s.mu.
func (s *Scheduler) effectiveText(r Rule) string {
	if r.Category == CategoryBraindead && s.settings.BraindeadPrompt != "" {
		return s.settings.BraindeadPrompt
	}
	return r.Text
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
		if !s.categoryEnabled(r.Category) {
			continue
		}
		n := s.effectiveN(r)
		if n <= 0 {
			n = 50
		}
		if s.turnCount > 0 && s.turnCount%n == 0 {
			if r.Condition != nil && !r.Condition() {
				continue
			}
			result = append(result, s.effectiveText(r))
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
		if fired >= s.settings.MaxPerBatch {
			break
		}
		if !s.shouldFire(i, r, lastToolError) {
			continue
		}
		s.lastFired[i] = s.toolCount
		result = append(result, s.effectiveText(r))
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
		if !s.categoryEnabled(r.Category) {
			continue
		}
		if result != "" {
			result += "\n"
		}
		result += s.effectiveText(r)
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
		if !s.categoryEnabled(r.Category) {
			continue
		}
		if !s.regexResults[i] {
			continue
		}
		if _, fired := s.lastFired[i]; fired {
			continue
		}
		s.lastFired[i] = s.toolCount
		result = append(result, s.effectiveText(r))
	}
	return result
}

// HasPreAnswerRules returns true if any rules have a pre_answer trigger.
func (s *Scheduler) HasPreAnswerRules() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rules {
		if r.Trigger.Type == "pre_answer" && s.categoryEnabled(r.Category) {
			return true
		}
	}
	return false
}

// shouldFire checks if rule i should fire right now. Caller must hold s.mu.
func (s *Scheduler) shouldFire(i int, r Rule, lastToolError bool) bool {
	if !s.categoryEnabled(r.Category) {
		return false
	}
	// Cooldown check
	if last, ok := s.lastFired[i]; ok {
		if s.toolCount-last < s.settings.Cooldown {
			return false
		}
	}
	// Runtime condition check
	if r.Condition != nil && !r.Condition() {
		return false
	}

	switch r.Trigger.Type {
	case "every_n_tools":
		n := s.effectiveN(r)
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

package nudge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/agent/turnevent"
	"foci/internal/log"
	"foci/internal/platform"
)

// buildExtractionPrompt constructs the extraction prompt, including only
// trigger types the backend can actually evaluate. This prevents the model
// from wasting extraction budget on rules the scheduler will silently skip.
func (e *Extractor) buildExtractionPrompt() string {
	var b strings.Builder

	b.WriteString(`Your character files are loaded in the system prompt. Read through them and identify
statements that look like rules — things you should or shouldn't do, patterns to watch
for, failure modes to avoid.

For each rule, consider: is this something that would come up during a normal
assistant turn (responding to messages, using tools, investigating questions)?
Skip rules that only apply in special contexts (e.g. memory file maintenance,
compaction, session handoffs, background processes) unless they also apply generally.

For each rule you extract, output a JSON object:
{
  "text": "Terse imperative reminder, max 50 words — written to yourself as a nudge",
  "source_file": "CRAFT.md",
  "source_text": "The original passage this is derived from",
  "trigger": <see trigger types below>,
  "priority": "high|medium|low"
}

`)

	b.WriteString("Trigger types (pick the most appropriate):\n")

	// regex works on all backends (turn-start, no injection needed).
	b.WriteString(`- {"type": "regex", "pattern": "regex"} — remind when the user's message matches this pattern
`)

	// Post-tool triggers: every_n_tools, after_error, tool_pattern.
	if e.canPostTool {
		b.WriteString(`- {"type": "every_n_tools", "n": N} — remind every N individual tool calls during a turn
- {"type": "after_error"} — remind when a tool call returns an error
- {"type": "tool_pattern", "tool_pattern": "regex", "input_pattern": "regex", "consecutive": N}
  — remind when the most recent tool calls match. tool_pattern matches the
  tool name (e.g. "^Read$", "^(Read|Grep|Glob)$"); input_pattern matches the
  raw tool_input JSON (e.g. "rm -rf", "/character/[^/]+\\.md"); consecutive
  defaults to 1. Both pattern fields are optional — omitting one means "any".
  Prefer tool_pattern over a high-N every_n_tools when the rule is really
  about a specific kind of work (reading without engaging, editing character
  files, running destructive bash). Common tool names: Read, Write, Edit,
  Bash, Grep, Glob, Task, WebFetch, WebSearch, TodoWrite.
`)
	}

	// Pre-answer trigger.
	if e.canPreAnswer {
		b.WriteString(`- {"type": "pre_answer"} — remind just before returning a final answer to the user
`)
	}

	b.WriteString("\n")

	if e.canPostTool {
		b.WriteString(`Use your judgment on trigger type and frequency. For every_n_tools rules, keep N high —
every 15 tool calls is already quite frequent. Only the most critical rules should
fire that often; most should use N=25 or higher. Rules about edge cases can have
even higher N or more specific triggers. tool_pattern is usually a better fit
than every_n_tools when the rule has a clear "what kind of tool use" signal.

`)
	}

	// Per-type limits — only mention available constrained types.
	if e.canPostTool || e.canPreAnswer {
		b.WriteString("Limit: return at most ONE rule")
		var constrained []string
		if e.canPreAnswer {
			constrained = append(constrained, `"pre_answer"`)
		}
		if e.canPostTool {
			constrained = append(constrained, `"after_error"`)
		}
		switch len(constrained) {
		case 1:
			b.WriteString(" for " + constrained[0] + ".\n")
		case 2:
			b.WriteString(" for " + constrained[0] + " and at most ONE rule for " + constrained[1] + ".\n")
		}
		b.WriteString(`If multiple rules would use the same trigger type, synthesize them into a single
combined nudge that covers all the key points. Keep the combined text under 50 words.

`)
	}

	b.WriteString(`For "regex" triggers: the regex is tested against the user's message with
re.MatchString (substring match, not full-string match). Be careful that what
you write will actually do what you intend:
- Use \b word boundaries to avoid matching substrings (e.g. \bcc\b not cc)
- Avoid overly broad patterns that fire on routine messages
- Test mentally: what common messages would this match? Would the nudge be useful there?
- If a rule can't be meaningfully scoped by regex, use a different trigger type

Return a JSON array. If no extractable rules exist, return [].

Respond with ONLY the JSON array. No explanation, no preamble, no markdown formatting.`)

	return b.String()
}

// Extractor handles LLM-based rule extraction from character files.
type Extractor struct {
	agentID      string
	workspaceDir string
	fileOrder    []string
	fileMode     os.FileMode
	canPostTool  bool
	canPreAnswer bool
}

// NewExtractor creates an Extractor for the given workspace.
// canPostTool and canPreAnswer mirror SchedulerOpts capabilities: the
// extraction prompt only lists trigger types the backend can evaluate,
// so the model doesn't produce rules the scheduler will discard.
func NewExtractor(agentID, workspaceDir string, fileOrder []string, fileMode os.FileMode, canPostTool, canPreAnswer bool) *Extractor {
	return &Extractor{
		agentID:      agentID,
		workspaceDir: workspaceDir,
		fileOrder:    fileOrder,
		fileMode:     fileMode,
		canPostTool:  canPostTool,
		canPreAnswer: canPreAnswer,
	}
}

// logger returns an agent-scoped logger so extraction log lines (fanned to
// every agent's session when they warn) are attributable via the component
// prefix ([nudge:<agentID>]).
func (e *Extractor) logger() *log.ComponentLogger {
	if e.agentID == "" {
		return log.NewComponentLogger("nudge")
	}
	return log.NewComponentLogger("nudge:" + e.agentID)
}

// BranchHandler is the interface for running extraction via a branch session.
// This matches the agent's HandleMessage signature.
type BranchHandler interface {
	HandleMessage(ctx context.Context, sessionKey string, texts []string, attachments []platform.Attachment) error
}

// OneShotRunner executes a one-shot prompt and returns the response.
// DelegatedManager implements this via claude --print.
type OneShotRunner interface {
	RunOnce(ctx context.Context, prompt string, systemPrompt string) (string, error)
}

// NeedsExtraction checks if character files have changed since the last extraction.
// Returns the current content hash and true if extraction is needed.
func (e *Extractor) NeedsExtraction() (string, bool) {
	contents := e.readCharacterFiles()
	if len(contents) == 0 {
		return "", false
	}
	hash := ContentHash(contents)

	rulesPath := RulesPath(e.workspaceDir)
	existing, err := LoadRules(rulesPath)
	if err != nil {
		e.logger().Warnf("loading existing rules: %v", err)
		return hash, true
	}
	if existing == nil {
		return hash, true
	}
	if existing.ContentHash != hash {
		return hash, true
	}
	// Even when character files are unchanged, force re-extraction if the
	// stored rules contain trigger types this backend can't evaluate — e.g.
	// after a backend switch (claude-code → opencode) that dropped post-tool
	// or pre-answer injection. Re-extraction regenerates the file offering
	// only supported trigger types, so the scheduler stops skip-warning the
	// dead rules on every load. Self-heals without a character-file edit.
	if e.hasUnsupportedTriggers(existing.Rules) {
		e.logger().Infof("existing rules use triggers unsupported by this backend — forcing re-extraction")
		return hash, true
	}
	return hash, false
}

// hasUnsupportedTriggers reports whether any stored rule uses a trigger type
// the current backend can't evaluate, given this Extractor's capabilities.
// Mirrors the gating the scheduler applies (TriggerRequiresPostTool /
// TriggerRequiresPreAnswer), so a rule that would be silently skipped at
// load time instead triggers a regeneration.
func (e *Extractor) hasUnsupportedTriggers(rules []Rule) bool {
	for _, r := range rules {
		if TriggerRequiresPostTool(r.Trigger.Type) && !e.canPostTool {
			return true
		}
		if TriggerRequiresPreAnswer(r.Trigger.Type) && !e.canPreAnswer {
			return true
		}
	}
	return false
}

// Extract runs rule extraction via a branch session and saves the results.
// The handler should be a branch session that inherits the agent's system prompt.
func (e *Extractor) Extract(ctx context.Context, handler BranchHandler, sessionKey string) error {
	hash, needed := e.NeedsExtraction()
	if !needed {
		e.logger().Infof("character files unchanged, skipping extraction")
		return nil
	}

	e.logger().Infof("extracting nudge rules (hash=%s)", hash[:16])

	buf := turnevent.NewBufferSink()
	extractCtx := turnevent.WithSink(ctx, buf)
	if err := handler.HandleMessage(extractCtx, sessionKey, []string{e.buildExtractionPrompt()}, nil); err != nil {
		return fmt.Errorf("nudge extraction: %w", err)
	}

	rules, err := ParseExtractionResponse(buf.FinalText())
	if err != nil {
		return fmt.Errorf("parse nudge extraction: %w", err)
	}

	rs := &RuleSet{
		ContentHash: hash,
		Rules:       rules,
	}
	rulesPath := RulesPath(e.workspaceDir)
	if err := SaveRules(rulesPath, rs, e.fileMode); err != nil {
		return fmt.Errorf("save nudge rules: %w", err)
	}

	e.logger().Infof("extracted %d nudge rules → %s", len(rules), rulesPath)
	return nil
}

// ExtractViaRunOnce runs rule extraction using a one-shot runner (claude --print).
// This is the backend-agent path: no interactive session, no platform delivery.
func (e *Extractor) ExtractViaRunOnce(ctx context.Context, runner OneShotRunner) error {
	hash, needed := e.NeedsExtraction()
	if !needed {
		e.logger().Infof("character files unchanged, skipping extraction")
		return nil
	}

	e.logger().Infof("extracting nudge rules via RunOnce (hash=%s)", hash[:16])

	response, err := runner.RunOnce(ctx, e.buildExtractionPrompt(), "")
	if err != nil {
		return fmt.Errorf("nudge extraction (RunOnce): %w", err)
	}

	rules, err := ParseExtractionResponse(response)
	if err != nil {
		return fmt.Errorf("parse nudge extraction: %w", err)
	}

	rs := &RuleSet{
		ContentHash: hash,
		Rules:       rules,
	}
	rulesPath := RulesPath(e.workspaceDir)
	if err := SaveRules(rulesPath, rs, e.fileMode); err != nil {
		return fmt.Errorf("save nudge rules: %w", err)
	}

	e.logger().Infof("extracted %d nudge rules → %s", len(rules), rulesPath)
	return nil
}

// ParseExtractionResponse parses the LLM response into rules.
// Handles raw JSON arrays, JSON in markdown code blocks, and JSON preceded
// by arbitrary preamble text. Returns empty rules (no error) for empty or
// truncated responses.
func ParseExtractionResponse(response string) ([]Rule, error) {
	text := strings.TrimSpace(response)

	// Empty response — model hit max_tokens or returned nothing.
	if text == "" {
		nudgeLog.Warnf("empty extraction response, returning no rules")
		return nil, nil
	}

	// Strip markdown code fences if present (possibly after preamble text).
	if start := strings.Index(text, "```"); start >= 0 {
		inner := text[start:]
		lines := strings.Split(inner, "\n")
		if len(lines) >= 2 {
			fenceStart := 1
			fenceEnd := len(lines)
			for fenceEnd > fenceStart && strings.TrimSpace(lines[fenceEnd-1]) == "```" {
				fenceEnd--
				break
			}
			text = strings.Join(lines[fenceStart:fenceEnd], "\n")
		}
	}

	// Find JSON array boundaries — handles preamble text before the array.
	text = strings.TrimSpace(text)
	openIdx := strings.Index(text, "[")
	if openIdx < 0 {
		nudgeLog.Warnf("no JSON array found in extraction response (%.200s)", text)
		return nil, nil
	}
	closeIdx := strings.LastIndex(text, "]")
	if closeIdx < 0 || closeIdx < openIdx {
		// Opening bracket but no closing — truncated JSON.
		nudgeLog.Warnf("truncated JSON array in extraction response, returning no rules")
		return nil, nil
	}
	text = text[openIdx : closeIdx+1]

	var rules []Rule
	if err := json.Unmarshal([]byte(text), &rules); err != nil {
		return nil, fmt.Errorf("unmarshal rules: %w (response: %.200s)", err, text)
	}
	return rules, nil
}

// readCharacterFiles reads the workspace character files in order.
func (e *Extractor) readCharacterFiles() []string {
	var contents []string
	for _, name := range e.fileOrder {
		data, err := os.ReadFile(filepath.Join(e.workspaceDir, name))
		if err != nil {
			continue // skip missing files
		}
		content := string(data)
		if content == "" {
			continue
		}
		contents = append(contents, content)
	}
	return contents
}

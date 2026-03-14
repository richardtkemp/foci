package nudge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/log"
)

// ExtractionPrompt is sent to the model to extract rules from character files.
const ExtractionPrompt = `Your character files are loaded in the system prompt. Read through them and identify
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

Trigger types (pick the most appropriate):
- {"type": "periodic", "n": N} — remind every N tool calls during a turn
- {"type": "pre_answer"} — remind just before returning a final answer to the user
- {"type": "after_streak", "n": N} — remind after N consecutive calls to the same tool
- {"type": "after_error"} — remind when a tool call returns an error
- {"type": "match", "pattern": "regex"} — remind when the user's message matches this pattern

Use your judgment on trigger type and frequency. For periodic rules, keep N high —
every 15 tool calls is already quite frequent. Only the most critical rules should
fire that often; most should use N=25 or higher. Rules about edge cases can have
even higher N or more specific triggers.

Limit: return at most ONE rule for "pre_answer" and at most ONE rule for "after_error".
If multiple rules would use the same trigger type, synthesize them into a single
combined nudge that covers all the key points. Keep the combined text under 50 words.

For "match" triggers: the regex is tested against the user's message with
re.MatchString (substring match, not full-string match). Be careful that what
you write will actually do what you intend:
- Use \b word boundaries to avoid matching substrings (e.g. \bcc\b not cc)
- Avoid overly broad patterns that fire on routine messages
- Test mentally: what common messages would this match? Would the nudge be useful there?
- If a rule can't be meaningfully scoped by regex, use a different trigger type

Return a JSON array. If no extractable rules exist, return [].`

// Extractor handles LLM-based rule extraction from character files.
type Extractor struct {
	workspaceDir string
	fileOrder    []string
}

// NewExtractor creates an Extractor for the given workspace.
func NewExtractor(workspaceDir string, fileOrder []string) *Extractor {
	return &Extractor{
		workspaceDir: workspaceDir,
		fileOrder:    fileOrder,
	}
}

// BranchHandler is the interface for running extraction via a branch session.
// This matches the agent's HandleMessage signature.
type BranchHandler interface {
	HandleMessage(ctx context.Context, sessionKey string, userMessage string) (string, error)
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
		log.Warnf("nudge", "loading existing rules: %v", err)
		return hash, true
	}
	if existing == nil {
		return hash, true
	}
	return hash, existing.ContentHash != hash
}

// Extract runs rule extraction via a branch session and saves the results.
// The handler should be a branch session that inherits the agent's system prompt.
func (e *Extractor) Extract(ctx context.Context, handler BranchHandler, sessionKey string) error {
	hash, needed := e.NeedsExtraction()
	if !needed {
		log.Infof("nudge", "character files unchanged, skipping extraction")
		return nil
	}

	log.Infof("nudge", "extracting nudge rules (hash=%s)", hash[:16])

	response, err := handler.HandleMessage(ctx, sessionKey, ExtractionPrompt)
	if err != nil {
		return fmt.Errorf("nudge extraction: %w", err)
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
	if err := SaveRules(rulesPath, rs); err != nil {
		return fmt.Errorf("save nudge rules: %w", err)
	}

	log.Infof("nudge", "extracted %d nudge rules → %s", len(rules), rulesPath)
	return nil
}

// ParseExtractionResponse parses the LLM response into rules.
// Handles both raw JSON arrays and JSON embedded in markdown code blocks.
func ParseExtractionResponse(response string) ([]Rule, error) {
	text := strings.TrimSpace(response)

	// Strip markdown code fences if present
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		// Remove first line (```json or ```) and last line (```)
		if len(lines) >= 2 {
			start := 1
			end := len(lines)
			for end > start && strings.TrimSpace(lines[end-1]) == "```" {
				end--
				break
			}
			text = strings.Join(lines[start:end], "\n")
		}
	}

	text = strings.TrimSpace(text)
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

package nudge

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RuleSet is the top-level structure stored in nudge-rules.json.
type RuleSet struct {
	ContentHash string `json:"content_hash"` // hash of character files at extraction time
	Rules       []Rule `json:"rules"`
}

// Rule is a single behavioral reminder extracted from character files.
type Rule struct {
	Text       string  `json:"text"`        // terse imperative reminder
	SourceFile string  `json:"source_file"` // which character file
	SourceText string  `json:"source_text"` // original passage
	Trigger    Trigger `json:"trigger"`
	Priority   string  `json:"priority"` // "high", "medium", "low"

	// Condition is an optional runtime predicate. If set, the rule only fires
	// when Condition returns true. Used by built-in rules that depend on agent
	// state (e.g. scratchpad non-empty). Not serialized to JSON.
	Condition func() bool `json:"-"`
}

// Trigger describes when a rule should fire.
//
// Trigger types:
//   - "every_n_tools" (uses N)        — every N tool calls during a turn
//   - "every_n_turns" (uses N)        — every N turns (lifetime counter)
//   - "after_error"                   — when the last tool returned an error
//   - "regex" (uses Pattern)          — when user message matches a regex
//   - "pre_answer"                    — before returning a final answer
//   - "tool_pattern" (uses ToolPattern / InputPattern / Consecutive) —
//     when recent tool invocations match a tool-name regex and/or a
//     tool-input regex. Defaults to "any tool" / "any input" when the
//     corresponding pattern is empty. Consecutive (default 1) requires
//     that many tail events to all match.
type Trigger struct {
	Type    string `json:"type"`
	N       int    `json:"n,omitempty"`       // parameter for every_n_tools/every_n_turns
	Pattern string `json:"pattern,omitempty"` // regex pattern for regex trigger

	// Tool-context fields (used when Type == "tool_pattern").
	ToolPattern  string `json:"tool_pattern,omitempty"`  // regex against the tool name
	InputPattern string `json:"input_pattern,omitempty"` // regex against the raw tool_input JSON
	Consecutive  int    `json:"consecutive,omitempty"`   // require N consecutive matches in the recent buffer (default 1)
}

const rulesFileName = "nudge-rules.json"

// RulesPath returns the path to the nudge rules file.
// Uses {workspace}/character/nudge-rules.json if the character dir exists,
// otherwise {workspace}/nudge-rules.json.
func RulesPath(workspaceDir string) string {
	charDir := filepath.Join(workspaceDir, "character")
	if info, err := os.Stat(charDir); err == nil && info.IsDir() {
		return filepath.Join(charDir, rulesFileName)
	}
	return filepath.Join(workspaceDir, rulesFileName)
}

// LoadRules reads a RuleSet from the given path.
// Returns nil and no error if the file does not exist.
func LoadRules(path string) (*RuleSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read nudge rules: %w", err)
	}
	var rs RuleSet
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, fmt.Errorf("parse nudge rules: %w", err)
	}
	return &rs, nil
}

// SaveRules writes a RuleSet to the given path as indented JSON.
func SaveRules(path string, rs *RuleSet, mode os.FileMode) error {
	data, err := json.MarshalIndent(rs, "", "\t")
	if err != nil {
		return fmt.Errorf("marshal nudge rules: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create nudge rules dir: %w", err)
	}
	return os.WriteFile(path, data, mode)
}

// ContentHash computes a SHA-256 hash of file contents concatenated together.
// Used to detect when character files have changed since the last extraction.
func ContentHash(contents []string) string {
	h := sha256.New()
	for _, c := range contents {
		h.Write([]byte(c))
		h.Write([]byte{0}) // separator
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// TriggerRequiresPostTool reports whether a trigger type needs mid-turn
// post-tool injection (PostToolNudgeFunc). These triggers can only fire
// on backends that advertise CanPostToolNudge.
func TriggerRequiresPostTool(triggerType string) bool {
	switch triggerType {
	case "every_n_tools", "after_error", "tool_pattern":
		return true
	default:
		return false
	}
}

// TriggerRequiresPreAnswer reports whether a trigger type needs mid-turn
// pre-answer injection (PreAnswerNudgeFunc).
func TriggerRequiresPreAnswer(triggerType string) bool {
	return triggerType == "pre_answer"
}

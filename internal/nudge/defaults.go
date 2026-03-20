package nudge

import (
	"fmt"
	"strings"
)

// toolDescriptions maps tool names to short human-readable descriptions.
// Used by DefaultRules to build reminder text listing available tools.
var toolDescriptions = map[string]string{
	"shell":                "run commands",
	"tmux":                 "manage terminal sessions",
	"browser":              "headless browser control",
	"read":                 "read file contents",
	"write":                "create or overwrite files",
	"edit":                 "find-replace in files",
	"summary":              "summarize files via fast model",
	"http_request":         "HTTP calls with secret resolution",
	"web_search":           "search the web",
	"web_fetch":            "fetch URL as markdown",
	"memory_search":        "search memory and conversation history",
	"scratchpad":           "persistent working notes surviving compaction",
	"todo":                 "task management",
	"task_list":            "track current work items",
	"remind":               "defer a thought for later with scheduled surfacing",
	"send_to_chat":         "proactive messages to user",
	"send_to_session":      "cross-session messaging",
	"spawn":                "create sub-agents for parallel work",
	"bitwarden_search":     "search credential vault",
	"bitwarden_unlock":     "unlock credential vault item",
}

// toolOrder defines a stable display order for tool descriptions.
var toolOrder = []string{
	"shell", "tmux", "browser",
	"read", "write", "edit", "summary",
	"http_request", "web_search", "web_fetch",
	"memory_search", "scratchpad", "todo", "task_list", "remind",
	"send_to_chat", "send_to_session", "spawn",
	"bitwarden_search", "bitwarden_unlock",
}

// SkillSummary holds a skill's name and description for default nudge generation.
type SkillSummary struct {
	Name        string
	Description string
}

// defaultBraindeadPrompt is the fallback text when no custom prompt is configured.
const defaultBraindeadPrompt = "You've made many consecutive tool calls. Stop and verify: is what you're doing right now what the user actually asked for?"

// BraindeadRule builds a nudge rule for the braindead warning.
// It fires every N tool calls, where N is the configured threshold.
// Returns nil if threshold <= 0 (disabled).
func BraindeadRule(threshold int, prompt string) []Rule {
	if threshold <= 0 {
		return nil
	}
	if prompt == "" {
		prompt = defaultBraindeadPrompt
	}
	return []Rule{{
		Text:       prompt,
		SourceFile: "builtin",
		Trigger:    Trigger{Type: "every_n_tools", N: threshold},
		Priority:   "high",
	}}
}

// ScratchpadRule builds a nudge rule that periodically reminds the agent to
// review, update, or clear scratchpad entries. The condition function gates
// firing so the nudge is suppressed when the scratchpad is empty.
// Returns nil if frequency <= 0 (disabled) or condition is nil.
func ScratchpadRule(frequency int, condition func() bool) []Rule {
	if frequency <= 0 || condition == nil {
		return nil
	}
	return []Rule{{
		Text:       "Scratchpad entries exist. Review them — update any that are stale, and clear entries you no longer need. Stale scratchpad entries waste context after compaction.",
		SourceFile: "builtin",
		Trigger:    Trigger{Type: "every_n_turns", N: frequency},
		Priority:   "low",
		Condition:  condition,
	}}
}

// DefaultRules builds nudge rules for periodic tool/skill reminders.
// Only tools present in toolNames appear in the reminder text. Skills
// are appended with their descriptions. Returns nil if nothing to list.
func DefaultRules(toolNames []string, skills []SkillSummary, frequency int) []Rule {
	if frequency <= 0 {
		frequency = 50
	}

	registered := make(map[string]bool, len(toolNames))
	for _, name := range toolNames {
		registered[name] = true
	}

	// Build tool list in stable order, filtering to registered tools.
	var parts []string
	for _, name := range toolOrder {
		if !registered[name] {
			continue
		}
		desc, ok := toolDescriptions[name]
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", name, desc))
	}
	// Include any registered tools not in toolOrder (e.g. MCP tools).
	for _, name := range toolNames {
		if _, inOrder := toolDescriptions[name]; inOrder {
			continue // already handled above
		}
		parts = append(parts, name)
	}

	if len(parts) == 0 && len(skills) == 0 {
		return nil
	}

	var b strings.Builder
	if len(parts) > 0 {
		b.WriteString("Tool reminder — you have access to: ")
		b.WriteString(strings.Join(parts, ", "))
		b.WriteString(".")
	}
	if len(skills) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Skills available: ")
		skillParts := make([]string, len(skills))
		for i, s := range skills {
			if s.Description != "" {
				skillParts[i] = fmt.Sprintf("%s (%s)", s.Name, s.Description)
			} else {
				skillParts[i] = s.Name
			}
		}
		b.WriteString(strings.Join(skillParts, ", "))
		b.WriteString(".")
	}

	return []Rule{{
		Text:       b.String(),
		SourceFile: "builtin",
		Trigger:    Trigger{Type: "every_n_turns", N: frequency},
		Priority:   "low",
	}}
}

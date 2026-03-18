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
	"send_message_to_user": "proactive messages to user",
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
	"send_message_to_user", "send_to_session", "spawn",
	"bitwarden_search", "bitwarden_unlock",
}

// SkillSummary holds a skill's name and description for default nudge generation.
type SkillSummary struct {
	Name        string
	Description string
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

// Package toolformat provides shared pure functions for formatting tool call
// summaries and result hints. Used by both the Telegram and Discord platform
// packages to avoid duplicating compact-display logic.
package toolformat

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// str extracts a string value from a parsed JSON object by key.
// Falls back to raw JSON text for non-string values.
func str(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Not a string — use raw JSON (e.g. numbers, booleans)
	return strings.TrimSpace(string(raw))
}

// CompactSummary extracts the most meaningful param values for a compact display.
func CompactSummary(toolName string, m map[string]json.RawMessage) string {
	switch toolName {
	case "shell":
		return Truncate(str(m, "command"), 60)
	case "web_fetch":
		return Truncate(str(m, "url"), 80)
	case "web_search", "memory_search":
		return Truncate(str(m, "query"), 60)
	case "http_request":
		method := str(m, "method")
		if method == "" {
			method = "GET"
		}
		return Truncate(method+" "+str(m, "url"), 80)
	case "read", "write", "edit":
		return Truncate(str(m, "path"), 80)
	case "tmux":
		op := str(m, "operation")
		name := str(m, "name")
		if op != "" && name != "" {
			return op + " " + name
		}
		if op != "" {
			return op
		}
		return Truncate(str(m, "name"), 60)
	case "todo", "scratchpad":
		return str(m, "action")
	case "remind":
		return Truncate(str(m, "text"), 40)
	case "send_to_chat":
		return Truncate(str(m, "text"), 40)
	case "spawn":
		return Truncate(str(m, "prompt"), 40)
	}

	// Fallback: use the first string-valued param (sorted for determinism)
	for _, key := range slices.Sorted(maps.Keys(m)) {
		if v := str(m, key); v != "" {
			return Truncate(v, 60)
		}
	}
	return ""
}

// CompactResultHint extracts a short hint from a tool result to append
// to the compact notification line. Returns "" if no useful hint can be extracted.
func CompactResultHint(toolName string, params json.RawMessage, result string) string {
	switch toolName {
	case "todo":
		return TodoResultHint(params, result)
	case "shell":
		return ShellResultHint(result)
	case "write":
		return WriteResultHint(result)
	case "edit":
		return EditResultHint(result)
	case "spawn":
		return SpawnResultHint(result)
	case "tmux":
		return TmuxResultHint(params, result)
	}
	return ""
}

// TodoResultHint extracts key info from todo results (e.g. "#542" from "Added #542 (medium)").
func TodoResultHint(params json.RawMessage, result string) string {
	var p struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	firstLine, _, _ := strings.Cut(result, "\n")
	switch p.Action {
	case "add":
		// "Added #542 (medium)" -> "#542"
		if i := strings.Index(firstLine, "#"); i >= 0 {
			end := strings.IndexByte(firstLine[i:], ' ')
			if end > 0 {
				return firstLine[i : i+end]
			}
			return firstLine[i:]
		}
	case "list", "search":
		// Count items by counting dividers (items separated by \n---\n)
		if strings.HasPrefix(firstLine, "No ") {
			return "0 items"
		}
		n := strings.Count(result, "\n---\n") + 1
		if n == 1 {
			return "1 item"
		}
		return fmt.Sprintf("%d items", n)
	case "transition", "complete", "drop":
		// "#542: done" or "#1: done\n#2: done" -> "done" (first line's state)
		if _, after, ok := strings.Cut(firstLine, ": "); ok {
			return Truncate(after, 20)
		}
	case "remove":
		// "#542: removed" -> ""
		return ""
	case "edit":
		// "#542: text: old -> new" -> show the ID
		if i := strings.Index(firstLine, "#"); i >= 0 {
			end := strings.IndexByte(firstLine[i:], ':')
			if end > 0 {
				return firstLine[i : i+end]
			}
		}
	}
	return ""
}

// ShellResultHint shows line count for multi-line output.
func ShellResultHint(result string) string {
	if result == "" {
		return "(empty)"
	}
	lines := strings.Count(result, "\n") + 1
	if lines <= 3 {
		return ""
	}
	return fmt.Sprintf("%d lines", lines)
}

// WriteResultHint extracts byte count from "Wrote N bytes to /path".
func WriteResultHint(result string) string {
	if strings.HasPrefix(result, "Wrote ") {
		// "Wrote 42 bytes to /path/file.go" -> "42 bytes"
		parts := strings.SplitN(result, " ", 4)
		if len(parts) >= 3 {
			return parts[1] + " " + parts[2]
		}
	}
	return ""
}

// EditResultHint extracts edit confirmation info.
func EditResultHint(result string) string {
	if strings.HasPrefix(result, "Applied ") || strings.HasPrefix(result, "Edited ") {
		firstLine, _, _ := strings.Cut(result, "\n")
		return Truncate(firstLine, 30)
	}
	return ""
}

// SpawnResultHint shows the spawned agent name.
func SpawnResultHint(result string) string {
	// Spawn results start with "Spawned agent: <name>" or similar
	if strings.HasPrefix(result, "Spawned ") {
		firstLine, _, _ := strings.Cut(result, "\n")
		return Truncate(firstLine, 30)
	}
	return ""
}

// TmuxResultHint extracts a short hint from a tmux tool result.
func TmuxResultHint(params json.RawMessage, result string) string {
	var p struct {
		Operation string `json:"operation"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	switch p.Operation {
	case "read":
		if strings.TrimSpace(result) == "" {
			return "(empty)"
		}
		lines := strings.Count(result, "\n") + 1
		if lines == 1 {
			return "1 line"
		}
		return fmt.Sprintf("%d lines", lines)
	default:
		firstLine, _, _ := strings.Cut(result, "\n")
		return Truncate(firstLine, 30)
	}
}

// Truncate shortens a string to max characters, appending "..." if truncated.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

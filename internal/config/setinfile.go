package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// SetTarget specifies where to write a key in the TOML config file.
type SetTarget struct {
	Section string // TOML section: "defaults", "sessions", "agents", etc.
	AgentID string // non-empty only when Section == "agents"
	Key     string // TOML key within the section
}

// SetInFile performs a surgical edit of a TOML config file, preserving
// comments and formatting. It finds the target section, then either
// updates an existing key or inserts a new one at the end of the section.
//
// For [[agents]] blocks, it matches the block containing id = "<agentID>".
//
// Returns the previous value (if the key existed) and any error.
func SetInFile(path string, target SetTarget, value string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}

	lines := strings.Split(string(data), "\n")

	var oldValue string
	if target.Section == "agents" {
		oldValue, lines, err = setInAgentBlock(lines, target.AgentID, target.Key, value)
	} else {
		oldValue, lines, err = setInSection(lines, target.Section, target.Key, value)
	}
	if err != nil {
		return "", err
	}

	output := strings.Join(lines, "\n")

	// Atomic write: temp file + rename.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(output), 0o644); err != nil {
		return "", fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath) // best effort cleanup
		return "", fmt.Errorf("rename: %w", err)
	}

	return oldValue, nil
}

// sectionHeaderRe matches [section] (not [[array]]).
var sectionHeaderRe = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*$`)

// arrayHeaderRe matches [[agents]].
var arrayHeaderRe = regexp.MustCompile(`^\s*\[\[([^\[\]]+)\]\]\s*$`)

// anySectionRe matches any section header (single or double bracket).
var anySectionRe = regexp.MustCompile(`^\s*\[{1,2}[^\[\]]+\]{1,2}\s*$`)

// setInSection finds [section] and sets key = value within it.
// If the section doesn't exist, it is appended before any [[agents]] blocks
// (or at EOF if no agents blocks exist).
func setInSection(lines []string, section, key, value string) (string, []string, error) {
	start, end := findSectionBounds(lines, section)

	if start < 0 {
		// Section not found — insert it.
		insertAt := findAgentsStart(lines)
		if insertAt < 0 {
			insertAt = len(lines)
		}
		// Ensure blank line before new section.
		newLines := make([]string, 0, len(lines)+3)
		newLines = append(newLines, lines[:insertAt]...)
		if insertAt > 0 && strings.TrimSpace(lines[insertAt-1]) != "" {
			newLines = append(newLines, "")
		}
		newLines = append(newLines, fmt.Sprintf("[%s]", section))
		newLines = append(newLines, fmt.Sprintf("%s = %s", key, value))
		newLines = append(newLines, lines[insertAt:]...)
		return "", newLines, nil
	}

	return replaceOrInsertKey(lines, start+1, end, key, value)
}

// setInAgentBlock finds the [[agents]] block with the given id and sets the key.
func setInAgentBlock(lines []string, agentID, key, value string) (string, []string, error) {
	start, end := findAgentBlock(lines, agentID)
	if start < 0 {
		return "", nil, fmt.Errorf("agent %q not found in config file", agentID)
	}

	return replaceOrInsertKey(lines, start+1, end, key, value)
}

// findSectionBounds returns the line range [start, end) for [section].
// start is the line with the header; end is the line of the next header or len(lines).
// Returns (-1, -1) if not found.
func findSectionBounds(lines []string, section string) (int, int) {
	target := strings.ToLower(section)
	for i, line := range lines {
		m := sectionHeaderRe.FindStringSubmatch(line)
		if m != nil && strings.ToLower(strings.TrimSpace(m[1])) == target {
			// Found section header at line i. Find end.
			end := len(lines)
			for j := i + 1; j < len(lines); j++ {
				if anySectionRe.MatchString(lines[j]) {
					end = j
					break
				}
			}
			return i, end
		}
	}
	return -1, -1
}

// findAgentBlock returns the line range [start, end) for the [[agents]] block
// whose id matches agentID. Returns (-1, -1) if not found.
func findAgentBlock(lines []string, agentID string) (int, int) {
	idPattern := regexp.MustCompile(`^\s*id\s*=\s*"` + regexp.QuoteMeta(agentID) + `"\s*$`)

	for i := 0; i < len(lines); i++ {
		m := arrayHeaderRe.FindStringSubmatch(lines[i])
		if m == nil || strings.ToLower(strings.TrimSpace(m[1])) != "agents" {
			continue
		}

		// Found an [[agents]] header at line i. Find its end.
		blockStart := i
		blockEnd := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if anySectionRe.MatchString(lines[j]) {
				blockEnd = j
				break
			}
		}

		// Check if this block has the target id.
		for j := blockStart + 1; j < blockEnd; j++ {
			if idPattern.MatchString(lines[j]) {
				return blockStart, blockEnd
			}
		}
	}
	return -1, -1
}

// findAgentsStart returns the line number of the first [[agents]] header,
// or -1 if none exists.
func findAgentsStart(lines []string) int {
	for i, line := range lines {
		m := arrayHeaderRe.FindStringSubmatch(line)
		if m != nil && strings.ToLower(strings.TrimSpace(m[1])) == "agents" {
			return i
		}
	}
	return -1
}

// keyLineRe builds a regex matching "key = ..." (possibly with leading whitespace).
func keyLineRe(key string) *regexp.Regexp {
	// Handle dotted keys like "keepalive.enabled" — match literally.
	escaped := regexp.QuoteMeta(key)
	return regexp.MustCompile(`^\s*` + escaped + `\s*=`)
}

// commentedKeyRe builds a regex matching "# key = ..." (commented out).
func commentedKeyRe(key string) *regexp.Regexp {
	escaped := regexp.QuoteMeta(key)
	return regexp.MustCompile(`^\s*#\s*` + escaped + `\s*=`)
}

// replaceOrInsertKey looks for key within lines[from:to] and either replaces
// its value or inserts a new line. Returns (oldValue, newLines, error).
func replaceOrInsertKey(lines []string, from, to int, key, value string) (string, []string, error) {
	active := keyLineRe(key)
	commented := commentedKeyRe(key)

	// First pass: look for an active (uncommented) key line.
	for i := from; i < to; i++ {
		if active.MatchString(lines[i]) {
			old := extractValue(lines[i])
			lines[i] = fmt.Sprintf("%s = %s", key, value)
			return old, lines, nil
		}
	}

	// Second pass: look for a commented-out key line — uncomment and set.
	for i := from; i < to; i++ {
		if commented.MatchString(lines[i]) {
			old := ""
			lines[i] = fmt.Sprintf("%s = %s", key, value)
			return old, lines, nil
		}
	}

	// Key not found in section — insert at end, before trailing blank lines.
	insertAt := to
	for insertAt > from && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}

	newLine := fmt.Sprintf("%s = %s", key, value)
	result := make([]string, 0, len(lines)+1)
	result = append(result, lines[:insertAt]...)
	result = append(result, newLine)
	result = append(result, lines[insertAt:]...)
	return "", result, nil
}

// extractValue extracts the value portion from a "key = value" line.
func extractValue(line string) string {
	_, after, ok := strings.Cut(line, "=")
	if !ok {
		return ""
	}
	v := strings.TrimSpace(after)
	// Strip inline comment (not inside a quoted string).
	if strings.HasPrefix(v, `"`) {
		// Find closing quote, skip inline comment after it.
		if end := strings.Index(v[1:], `"`); end >= 0 {
			return v[:end+2]
		}
	}
	if idx := strings.Index(v, " #"); idx >= 0 {
		v = strings.TrimSpace(v[:idx])
	}
	return v
}

// FormatTOMLValue formats a raw string value for TOML output based on field type.
// Returns the formatted TOML value or an error if the value is invalid for the type.
func FormatTOMLValue(value string, ft FieldType) (string, error) {
	value = strings.TrimSpace(value)
	switch ft {
	case FieldString, FieldDuration:
		// Already quoted — pass through.
		if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			return value, nil
		}
		return fmt.Sprintf("%q", value), nil

	case FieldInt:
		if _, err := strconv.Atoi(value); err != nil {
			return "", fmt.Errorf("invalid integer: %q", value)
		}
		return value, nil

	case FieldFloat:
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return "", fmt.Errorf("invalid float: %q", value)
		}
		return value, nil

	case FieldBool:
		switch strings.ToLower(value) {
		case "true", "on", "yes", "1":
			return "true", nil
		case "false", "off", "no", "0":
			return "false", nil
		default:
			return "", fmt.Errorf("invalid bool: %q (use true/false)", value)
		}
	}
	return value, nil
}

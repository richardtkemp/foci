package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SetTableArray replaces the [[section]] array-of-tables blocks in the TOML
// file at path with one block per entry, preserving every other line (other
// sections, scalar keys, blank lines, comments). Each entry maps a sub-field
// name to its value; values are formatted by Go type: string -> quoted
// (%q), float64 -> bare number, int/int64 -> bare integer, bool ->
// true/false. section may be dotted ("memory.sources" -> [[memory.sources]]).
// Passing zero entries removes the section's blocks entirely. It returns the
// number of blocks written.
func SetTableArray(path, section string, entries []map[string]any, mode os.FileMode) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read config: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	// strings.Split on a trailing "\n" yields a final "" element; strip it so
	// block-boundary scanning (which treats EOF as a block terminator) isn't
	// thrown off by whether the file happens to end in a newline. A single
	// trailing newline is always restored at the end.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	headerRe := tableArrayHeaderRe(section)

	// Find all existing blocks for this section, and remember where the
	// first one started so the new blocks can be inserted in its place.
	insertAt := -1
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		if headerRe.MatchString(lines[i]) {
			if insertAt < 0 {
				insertAt = len(out)
			}
			// Skip this block: from the header up to (but not including)
			// the next header line (any [section] or [[section]]), or EOF.
			hardEnd := i + 1
			for hardEnd < len(lines) && !anySectionRe.MatchString(lines[hardEnd]) {
				hardEnd++
			}
			// The block proper is the header + its contiguous key lines. Trailing
			// blank/comment lines after the last key are ambiguous: pure blank
			// lines are just separator spacing (consumed with the block), but a
			// trailing COMMENT is unrelated user content that must be preserved —
			// deleting it would silently eat foci.toml content. So back off over
			// the trailing blank/comment run, and only keep it (end the removal
			// early) when it actually contains a comment.
			k := hardEnd
			hasComment := false
			for k > i+1 && isBlankOrComment(lines[k-1]) {
				k--
				if strings.HasPrefix(strings.TrimSpace(lines[k]), "#") {
					hasComment = true
				}
			}
			if hasComment {
				i = k
			} else {
				i = hardEnd
			}
			continue
		}
		out = append(out, lines[i])
		i++
	}

	rendered := renderTableArrayBlocks(section, entries)

	var result []string
	if insertAt < 0 {
		// Section not previously present — append at end, before any
		// trailing blank lines, mirroring setinfile.go's approach.
		insertAt = len(out)
		for insertAt > 0 && strings.TrimSpace(out[insertAt-1]) == "" {
			insertAt--
		}
	}

	if len(rendered) > 0 {
		result = make([]string, 0, len(out)+len(rendered)+2)
		result = append(result, out[:insertAt]...)
		if insertAt > 0 && strings.TrimSpace(out[insertAt-1]) != "" {
			result = append(result, "")
		}
		result = append(result, rendered...)
		result = append(result, out[insertAt:]...)
	} else {
		result = out
	}

	output := strings.Join(result, "\n") + "\n"

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(output), mode); err != nil {
		return 0, fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath) // best effort cleanup
		return 0, fmt.Errorf("rename: %w", err)
	}

	return len(entries), nil
}

// isBlankOrComment reports whether a line is empty/whitespace-only or a
// full-line TOML comment.
func isBlankOrComment(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

// tableArrayHeaderRe builds a regexp matching a "[[section]]" header line
// (with an optional trailing comment), where section may contain dots.
func tableArrayHeaderRe(section string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*\[\[` + regexp.QuoteMeta(section) + `\]\]\s*(#.*)?$`)
}

// renderTableArrayBlocks renders one "[[section]]" block per entry, each
// followed by its "key = value" lines, with exactly one blank line between
// consecutive blocks (and none trailing after the last one).
func renderTableArrayBlocks(section string, entries []map[string]any) []string {
	var out []string
	for idx, entry := range entries {
		if idx > 0 {
			out = append(out, "")
		}
		out = append(out, fmt.Sprintf("[[%s]]", section))
		keys := make([]string, 0, len(entry))
		for k := range entry {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, fmt.Sprintf("%s = %s", k, formatTableArrayValue(entry[k])))
		}
	}
	return out
}

// formatTableArrayValue formats a Go value for TOML output based on its
// dynamic type: string -> quoted, float64 -> bare number, int/int64 -> bare
// integer, bool -> true/false. Any other type falls back to fmt.Sprintf.
func formatTableArrayValue(v any) string {
	switch val := v.(type) {
	case string:
		return fmt.Sprintf("%q", val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", val)
	}
}

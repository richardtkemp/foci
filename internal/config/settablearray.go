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
// name to its value; values are formatted by Go type: string -> TOML basic
// string (encodeTOMLBasicString), float64 -> bare number, int/int64 -> bare
// integer, bool -> true/false. section may be dotted ("memory.sources" ->
// [[memory.sources]]). Passing zero entries removes the section's blocks
// entirely. It returns the number of blocks written.
func SetTableArray(path, section string, entries []map[string]any, mode os.FileMode) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read config: %w", err)
	}
	next, err := PlanSetTableArray(string(data), section, entries)
	if err != nil {
		return 0, err
	}
	if err := WriteFileAtomic(path, next, mode); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// PlanSetTableArray is SetTableArray's pure core (see PlanSetInFile): the
// post-edit content computed from candidate content, touching no disk. It
// keeps SetTableArray's own output convention — the result always ends in
// exactly one trailing newline — so the two are byte-identical.
func PlanSetTableArray(content, section string, entries []map[string]any) (string, error) {
	lines := strings.Split(content, "\n")
	// strings.Split on a trailing "\n" yields a final "" element; strip it so
	// block-boundary scanning (which treats EOF as a block terminator) isn't
	// thrown off by whether the file happens to end in a newline. A single
	// trailing newline is always restored at the end.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	headerRe := tableArrayHeaderRe(section)
	top := topLevel(lines) // header look-alikes inside multi-line values are content (#2191)

	// Find all existing blocks for this section, and remember where the
	// first one started so the new blocks can be inserted in its place.
	insertAt := -1
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		if top[i] && headerRe.MatchString(lines[i]) {
			if insertAt < 0 {
				insertAt = len(out)
			}
			// Skip this block: the header plus its body (blockBodyEnd),
			// minus any trailing comment run that must stay
			// (blockRemovalEnd — the shared comment-keeping rule).
			i = blockRemovalEnd(lines, i, blockBodyEnd(lines, top, i))
			continue
		}
		out = append(out, lines[i])
		i++
	}

	rendered, err := renderTableArrayBlocks(section, entries)
	if err != nil {
		return "", err
	}

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

	return strings.Join(result, "\n") + "\n", nil
}

// blockBodyEnd returns the exclusive end of the table block whose header is
// lines[header]: the next top-level section header (any [section] or
// [[section]]), or EOF. top is topLevel(lines).
func blockBodyEnd(lines []string, top []bool, header int) int {
	end := header + 1
	for end < len(lines) && !(top[end] && anySectionRe.MatchString(lines[end])) {
		end++
	}
	return end
}

// blockRemovalEnd returns the exclusive end to use when REMOVING the table
// block that spans lines[header:bodyEnd]. Trailing blank lines after the
// block's last key are separator spacing — consumed with the block — but a
// trailing COMMENT is unrelated user content that must be preserved: deleting
// it would silently eat foci.toml content. So it backs off over the trailing
// blank/comment run and ends the removal early (before the run) only when the
// run actually contains a comment. This is the ONE comment-keeping rule for
// block removal, shared by SetTableArray's section replacement and mapObject
// entry deletion (mapobject.go).
func blockRemovalEnd(lines []string, header, bodyEnd int) int {
	k := bodyEnd
	hasComment := false
	for k > header+1 && isBlankOrComment(lines[k-1]) {
		k--
		if strings.HasPrefix(strings.TrimSpace(lines[k]), "#") {
			hasComment = true
		}
	}
	if hasComment {
		return k
	}
	return bodyEnd
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
// consecutive blocks (and none trailing after the last one). An error names
// the first value that cannot be written as valid TOML.
func renderTableArrayBlocks(section string, entries []map[string]any) ([]string, error) {
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
			val, err := formatTableArrayValue(entry[k])
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", section, k, err)
			}
			out = append(out, fmt.Sprintf("%s = %s", k, val))
		}
	}
	return out, nil
}

// formatTableArrayValue formats a Go value for TOML output based on its
// dynamic type: string -> TOML basic string (encodeTOMLBasicString — Go's %q
// would emit \a/\v escapes the parser rejects), float64 -> bare number,
// int/int64 -> bare integer, bool -> true/false. Any other type falls back to
// fmt.Sprintf.
func formatTableArrayValue(v any) (string, error) {
	switch val := v.(type) {
	case string:
		return encodeTOMLBasicString(val)
	case bool:
		if val {
			return "true", nil
		}
		return "false", nil
	case int:
		return strconv.Itoa(val), nil
	case int64:
		return strconv.FormatInt(val, 10), nil
	case float64:
		return strconv.FormatFloat(val, 'g', -1, 64), nil
	default:
		return fmt.Sprintf("%v", val), nil
	}
}

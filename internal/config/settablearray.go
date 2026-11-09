package config

import (
	"fmt"
	"os"
	"reflect"
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
	if err := WriteConfigFile(path, next, mode); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// PlanSetTableArray is SetTableArray's pure core (see PlanSetInFile): the
// post-edit content computed from candidate content, touching no disk. It
// keeps SetTableArray's own output convention — the result always ends in
// exactly one trailing newline — so the two are byte-identical.
//
// When the array already exists, only the entries that differ are edited
// (planTableArrayDiff, #2938), so unchanged blocks keep their comments and
// layout; the result must decode exactly as the whole rewrite below would.
//
// The element diff also edits an array the file writes as one inline
// assignment (`tts = [{...}, ...]`, Dick's live [[tts]], #2946); clearing one
// removes the assignment (planClearInlineArray).
func PlanSetTableArray(content, section string, entries []map[string]any) (string, error) {
	if next, ok, err := planClearInlineArray(content, section, entries); ok {
		return next, err
	}
	whole, err := planSetTableArrayWhole(content, section, entries)
	if err != nil {
		return "", err
	}
	if next, ok := planTableArrayDiff(content, section, entries); ok {
		return next, nil
	}
	return whole, nil
}

// planTableArrayDiff edits the existing array at section element by element
// into entries. ok is false when it cannot, or when its result does not
// decode as content with exactly that array replaced by entries.
func planTableArrayDiff(content, section string, entries []map[string]any) (string, bool) {
	keys := strings.Split(section, ".")
	if len(entries) == 0 || keys[0] == agentsKey {
		return "", false
	}
	for _, k := range keys {
		if !bareKeyRe.MatchString(k) {
			return "", false
		}
	}
	p := KeyPath{Keys: keys}
	cur, set, err := LookupPath(content, p)
	if err != nil || !set {
		return "", false
	}
	oldArr, ok := asArray(cur)
	if !ok {
		return "", false
	}
	texts := make([]string, len(entries))
	newArr := make([]any, len(entries))
	for i, entry := range entries {
		var err error
		if texts[i], err = entryInlineTable(entry); err != nil {
			return "", false
		}
		if newArr[i], err = DecodeTOMLValue(texts[i]); err != nil {
			return "", false
		}
	}
	ed, err := arrayEditorFor(content, p)
	if err != nil || applyArrayDiff(ed, oldArr, newArr, texts) != nil {
		return "", false
	}
	next := strings.TrimRight(ed.result(), "\n") + "\n"
	got, err1 := decodeDoc(next)
	want, err2 := decodeDoc(content)
	if err1 != nil || err2 != nil || setIn(want, keys, newArr) != nil || !reflect.DeepEqual(normValue(got), normValue(want)) {
		return "", false
	}
	return next, true
}

// entryInlineTable renders one entry as a TOML inline table, keys sorted.
// Sub-field names must be bare keys.
func entryInlineTable(entry map[string]any) (string, error) {
	names := make([]string, 0, len(entry))
	for k := range entry {
		if !bareKeyRe.MatchString(k) {
			return "", fmt.Errorf("field name %q is not a bare TOML key", k)
		}
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for j, k := range names {
		v, err := formatTableArrayValue(entry[k])
		if err != nil {
			return "", fmt.Errorf("%s: %w", k, err)
		}
		parts[j] = k + " = " + v
	}
	return "{" + strings.Join(parts, ", ") + "}", nil
}

// planClearInlineArray removes a section the file holds as an inline array
// assignment (`tts = [...]`) when entries is empty: the block planners only
// see [[section]] headers and would leave it in place. ok is false when this
// is not that case.
func planClearInlineArray(content, section string, entries []map[string]any) (string, bool, error) {
	keys := strings.Split(section, ".")
	if len(entries) > 0 || keys[0] == agentsKey {
		return "", false, nil
	}
	p := KeyPath{Keys: keys}
	if _, set, err := LookupPath(content, p); err != nil || !set {
		return "", false, nil
	}
	lines := strings.Split(content, "\n")
	top := topLevel(lines)
	headerRe := tableArrayHeaderRe(section)
	for i, l := range lines {
		if top[i] && headerRe.MatchString(l) {
			return "", false, nil
		}
	}
	next, err := PlanUnsetPath(content, p)
	if err != nil {
		return "", true, err
	}
	return strings.TrimRight(next, "\n") + "\n", true, nil
}

// planSetTableArrayWhole rewrites every [[section]] block (the original
// PlanSetTableArray).
func planSetTableArrayWhole(content, section string, entries []map[string]any) (string, error) {
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
			i = blockRemovalEnd(lines, i, subTablesEnd(lines, top, section, blockBodyEnd(lines, top, i)))
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

// subTablesEnd extends a [[section]] block's end over the sub-tables that
// belong to its element ([section.x], [[section.x]]), so removing the block
// does not leave them behind, attached to the previous element.
func subTablesEnd(lines []string, top []bool, section string, end int) int {
	subRe := regexp.MustCompile(`^\s*\[\[?` + regexp.QuoteMeta(section) + `\.`)
	for end < len(lines) && top[end] && subRe.MatchString(lines[end]) {
		end = blockBodyEnd(lines, top, end)
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
	case map[string]string:
		return formatInlineStringMap(val)
	default:
		return fmt.Sprintf("%v", val), nil
	}
}

// formatInlineStringMap writes a string->string table as a TOML inline table,
// keys sorted; a key that is not bare is quoted (FieldStringMap, #2946).
func formatInlineStringMap(m map[string]string) (string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		kt := k
		if !bareKeyRe.MatchString(k) {
			q, err := encodeTOMLBasicString(k)
			if err != nil {
				return "", err
			}
			kt = q
		}
		v, err := encodeTOMLBasicString(m[k])
		if err != nil {
			return "", err
		}
		parts[i] = kt + " = " + v
	}
	return "{" + strings.Join(parts, ", ") + "}", nil
}

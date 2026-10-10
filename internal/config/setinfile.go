package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// SetTarget specifies where to write a key in the TOML config file.
type SetTarget struct {
	Section string // TOML section: "agent_loop", "sessions", "agents", etc.
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
func SetInFile(path string, target SetTarget, value string, mode os.FileMode) (string, error) {
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
	if err := os.WriteFile(tmpPath, []byte(output), mode); err != nil {
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

// topLevel reports, per line, whether the line starts outside any value: a
// header, key line, comment or blank line rather than the body of a multi-line
// string, array or inline table. The line regexes here (headers, key lines,
// commented keys, id lines) must only be trusted on such lines (#2191): a
// string line "[deploy]" or a nested array row `[ "a" ]` otherwise reads as a
// table header. A value that never closes leaves the rest of the file
// unmasked, the old line-based reading; valueSpan refuses an edit to it.
func topLevel(lines []string) []bool {
	top := make([]bool, len(lines))
	for i := 0; i < len(lines); i++ {
		top[i] = true
		t := strings.TrimSpace(lines[i])
		if t == "" || t[0] == '#' || t[0] == '[' || !strings.Contains(t, "=") {
			continue
		}
		end, _, err := valueSpan(lines, i)
		if err != nil {
			for j := i + 1; j < len(lines); j++ {
				top[j] = true
			}
			break
		}
		i = end
	}
	return top
}

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

// setInAgentBlock finds the [[agents]] block with the given id and sets the
// key. A dotted key may already live in a sub-table header form ([agents.loop]
// max_tool_loops = …) — TOML attributes those headers to the preceding
// [[agents]] entry — so the locator checks both forms. A NEW dotted key is
// written into its [agents.<tablePath>] table (reusing it if one already
// exists — even without this specific leaf — or creating it fresh otherwise),
// splitting at the key's LAST dot: tablePath = everything before, leaf = the
// final segment. This "upgrades" what used to be an inline dotted key
// (groups.calls.new-site = …, sitting in the [[agents]] block's own body) to
// proper table form; TOML tolerates the old inline-then-header ordering fine,
// but the table form reads cleanly and matches how a human would write it,
// and is required for the growing set of per-agent map fields (#1231) whose
// keys are user-defined and open-ended, not a single fixed struct path. A key
// with no dot at all keeps using the flat inline-key form (nothing to
// "upgrade" — there's no table to have one).
func setInAgentBlock(lines []string, agentID, key, value string) (string, []string, error) {
	loc, err := locateAgentKey(lines, agentID, key)
	if err != nil {
		return "", nil, err
	}
	if loc.found >= 0 {
		end, old, err := valueSpan(lines, loc.found)
		if err != nil {
			return "", nil, err
		}
		return old, replaceSpan(lines, loc.found, end, fmt.Sprintf("%s = %s", loc.lineKey, value)), nil
	}

	tablePath, leaf, dotted := cutLastDot(key)
	if !dotted {
		return replaceOrInsertKey(lines, loc.inlineFrom, loc.insertAt, key, value)
	}
	if loc.subTableFrom >= 0 {
		return replaceOrInsertKey(lines, loc.subTableFrom, loc.subTableTo, leaf, value)
	}
	return insertNewSection(lines, loc.subRegionEnd, "agents."+tablePath, leaf, value)
}

// cutLastDot splits key at its LAST "." into (everything before, final
// segment). ok is false for a flat key with no dot.
func cutLastDot(key string) (tablePath, leaf string, ok bool) {
	i := strings.LastIndex(key, ".")
	if i < 0 {
		return "", key, false
	}
	return key[:i], key[i+1:], true
}

// insertNewSection inserts a brand-new "[header]\nleaf = value" block at
// line index at, returning the updated lines (old value is always "" — the
// section didn't exist).
func insertNewSection(lines []string, at int, header, leaf, value string) (string, []string, error) {
	newLines := make([]string, 0, len(lines)+2)
	newLines = append(newLines, lines[:at]...)
	newLines = append(newLines, fmt.Sprintf("[%s]", header))
	newLines = append(newLines, fmt.Sprintf("%s = %s", leaf, value))
	newLines = append(newLines, lines[at:]...)
	return "", newLines, nil
}

// agentKeyLoc is locateAgentKey's result: the line holding the key (-1 when
// absent) and the key form as written on that line (inline dotted key, or the
// sub-table leaf), plus the bounds setInAgentBlock needs for insertion.
type agentKeyLoc struct {
	found      int    // line index of the existing assignment, -1 if absent
	lineKey    string // key text to keep on the rewritten line
	inlineFrom int    // first line after the [[agents]] header
	insertAt   int    // insertion bound for a new flat (non-dotted) inline key

	// subTableFrom/subTableTo: body bounds of an EXISTING [agents.<tablePath>]
	// matching a new dotted key's table path (longest match if more than one
	// [agents.*] header could apply) — insert the new leaf here if >= 0.
	subTableFrom, subTableTo int
	// subRegionEnd: line index right after the LAST contiguous [agents.*]
	// sub-table (or == insertAt if there are none) — where a brand-new
	// [agents.<tablePath>] table gets created when no existing one matches.
	subRegionEnd int
}

// locateAgentKey finds where `key` lives for the [[agents]] block with the
// given id: first as an inline (possibly dotted) key within the block, then
// as a leaf inside a contiguous [agents.<sub>] sub-table following it. When
// absent, also locates (for a dotted key) an existing [agents.<tablePath>]
// table to insert the new leaf into, or failing that the insertion point for
// a fresh one — see setInAgentBlock.
func locateAgentKey(lines []string, agentID, key string) (agentKeyLoc, error) {
	start, end := findAgentBlock(lines, agentID)
	if start < 0 {
		return agentKeyLoc{}, fmt.Errorf("agent %q not found in config file", agentID)
	}
	loc := agentKeyLoc{found: -1, lineKey: key, inlineFrom: start + 1, insertAt: end, subTableFrom: -1, subRegionEnd: end}

	top := topLevel(lines)
	active := keyLineRe(key)
	for i := start + 1; i < end; i++ {
		if top[i] && active.MatchString(lines[i]) {
			loc.found = i
			return loc, nil
		}
	}

	tablePath, _, dotted := cutLastDot(key)

	// Contiguous [agents.*] sub-tables after the block belong to this entry
	// (only for the LAST [[agents]] block do they follow it directly, but any
	// sub-tables between this block and the next [[agents]]/other header are
	// this entry's by TOML's rules — findAgentBlock's `end` stops at the first
	// header, so walk from there).
	bestSubLen := -1
	i := end
	for i < len(lines) {
		m := sectionHeaderRe.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		header := strings.ToLower(strings.TrimSpace(m[1]))
		if !strings.HasPrefix(header, "agents.") {
			break
		}
		bodyEnd := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if top[j] && anySectionRe.MatchString(lines[j]) {
				bodyEnd = j
				break
			}
		}
		sub := strings.TrimPrefix(header, "agents.")
		// [agents.loop] + key "loop.max_tool_loops" → leaf "max_tool_loops".
		if leaf, ok := strings.CutPrefix(strings.ToLower(key), sub+"."); ok {
			leafRe := keyLineRe(leaf)
			for j := i + 1; j < bodyEnd; j++ {
				if top[j] && leafRe.MatchString(lines[j]) {
					loc.found = j
					loc.lineKey = leaf
					return loc, nil
				}
			}
		}
		// This sub-table doesn't have the leaf, but if its name IS the new
		// dotted key's table path exactly (longest match wins — matters e.g.
		// when both [agents.groups] and [agents.groups.calls] exist), it's
		// where a new leaf should be inserted rather than creating a
		// duplicate/inline entry elsewhere.
		if dotted && strings.EqualFold(sub, tablePath) && len(sub) > bestSubLen {
			bestSubLen = len(sub)
			loc.subTableFrom = i + 1
			loc.subTableTo = bodyEnd
		}
		i = bodyEnd
		loc.subRegionEnd = i
	}
	return loc, nil
}

// findSectionBounds returns the line range [start, end) for [section].
// start is the line with the header; end is the line of the next header or len(lines).
// Returns (-1, -1) if not found.
func findSectionBounds(lines []string, section string) (int, int) {
	target := strings.ToLower(section)
	top := topLevel(lines)
	for i, line := range lines {
		if !top[i] {
			continue
		}
		m := sectionHeaderRe.FindStringSubmatch(line)
		if m != nil && strings.ToLower(strings.TrimSpace(m[1])) == target {
			// Found section header at line i. Find end.
			end := len(lines)
			for j := i + 1; j < len(lines); j++ {
				if top[j] && anySectionRe.MatchString(lines[j]) {
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
	top := topLevel(lines)

	for i := 0; i < len(lines); i++ {
		if !top[i] {
			continue
		}
		m := arrayHeaderRe.FindStringSubmatch(lines[i])
		if m == nil || strings.ToLower(strings.TrimSpace(m[1])) != "agents" {
			continue
		}

		// Found an [[agents]] header at line i. Find its end.
		blockStart := i
		blockEnd := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if top[j] && anySectionRe.MatchString(lines[j]) {
				blockEnd = j
				break
			}
		}

		// Check if this block has the target id.
		for j := blockStart + 1; j < blockEnd; j++ {
			if top[j] && idPattern.MatchString(lines[j]) {
				return blockStart, blockEnd
			}
		}
	}
	return -1, -1
}

// findAgentsStart returns the line number of the first [[agents]] header,
// or -1 if none exists.
func findAgentsStart(lines []string) int {
	top := topLevel(lines)
	for i, line := range lines {
		m := arrayHeaderRe.FindStringSubmatch(line)
		if top[i] && m != nil && strings.ToLower(strings.TrimSpace(m[1])) == "agents" {
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
	top := topLevel(lines)

	// First pass: look for an active (uncommented) key line.
	for i := from; i < to; i++ {
		if top[i] && active.MatchString(lines[i]) {
			// Replace the value's whole span (it may be multi-line) with the
			// single new line, else the old body lines are orphaned.
			end, old, err := valueSpan(lines, i)
			if err != nil {
				return "", nil, err
			}
			return old, replaceSpan(lines, i, end, fmt.Sprintf("%s = %s", key, value)), nil
		}
	}

	// Second pass: look for a commented-out key line — uncomment and set.
	for i := from; i < to; i++ {
		if top[i] && commented.MatchString(lines[i]) {
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

// replaceSpan replaces lines[from..to] (inclusive) with repl, or deletes them
// when repl is "".
func replaceSpan(lines []string, from, to int, repl string) []string {
	out := make([]string, 0, len(lines)-(to-from))
	out = append(out, lines[:from]...)
	if repl != "" {
		out = append(out, repl)
	}
	return append(out, lines[to+1:]...)
}

// valueSpan finds the last line of the value assigned on line i ("key = value")
// and returns it with the value's text (comments stripped, lines joined by
// "\n"). A TOML value spans lines as a multi-line array or inline table, or as
// a triple-quoted string, basic or literal (#2188), so it tokenises the value
// across lines: brackets nest, and inside any kind of string, brackets, '#'
// and other quotes are content. It deliberately ignores section bounds: a
// string line such as "[deploy]" looks like a header, and in valid TOML no
// value runs past a real one. A value still open at EOF is an error: guessing
// where it ends would corrupt the lines after it.
func valueSpan(lines []string, i int) (int, string, error) {
	const (
		none = iota
		basic
		literal
		mlBasic
		mlLiteral
	)
	mode, depth := none, 0
	var parts []string
	col := strings.Index(lines[i], "=") + 1
	for ln := i; ln < len(lines); ln, col = ln+1, 0 {
		line := lines[ln]
		cut := len(line)
	scan:
		for j := col; j < len(line); j++ {
			c := line[j]
			switch mode {
			case basic, mlBasic:
				switch {
				case c == '\\':
					j++ // escaped char; a trailing '\' is a line continuation
				case c == '"' && mode == basic:
					mode = none
				case c == '"':
					// A run of 3-5 quotes closes; quotes beyond 3 are content.
					if n := quoteRun(line, j, '"'); n >= 3 {
						mode = none
						j += n - 1
					}
				}
			case literal, mlLiteral:
				switch {
				case c == '\'' && mode == literal:
					mode = none
				case c == '\'':
					if n := quoteRun(line, j, '\''); n >= 3 {
						mode = none
						j += n - 1
					}
				}
			default:
				switch c {
				case '#':
					cut = j
					break scan
				case '[', '{':
					depth++
				case ']', '}':
					depth--
				case '"', '\'':
					single, multi := basic, mlBasic
					if c == '\'' {
						single, multi = literal, mlLiteral
					}
					if quoteRun(line, j, c) >= 3 {
						mode = multi
						j += 2
					} else {
						mode = single
					}
				}
			}
		}
		part := line[col:cut]
		if mode != mlBasic && mode != mlLiteral {
			part = strings.TrimRight(part, " \t")
		}
		parts = append(parts, part)
		if mode == basic || mode == literal {
			mode = none // unterminated one-line string: TOML's error to report, not a span
		}
		if mode == none && depth <= 0 {
			return ln, strings.TrimSpace(strings.Join(parts, "\n")), nil
		}
	}
	return 0, "", fmt.Errorf("the value at line %d never closes; refusing to edit", i+1)
}

// quoteRun counts the consecutive q bytes in line from index j.
func quoteRun(line string, j int, q byte) int {
	n := 0
	for j+n < len(line) && line[j+n] == q {
		n++
	}
	return n
}

// errNotUTF8 is the refusal for string values TOML cannot hold: invalid
// UTF-8. Go's %q would write it as \xff (which reads back as the different
// character U+00FF), and the parser silently substitutes U+FFFD — both lose
// the value, so it is refused instead.
func errNotUTF8(s string) error {
	return fmt.Errorf("not valid UTF-8 (TOML cannot hold it): %q", s)
}

// encodeTOMLBasicString encodes a Go string as one TOML basic string
// (double-quoted): `"` and `\` are escaped, \b \t \n \f \r stand for
// themselves, and every other control character (U+0000–U+001F and U+007F)
// becomes \uXXXX. Everything else — printable and non-ASCII alike — is
// written as-is. This is the ONE encoder for every string this package
// writes into foci.toml; Go's %q is not a substitute (it emits \a, \v and
// \xff, which TOML rejects or misreads).
func encodeTOMLBasicString(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errNotUTF8(s)
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\b':
			sb.WriteString(`\b`)
		case '\t':
			sb.WriteString(`\t`)
		case '\n':
			sb.WriteString(`\n`)
		case '\f':
			sb.WriteString(`\f`)
		case '\r':
			sb.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String(), nil
}

// decodeTOMLQuotedValue decodes an already-quoted TOML value by parsing
// "v = <value>" with the same parser Load uses. It returns the decoded string
// only when the value is exactly ONE string-valued key; anything else — a
// parse error, trailing garbage, a second key, a non-string type — is an
// error naming the value.
func decodeTOMLQuotedValue(value string) (string, error) {
	invalid := fmt.Errorf("invalid quoted string: %q", value)
	var m map[string]any
	if err := toml.Unmarshal([]byte("v = "+value), &m); err != nil {
		return "", invalid
	}
	if len(m) != 1 {
		return "", invalid
	}
	s, ok := m["v"].(string)
	if !ok {
		return "", invalid
	}
	return s, nil
}

// FormatTOMLValue formats a raw string value for TOML output based on field
// type. Returns the formatted TOML value or an error if the value is invalid
// for the type. For FieldString the result is always ONE well-formed TOML
// string: it parses, as `v = <result>`, to exactly one key v holding the
// intended string — see the FieldString case for the quoting rules. The
// other types emit bare scalars or checked durations exactly as before.
func FormatTOMLValue(value string, ft FieldType) (string, error) {
	value = strings.TrimSpace(value)
	switch ft {
	case FieldString:
		// A value that is not valid UTF-8 is refused outright (see
		// errNotUTF8) — the quoted-decode path below would otherwise let it
		// through as the parser's U+FFFD substitution.
		//
		// An already-quoted value (length ≥ 2, starting and ending with `"`;
		// the one-character value `"` is NOT already quoted) is decoded as
		// `v = <value>` and re-encoded in canonical form, so only a value
		// that is exactly one TOML string passes: `"x" # "y"` becomes `"x"`,
		// while trailing garbage, unterminated strings and smuggled second
		// keys are refused instead of written into foci.toml. Unquoted
		// values (and the bare `"`) go through encodeTOMLBasicString, which
		// escapes every control character.
		if !utf8.ValidString(value) {
			return "", errNotUTF8(value)
		}
		if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			decoded, err := decodeTOMLQuotedValue(value)
			if err != nil {
				return "", err
			}
			return encodeTOMLBasicString(decoded)
		}
		return encodeTOMLBasicString(value)

	case FieldDuration:
		// The runtime parses durations with time.ParseDuration, so an
		// unparseable value would land in foci.toml and only fail (or
		// silently fall back) after the next reload — hold durations to
		// the same type check as int/float/bool below. The already-quoted
		// passthrough form is checked on its inner text, so quoting
		// cannot smuggle an invalid value past the check. Empty stays
		// valid: several duration fields document "empty = <default>" or
		// "empty disables it".
		inner := value
		quoted := len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)
		if quoted {
			inner = value[1 : len(value)-1]
		}
		if inner != "" {
			if _, err := time.ParseDuration(inner); err != nil {
				return "", fmt.Errorf("invalid duration: %q (use e.g. 30s, 5m, 1h)", value)
			}
		}
		if quoted {
			return value, nil
		}
		return fmt.Sprintf("%q", value), nil

	case FieldSchedule:
		// Same shape as FieldDuration, held to ParseSchedule instead: the
		// load-time walk (validateTaggedDurations) and every write path must
		// accept exactly the same values, so a "HH:MM" clock time like 04:00
		// (Dick's live-config form) passes while banana / 25:00 / 0s are
		// refused here rather than written and rejected at run time. Empty
		// stays valid: reset_time = "" means "never".
		inner := value
		quoted := len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)
		if quoted {
			inner = value[1 : len(value)-1]
		}
		if inner != "" {
			if _, err := ParseSchedule(inner); err != nil {
				return "", fmt.Errorf("invalid schedule: %q (use HH:MM like 04:00, or a duration like 20h)", value)
			}
		}
		if quoted {
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

	case FieldStringList:
		// The wire value is a JSON array of strings; emit a single-line TOML
		// array. Writing single-line keeps replaceOrInsertKey's span logic in
		// sync (it collapses any prior multi-line array to this one line).
		// Each item is encoded by encodeTOMLBasicString — Go's %q would emit
		// \a/\v escapes TOML rejects.
		var items []string
		if err := json.Unmarshal([]byte(value), &items); err != nil {
			return "", fmt.Errorf("invalid string list (expected a JSON array): %w", err)
		}
		parts := make([]string, len(items))
		for i, s := range items {
			enc, err := encodeTOMLBasicString(s)
			if err != nil {
				return "", fmt.Errorf("item %d: %w", i, err)
			}
			parts[i] = enc
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	}
	return value, nil
}

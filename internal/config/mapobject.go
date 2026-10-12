package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// This file implements the app config editor's write path for map[string]Struct
// sections ([models.*], [endpoints.*] — MapObjectFieldSpec): adding an entry,
// merging sub-fields into one, removing a sub-field and deleting an entry, as
// pure planners in the same line-oriented, comment-preserving style as
// setinfile.go. One frame is one Plan* call plus one atomic write by the
// editor's funnel (Hub.checkedFileEdit), which also runs the whole-file load
// check on the result.

// ParseMapObjectValue decodes the editor's mapObject put value — a JSON object
// mapping sub-field key → display-form string ("true", "262k", "5m") — against
// the section's registered sub-fields, the way ParseObjectListValue does for
// object lists. Refused: malformed JSON, a non-string value, an empty object,
// a sub-field the section does not register, and a value that does not fit its
// sub-field type (FormatTOMLValue is the ONE type check, shared with every
// other editor path).
func ParseMapObjectValue(spec MapObjectFieldSpec, value string) (map[string]string, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("invalid entry value: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("an entry needs at least one sub-field")
	}
	types := map[string]FieldType{}
	for _, sf := range spec.Fields {
		types[sf.Key] = sf.Type
	}
	out := make(map[string]string, len(raw))
	for _, k := range sortedKeys(raw) {
		ft, ok := types[k]
		if !ok {
			return nil, fmt.Errorf("unknown field %q for [%s.<name>]", k, spec.Section)
		}
		s, ok := raw[k].(string)
		if !ok {
			return nil, fmt.Errorf("field %q: expected a string value", k)
		}
		if _, err := FormatTOMLValue(s, ft); err != nil {
			return nil, fmt.Errorf("field %q: %w", k, err)
		}
		out[k] = s
	}
	return out, nil
}

// PlanSetMapObjectEntry computes the post-edit content for a mapObject put:
// key "<entry>", settings the parsed put value. When the entry's [S.<entry>]
// table exists, each setting is merged into it via replaceOrInsertKey — only
// the named key lines change; comments, sibling keys and any [S.<entry>.*]
// sub-table stay byte-for-byte. Otherwise the entry is CREATED as a
// [S.<entry>] table after the section's existing entries (their sub-tables
// included), or before the entry's first sub-table when only those exist, or
// where a brand-new section goes when the section has no table at all.
//
// Refusals (nothing written), in the order they are checked: an entry name
// that is not a bare TOML key (bareTOMLKeyRe — this writer never quotes),
// an unknown sub-field, a value that does not fit its type (FormatTOMLValue
// again, so validation and writing share one definition of type-fit), a
// create with no sub-fields, an entry that exists in the file only as an
// inline table or dotted keys under [S] — checked BEFORE the create rules:
// such an entry already has its model, so a partial put must be told to
// edit by hand, not that its model is missing — and a create missing the
// spec's Required sub-field (models is meaningless without its model ID).
func PlanSetMapObjectEntry(content string, spec MapObjectFieldSpec, entry string, settings map[string]string) (string, error) {
	if !bareTOMLKeyRe.MatchString(entry) {
		return "", errEntryNotBareKey(spec, entry)
	}
	ordered, err := formatMapObjectSettings(spec, entry, settings)
	if err != nil {
		return "", err
	}
	if len(ordered) == 0 {
		return "", fmt.Errorf("an entry needs at least one sub-field")
	}

	lines := strings.Split(content, "\n")
	top := topLevel(lines)
	scan := scanMapObjectSection(lines, top, spec.Section, entry)

	if scan.entryHeader >= 0 {
		// Merge: replaceOrInsertKey edits only at lines after the header,
		// so the header index is stable; the body end shifts by each
		// edit's line-count delta.
		bodyEnd := blockBodyEnd(lines, top, scan.entryHeader)
		for _, s := range ordered {
			before := len(lines)
			_, lines, err = replaceOrInsertKey(lines, scan.entryHeader+1, bodyEnd, s.key, s.toml)
			if err != nil {
				return "", err
			}
			bodyEnd += len(lines) - before
		}
		return strings.Join(lines, "\n"), nil
	}

	// An entry the file holds as an inline table or dotted keys under [S]
	// (with or without [S.<entry>.*] sub-tables) has no key lines to merge
	// into. Refuse it before the create checks: such an entry already has
	// its model, so a partial put must hear "edit foci.toml by hand", not
	// "model is required" — and a create would add a duplicate table.
	if err := errNonTableEntry(content, spec, entry, scan); err != nil {
		return "", err
	}

	// Create: a create must set the spec's Required sub-field to a
	// non-empty value (validateModelKeys is the load-time backstop with
	// the same message shape).
	if spec.Required != "" {
		if v, ok := settings[spec.Required]; !ok || strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("[%s.%s] %s is required", spec.Section, entry, spec.Required)
		}
	}

	anchor := createAnchor(lines, scan)
	body := make([]string, 0, len(ordered))
	for _, s := range ordered {
		body = append(body, s.key+" = "+s.toml)
	}
	return strings.Join(insertSectionBlock(lines, anchor, spec.Section+"."+entry, body), "\n"), nil
}

// PlanUnsetMapObject computes the post-edit content for a mapObject unset:
// key "<entry>" deletes the whole entry — its [S.<entry>] table and every
// [S.<entry>.*] sub-table, consuming the blank lines that separated them and
// keeping comment lines after a block's last key (blockRemovalEnd, the shared
// comment-keeping rule); key "<entry>.<sub>" removes that one key line from
// the entry's table, exactly as UnsetInFile removes a scalar key. Same
// refusals as PlanSetMapObjectEntry for entry names and non-table forms, plus
// "is not set" for an absent entry or sub-field.
func PlanUnsetMapObject(content string, spec MapObjectFieldSpec, key string) (string, error) {
	entry, sub, hasSub := strings.Cut(key, ".")
	if !bareTOMLKeyRe.MatchString(entry) {
		return "", errEntryNotBareKey(spec, entry)
	}

	lines := strings.Split(content, "\n")
	top := topLevel(lines)
	scan := scanMapObjectSection(lines, top, spec.Section, entry)

	if !hasSub {
		if scan.entryHeader < 0 {
			// No [S.<entry>] table: an inline/dotted entry is refused
			// (deleting only its sub-tables would leave the entry behind
			// and report success).
			if err := errNonTableEntry(content, spec, entry, scan); err != nil {
				return "", err
			}
			if len(scan.entrySubs) == 0 {
				return "", fmt.Errorf("%s.%s is not set in the config file", spec.Section, entry)
			}
		}
		// Drop the entry's own block and each sub-table block, each up to
		// its comment-keeping removal end.
		drop := make([]bool, len(lines))
		for _, hdr := range append([]int{scan.entryHeader}, scan.entrySubs...) {
			if hdr < 0 {
				continue
			}
			for i := hdr; i < blockRemovalEnd(lines, hdr, blockBodyEnd(lines, top, hdr)); i++ {
				drop[i] = true
			}
		}
		out := make([]string, 0, len(lines))
		for i, line := range lines {
			if !drop[i] {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n"), nil
	}

	// "<entry>.<sub>" — remove one sub-field key line. The sub-field must
	// be one the section registers; a key that names none may be
	// addressing (a sub-field of) one dotted quoted entry name sent flat
	// (models."glm-4.6" as models.glm-4.6[.model]), so when an entry
	// table's name matches the key or is a dotted prefix of it, say that
	// instead of a misleading "unknown field" — and never act on the
	// truncated entry the first-dot split produced.
	if !isRegisteredSubField(spec, sub) {
		if entryTableNamed(lines, top, spec.Section, key) {
			return "", errEntryNotBareKey(spec, key)
		}
		return "", fmt.Errorf("unknown field %q for [%s.<name>]", sub, spec.Section)
	}
	if scan.entryHeader < 0 {
		// Absent, or present only as sub-tables (no registered sub-field
		// is set anywhere) — unless it is an inline/dotted entry.
		if err := errNonTableEntry(content, spec, entry, scan); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s.%s.%s is not set in the config file", spec.Section, entry, sub)
	}
	active := keyLineRe(sub)
	for i := scan.entryHeader + 1; i < blockBodyEnd(lines, top, scan.entryHeader); i++ {
		if top[i] && active.MatchString(lines[i]) {
			end, _, err := valueSpan(lines, i)
			if err != nil {
				return "", err
			}
			return strings.Join(replaceSpan(lines, i, end, ""), "\n"), nil
		}
	}
	return "", fmt.Errorf("%s.%s.%s is not set in the config file", spec.Section, entry, sub)
}

// errNonTableEntry refuses (errEntryNotATable) an entry that has no
// [S.<entry>] table header but that the decoded tree holds with content its
// [S.<entry>.*] sub-table headers do not account for: an inline table, dotted
// keys under [S] (z.model = "x", alongside a [S.z.provider] sub-table or
// not), or a scalar. It returns nil when the entry is absent from the tree or
// exists only through its sub-table headers — the callers' "is not set" /
// create / delete-the-sub-tables cases. Callers reach it only when
// scan.entryHeader < 0.
func errNonTableEntry(content string, spec MapObjectFieldSpec, entry string, scan mapObjectScan) error {
	tree, err := decodeTOMLTree(content)
	if err != nil {
		return err
	}
	sec, ok := tree[spec.Section].(map[string]any)
	if !ok {
		return nil
	}
	v, ok := sec[entry]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return errEntryNotATable(spec, entry)
	}
	for k := range m {
		if !scan.entrySubNames[k] {
			return errEntryNotATable(spec, entry)
		}
	}
	return nil
}

// mapObjectSetting is one sub-field write, already formatted as a TOML value.
type mapObjectSetting struct {
	key, toml string
}

// formatMapObjectSettings validates settings against the section's registered
// sub-fields and formats each value for TOML (FormatTOMLValue — the same
// check ParseMapObjectValue ran, so a plan built straight from a map cannot
// bypass type-fit). Output order follows the spec, so writes are
// deterministic; unknown sub-field keys are refused.
func formatMapObjectSettings(spec MapObjectFieldSpec, entry string, settings map[string]string) ([]mapObjectSetting, error) {
	registered := map[string]bool{}
	for _, sf := range spec.Fields {
		registered[sf.Key] = true
	}
	for k := range settings {
		if !registered[k] {
			return nil, fmt.Errorf("unknown field %q for [%s.<name>]", k, spec.Section)
		}
	}
	var ordered []mapObjectSetting
	for _, sf := range spec.Fields {
		val, ok := settings[sf.Key]
		if !ok {
			continue
		}
		toml, err := FormatTOMLValue(val, sf.Type)
		if err != nil {
			return nil, fmt.Errorf("%s.%s.%s: %w", spec.Section, entry, sf.Key, err)
		}
		ordered = append(ordered, mapObjectSetting{key: sf.Key, toml: toml})
	}
	return ordered, nil
}

// mapObjectScan is the line-structure map of one mapObject section: where the
// addressed entry's own [S.<entry>] table sits, its [S.<entry>.*] sub-tables,
// and where the section's last table block ends (the create anchor's base).
type mapObjectScan struct {
	entryHeader int   // line of [S.<entry>], -1 when absent
	entrySubs   []int // header lines of [S.<entry>.*] sub-tables, file order
	// entrySubNames holds the sub-table names those headers open (the
	// third path segment: provider for [S.<entry>.provider.x]).
	entrySubNames map[string]bool
	// lastBlockEnd is the exclusive end of the section's LAST table block
	// ([S], [S.x], [S.x.y], ...), -1 when the section has no table at all.
	lastBlockEnd int
}

// scanMapObjectSection walks every top-level [table] header whose first path
// segment is the section (quote-aware, so [models."glm-4.6"] and
// [models.x.provider] parse correctly) and maps out the entry's tables. The
// section segment matches case-insensitively (findSectionBounds' convention);
// the entry segment matches exactly (TOML keys are case-sensitive).
func scanMapObjectSection(lines []string, top []bool, section, entry string) mapObjectScan {
	s := mapObjectScan{entryHeader: -1, lastBlockEnd: -1, entrySubNames: map[string]bool{}}
	for i, line := range lines {
		if !top[i] {
			continue
		}
		m := sectionHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		path, ok := parseTableHeaderPath(m[1])
		if !ok || len(path) == 0 || !strings.EqualFold(path[0], section) {
			continue
		}
		s.lastBlockEnd = blockBodyEnd(lines, top, i)
		if len(path) >= 2 && path[1] == entry {
			if len(path) == 2 {
				if s.entryHeader < 0 {
					s.entryHeader = i
				}
			} else {
				s.entrySubs = append(s.entrySubs, i)
				s.entrySubNames[path[2]] = true
			}
		}
	}
	return s
}

// createAnchor returns the line index a new [S.<entry>] table is inserted at:
// before the entry's first existing sub-table when only those exist (the
// entry "lives" there already), else after the section's last table block
// (backing off its trailing blank run so the separator spacing lands after
// the new table), else where a brand-new section goes (before the first
// [[agents]], else EOF).
func createAnchor(lines []string, scan mapObjectScan) int {
	if len(scan.entrySubs) > 0 {
		return scan.entrySubs[0]
	}
	if scan.lastBlockEnd >= 0 {
		anchor := scan.lastBlockEnd
		for anchor > 0 && strings.TrimSpace(lines[anchor-1]) == "" {
			anchor--
		}
		return anchor
	}
	return newSectionInsertAt(lines)
}

// entryTableNamed reports whether a [S.<name>] table exists whose NOT-BARE
// name (a quoted name containing dots, e.g. glm-4.6) is key or a dotted
// prefix of it — the frame key addresses an entry the bare-key editor cannot
// address at all. Bare-named entries never match: their sub-field path works
// normally, so an unknown sub-field stays an unknown-field error.
func entryTableNamed(lines []string, top []bool, section, key string) bool {
	for i, line := range lines {
		if !top[i] {
			continue
		}
		m := sectionHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		path, ok := parseTableHeaderPath(m[1])
		if !ok || len(path) != 2 || !strings.EqualFold(path[0], section) {
			continue
		}
		if bareTOMLKeyRe.MatchString(path[1]) {
			continue
		}
		if path[1] == key || strings.HasPrefix(key, path[1]+".") {
			return true
		}
	}
	return false
}

// isRegisteredSubField reports whether key is one of spec's sub-fields.
func isRegisteredSubField(spec MapObjectFieldSpec, key string) bool {
	for _, sf := range spec.Fields {
		if sf.Key == key {
			return true
		}
	}
	return false
}

// errEntryNotBareKey refuses an entry name this writer cannot address: the
// line editors never quote keys, so a name outside bareTOMLKeyRe (a dot, a
// space, ...) would either fail to parse or silently name a different table.
func errEntryNotBareKey(spec MapObjectFieldSpec, name string) error {
	return fmt.Errorf("%s.%s: entry names in the app must be bare TOML keys (letters, digits, _, -) — edit foci.toml by hand", spec.Section, name)
}

// errEntryNotATable refuses an edit to an entry the file holds in a form the
// line editor cannot touch safely: an inline table or dotted keys under [S]
// (the entry IS in the decoded tree, but there is no [S.<entry>] table whose
// key lines could be edited).
func errEntryNotATable(spec MapObjectFieldSpec, entry string) error {
	return fmt.Errorf("%s.%s exists in foci.toml but not as a [%s.%s] table — edit foci.toml by hand", spec.Section, entry, spec.Section, entry)
}

// parseTableHeaderPath splits the inner text of a table header ([a."b.c"]) on
// dots OUTSIDE quotes, unquoting "…"/'…' segments, so [models."glm-4.6"] is
// the path [models, glm-4.6] and [models.x.provider] is [models, x,
// provider]. Whitespace around a segment is ignored as TOML allows, so
// [ models . g ] is [models, g]. ok is false for an unterminated quote, an
// empty segment, or a bare segment holding a character outside the bare-key
// set (letters, digits, _, -; e.g. an inner space) — such a header never
// matches a section/entry pair.
func parseTableHeaderPath(inner string) ([]string, bool) {
	var path []string
	i, n := 0, len(inner)
	skipSpace := func() {
		for i < n && (inner[i] == ' ' || inner[i] == '\t') {
			i++
		}
	}
	for {
		skipSpace()
		var seg strings.Builder
		if i < n && (inner[i] == '"' || inner[i] == '\'') {
			quote := inner[i]
			i++
			closed := false
			for i < n {
				c := inner[i]
				if c == quote {
					closed = true
					i++
					break
				}
				if c == '\\' && quote == '"' && i+1 < n {
					// A basic-string escape: keep both bytes verbatim
					// (matching only needs ordinary names, not exact
					// unescaping).
					seg.WriteByte(c)
					seg.WriteByte(inner[i+1])
					i += 2
					continue
				}
				seg.WriteByte(c)
				i++
			}
			if !closed {
				return nil, false
			}
		} else {
			for i < n && isBareKeyByte(inner[i]) {
				seg.WriteByte(inner[i])
				i++
			}
		}
		if seg.Len() == 0 {
			return nil, false
		}
		path = append(path, seg.String())
		skipSpace()
		if i == n {
			return path, true
		}
		if inner[i] != '.' {
			return nil, false
		}
		i++
	}
}

// isBareKeyByte reports whether c may appear in a bare TOML key.
func isBareKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// decodeTOMLTree decodes content into the raw TOML tree (table → key → value),
// the form the entry readers and non-table-entry detection need. Quoted
// spelled keys arrive unquoted (glm-4.6 stays one key).
func decodeTOMLTree(content string) (map[string]any, error) {
	var tree map[string]any
	if _, err := toml.Decode(content, &tree); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return tree, nil
}

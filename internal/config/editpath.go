package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// editpath.go edits ANY TOML key path in foci.toml (#479): a top-level or
// nested key, a key inside one [[agents]] block chosen by its id, a key that
// needs TOML quoting, or a whole array of tables, with any TOML value. It is
// the file half of the agent config tool (`foci config set|unset|add`).
//
// The edit is textual, so comments and layout outside the edited span are
// kept. Because a textual edit of TOML has many shapes (a key line, a
// [table] or [[array]] section, dotted keys under a parent table), each plan
// tries the candidate edits in order and keeps the first whose DECODED result
// differs from the decoded original ONLY at the path and holds the requested
// value there (VerifyPathEdit). An edit no candidate can make that way is
// refused rather than guessed.

// KeyPath names one key in the config file. AgentID non-empty means a key
// inside the [[agents]] block whose id is AgentID; Keys are the unquoted
// dotted-key segments relative to that block (or to the file's root table).
// Elem non-nil names ONE element of the array at Keys (#2938); Sub then
// optionally names a key inside that element, a table (#2991).
type KeyPath struct {
	AgentID string
	Keys    []string
	Elem    *ElemSel
	Sub     []string
}

// ElemSel picks one element of an array, written after the array's key in
// brackets:
//
//	[3]             the element at index 3 (counting from 0)
//	[field=value]   the table whose field equals value
//	[value]         the table whose name (else id) equals value, or the
//	                string element equal to value
//
// A value that is not a bare key is quoted: ["/tmp/x"], [name="a b"].
type ElemSel struct {
	Index int // >= 0 selects by position; -1 otherwise
	Field string
	Value string
}

// String renders the selector in brackets.
func (e ElemSel) String() string {
	switch {
	case e.Index >= 0:
		return "[" + strconv.Itoa(e.Index) + "]"
	case e.Field != "":
		return "[" + renderDottedKey([]string{e.Field}) + "=" + renderDottedKey([]string{e.Value}) + "]"
	}
	return "[" + renderDottedKey([]string{e.Value}) + "]"
}

// Array is the path of the array p's element selector applies to (p itself
// when it has no selector).
func (p KeyPath) Array() KeyPath {
	return KeyPath{AgentID: p.AgentID, Keys: p.Keys}
}

// Element is the path of the one element p names, without p's key inside it.
func (p KeyPath) Element() KeyPath {
	return KeyPath{AgentID: p.AgentID, Keys: p.Keys, Elem: p.Elem}
}

// parseElemSel parses the selector text between the brackets.
func parseElemSel(s string) (ElemSel, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ElemSel{}, errors.New("empty element selector []")
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return ElemSel{}, fmt.Errorf("element index %d is negative", n)
		}
		return ElemSel{Index: n}, nil
	}
	sel := ElemSel{Index: -1}
	value := s
	if keys, rest, err := parseDottedKey(s); err == nil && len(keys) == 1 && strings.HasPrefix(rest, "=") {
		sel.Field = keys[0]
		value = strings.TrimSpace(rest[1:])
	}
	if value == "" {
		return ElemSel{}, fmt.Errorf("element selector [%s] has no value", s)
	}
	if value[0] == '"' || value[0] == '\'' {
		keys, rest, err := parseDottedKey(value)
		if err != nil || len(keys) != 1 || strings.TrimSpace(rest) != "" {
			return ElemSel{}, fmt.Errorf("element selector [%s]: bad quoted value", s)
		}
		value = keys[0]
	}
	sel.Value = value
	return sel, nil
}

// cutSelector splits "[...]" at the start of s into the text inside the
// brackets and the rest, honouring quotes inside.
func cutSelector(s string) (inner, rest string, err error) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '"', '\'':
			e, serr := scanStringEnd(s, i)
			if serr != nil {
				return "", "", serr
			}
			i = e - 1
		case ']':
			return s[1:i], s[i+1:], nil
		}
	}
	return "", "", errors.New("unclosed '['")
}

// resolveElem returns the index of the element sel picks in arr.
func resolveElem(arr []any, sel ElemSel, arrP KeyPath) (int, error) {
	if sel.Index >= 0 {
		if sel.Index >= len(arr) {
			return 0, fmt.Errorf("%s has %d elements; there is no element %d", arrP, len(arr), sel.Index)
		}
		return sel.Index, nil
	}
	match := func(field string) []int {
		var out []int
		for i, e := range arr {
			switch v := e.(type) {
			case map[string]any:
				if fv, ok := v[field]; ok {
					if s, isStr := fv.(string); (isStr && s == sel.Value) || (!isStr && RenderTOMLValue(fv) == sel.Value) {
						out = append(out, i)
					}
				}
			case string:
				if sel.Field == "" && v == sel.Value {
					out = append(out, i)
				}
			}
		}
		return out
	}
	var hits []int
	if sel.Field != "" {
		hits = match(sel.Field)
	} else if hits = match("name"); len(hits) == 0 {
		hits = match("id")
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		if sel.Field != "" {
			return 0, fmt.Errorf("no element of %s has %s = %q", arrP, sel.Field, sel.Value)
		}
		return 0, fmt.Errorf("no element of %s has name or id %q, or equals it", arrP, sel.Value)
	}
	return 0, fmt.Errorf("%d elements of %s match %s; pick one by index, e.g. %s[%d]", len(hits), arrP, sel, arrP, hits[0])
}

// agentsKey is the array of tables holding the agents.
const agentsKey = "agents"

// ParseKeyPath parses a key path: TOML dotted-key syntax (bare segments, or
// "basic" / 'literal' quoted ones), where a leading agents[<id>] selects one
// agent's block. Examples: agent_loop.max_tool_loops,
// agents[clutch].nudge.enable, models."claude.fast".id, blocked_paths.
// One element selector may follow the keys (blocked_paths[root]), and a
// dotted key after it names a key inside that element
// (tts[groq-playai].fallback, #2991).
func ParseKeyPath(s string) (KeyPath, error) {
	s = strings.TrimSpace(s)
	var p KeyPath
	if rest, ok := strings.CutPrefix(s, agentsKey+"["); ok {
		id, after, found := strings.Cut(rest, "]")
		if !found || strings.TrimSpace(id) == "" {
			return KeyPath{}, fmt.Errorf("key path %q: agents[...] needs an agent id, e.g. agents[main].nudge.enable", s)
		}
		p.AgentID = strings.TrimSpace(id)
		after = strings.TrimSpace(after)
		if after == "" {
			return KeyPath{}, fmt.Errorf("key path %q names a whole agent; name a key inside it, e.g. agents[%s].model", s, p.AgentID)
		}
		dot, ok := strings.CutPrefix(after, ".")
		if !ok {
			return KeyPath{}, fmt.Errorf("key path %q: expected '.' after agents[%s]", s, p.AgentID)
		}
		s = dot
	}
	keys, rest, err := parseDottedKey(s)
	if err != nil {
		return KeyPath{}, fmt.Errorf("key path %q: %w", s, err)
	}
	if strings.HasPrefix(rest, "[") {
		inner, after, err := cutSelector(rest)
		if err != nil {
			return KeyPath{}, fmt.Errorf("key path %q: element selector: %w", s, err)
		}
		sel, err := parseElemSel(inner)
		if err != nil {
			return KeyPath{}, fmt.Errorf("key path %q: %w", s, err)
		}
		p.Elem = &sel
		rest = ""
		if after = strings.TrimSpace(after); after != "" {
			dot, ok := strings.CutPrefix(after, ".")
			if !ok {
				return KeyPath{}, fmt.Errorf("key path %q: expected '.' and a key after the element selector (got %q)", s, after)
			}
			sub, subRest, err := parseDottedKey(dot)
			if err != nil {
				return KeyPath{}, fmt.Errorf("key path %q: key after the element selector: %w", s, err)
			}
			if strings.HasPrefix(subRest, "[") {
				return KeyPath{}, fmt.Errorf("key path %q: only one element selector [...] per path", s)
			}
			p.Sub = sub
			rest = subRest
		}
	}
	if strings.TrimSpace(rest) != "" {
		return KeyPath{}, fmt.Errorf("key path %q: unexpected %q after the key", s, rest)
	}
	if p.AgentID == "" && keys[0] == agentsKey {
		return KeyPath{}, fmt.Errorf("key path %q: address an agent's keys as agents[<id>].<key>", s)
	}
	p.Keys = keys
	return p, nil
}

// String renders the path in ParseKeyPath's syntax.
func (p KeyPath) String() string {
	k := renderDottedKey(p.Keys)
	if p.Elem != nil {
		k += p.Elem.String()
		if len(p.Sub) > 0 {
			k += "." + renderDottedKey(p.Sub)
		}
	}
	if p.AgentID != "" {
		return agentsKey + "[" + p.AgentID + "]." + k
	}
	return k
}

// LiveApplyAddress is the registry address the live applier keys this path
// by: section "agent" for an agent's key, else every segment but the last.
func (p KeyPath) LiveApplyAddress() (section, key string) {
	if p.AgentID != "" {
		return "agent", strings.Join(p.Keys, ".")
	}
	if len(p.Keys) == 1 {
		return p.Keys[0], ""
	}
	return strings.Join(p.Keys[:len(p.Keys)-1], "."), p.Keys[len(p.Keys)-1]
}

// bareKeyRe matches a TOML bare key segment.
var bareKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// renderDottedKey renders key segments, quoting any that is not a bare key.
func renderDottedKey(keys []string) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		if bareKeyRe.MatchString(k) {
			parts[i] = k
			continue
		}
		q, err := encodeTOMLBasicString(k)
		if err != nil {
			q = fmt.Sprintf("%q", k)
		}
		parts[i] = q
	}
	return strings.Join(parts, ".")
}

// parseDottedKey parses a TOML dotted key at the start of s and returns its
// segments and the rest of s after it (and after trailing whitespace).
func parseDottedKey(s string) ([]string, string, error) {
	var keys []string
	i := 0
	skipWS := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	for {
		skipWS()
		if i >= len(s) {
			return nil, "", errors.New("empty key")
		}
		switch s[i] {
		case '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, "", errors.New("unterminated quoted key")
			}
			seg, err := decodeTOMLQuotedValue(s[i : j+1])
			if err != nil {
				return nil, "", err
			}
			keys = append(keys, seg)
			i = j + 1
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, "", errors.New("unterminated quoted key")
			}
			keys = append(keys, s[i+1:i+1+j])
			i = i + 2 + j
		default:
			j := i
			for j < len(s) && (s[j] == '_' || s[j] == '-' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			if j == i {
				return nil, "", fmt.Errorf("unexpected %q in key", s[i:])
			}
			keys = append(keys, s[i:j])
			i = j
		}
		skipWS()
		if i < len(s) && s[i] == '.' {
			i++
			continue
		}
		return keys, s[i:], nil
	}
}

// ---- document model ----

// docSection is one table of the file: the root table (start == -1) or a
// [header] / [[header]] section and its body, up to the next header.
type docSection struct {
	start, end int      // header line (-1 for root), exclusive end of the body
	header     []string // full header segments; nil for root
	array      bool     // [[header]]
	agentID    string   // the [[agents]] entry this section belongs to, if any
	rel        []string // path of the section relative to its container (root or the agent entry)
}

// docKey is one key = value assignment: lines start..end (inclusive).
type docKey struct {
	sec        int
	keys       []string // key segments as written on the line
	start, end int
	keyText    string // the key as written (kept on an in-place rewrite)
	indent     string
}

type tomlDoc struct {
	lines    []string
	sections []docSection
	keys     []docKey
}

// parseDoc builds the line model. It only reads lines topLevel marks as
// starting outside a value, so header look-alikes inside strings are content.
func parseDoc(content string) (*tomlDoc, error) {
	lines := strings.Split(content, "\n")
	top := topLevel(lines)
	d := &tomlDoc{lines: lines}
	d.sections = append(d.sections, docSection{start: -1, end: len(lines)})
	agentOpen := false
	for i := 0; i < len(lines); i++ {
		if !top[i] {
			continue
		}
		t := strings.TrimSpace(lines[i])
		if t == "" || t[0] == '#' {
			continue
		}
		if t[0] == '[' {
			array := strings.HasPrefix(t, "[[")
			inner := strings.TrimLeft(t, "[")
			header, rest, err := parseDottedKey(inner)
			if err != nil {
				return nil, fmt.Errorf("line %d: header: %w", i+1, err)
			}
			closer := "]"
			if array {
				closer = "]]"
			}
			if !strings.HasPrefix(strings.TrimSpace(rest), closer) {
				return nil, fmt.Errorf("line %d: malformed header", i+1)
			}
			d.sections[len(d.sections)-1].end = i
			sec := docSection{start: i, end: len(lines), header: header, array: array, rel: header}
			switch {
			case array && len(header) == 1 && header[0] == agentsKey:
				agentOpen = true
				sec.agentID = "\x00pending" // filled in from its id key below
				sec.rel = []string{}
			case agentOpen && header[0] == agentsKey:
				sec.agentID = "\x00inherit"
				sec.rel = header[1:]
			default:
				agentOpen = false
			}
			d.sections = append(d.sections, sec)
			continue
		}
		if !strings.Contains(t, "=") {
			continue
		}
		keys, rest, err := parseDottedKey(t)
		if err != nil || !strings.HasPrefix(rest, "=") {
			continue
		}
		end, _, err := valueSpan(lines, i)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		eq := strings.Index(lines[i], "=")
		d.keys = append(d.keys, docKey{
			sec:     len(d.sections) - 1,
			keys:    keys,
			start:   i,
			end:     end,
			keyText: strings.TrimSpace(lines[i][:eq]),
			indent:  lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))],
		})
		i = end
	}
	// Resolve each [[agents]] block's id, then hand it to its sub-sections.
	current := ""
	for si := range d.sections {
		s := &d.sections[si]
		switch s.agentID {
		case "\x00pending":
			current = d.agentIDOf(si)
			s.agentID = current
		case "\x00inherit":
			s.agentID = current
		default:
			current = ""
		}
	}
	return d, nil
}

// agentIDOf decodes the id key of the [[agents]] block at section si.
func (d *tomlDoc) agentIDOf(si int) string {
	for _, k := range d.keys {
		if k.sec != si || len(k.keys) != 1 || k.keys[0] != "id" {
			continue
		}
		var m map[string]any
		if _, err := toml.Decode("v = "+d.valueText(k), &m); err == nil {
			if id, ok := m["v"].(string); ok {
				return id
			}
		}
	}
	return ""
}

// trailingComment returns " # ..." when the assignment's last line ends in a
// comment after the value, so an in-place rewrite keeps it.
func (d *tomlDoc) trailingComment(k docKey) string {
	return assignTrailingComment(d.lines, k.start, k.end)
}

// valueText is the raw value text of an assignment (all its lines).
func (d *tomlDoc) valueText(k docKey) string {
	first := d.lines[k.start]
	parts := []string{first[strings.Index(first, "=")+1:]}
	parts = append(parts, d.lines[k.start+1:k.end+1]...)
	return strings.Join(parts, "\n")
}

// inContainer reports whether section s is in p's container: the agent's
// region, or (for a root path) any section outside every agent region that
// is not itself under agents.
func (s docSection) inContainer(p KeyPath) bool {
	if p.AgentID != "" {
		return s.agentID == p.AgentID
	}
	return s.agentID == "" && (len(s.header) == 0 || s.header[0] != agentsKey)
}

func hasPrefix(full, prefix []string) bool {
	if len(full) < len(prefix) {
		return false
	}
	for i := range prefix {
		if full[i] != prefix[i] {
			return false
		}
	}
	return true
}

func equalKeys(a, b []string) bool { return len(a) == len(b) && hasPrefix(a, b) }

// pathDefs is everything in the file that defines (part of) the path.
type pathDefs struct {
	exact    []docKey // key = value at exactly the path
	under    []docKey // dotted keys defining something below the path, outside the deleted sections
	sections []int    // [path...] / [[path...]] sections
	outer    []docKey // an assignment whose value CONTAINS the path (inline table/array)
}

func (d *tomlDoc) find(p KeyPath) pathDefs {
	var out pathDefs
	deleted := map[int]bool{}
	for si, s := range d.sections {
		if s.start < 0 || !s.inContainer(p) {
			continue
		}
		if hasPrefix(s.rel, p.Keys) {
			out.sections = append(out.sections, si)
			deleted[si] = true
		}
	}
	for _, k := range d.keys {
		s := d.sections[k.sec]
		if deleted[k.sec] || !s.inContainer(p) {
			continue
		}
		full := append(append([]string{}, s.rel...), k.keys...)
		switch {
		case equalKeys(full, p.Keys):
			out.exact = append(out.exact, k)
		case hasPrefix(full, p.Keys):
			out.under = append(out.under, k)
		case hasPrefix(p.Keys, full):
			out.outer = append(out.outer, k)
		}
	}
	return out
}

func (pd pathDefs) empty() bool {
	return len(pd.exact) == 0 && len(pd.under) == 0 && len(pd.sections) == 0
}

// removal is a line range [from, to) to delete.
type removal struct{ from, to int }

// withoutDefs returns the lines with every definition of the path removed.
func (d *tomlDoc) withoutDefs(pd pathDefs) []string {
	var rs []removal
	for _, si := range pd.sections {
		s := d.sections[si]
		rs = append(rs, removal{s.start, blockRemovalEnd(d.lines, s.start, s.end)})
	}
	for _, k := range append(append([]docKey{}, pd.exact...), pd.under...) {
		rs = append(rs, removal{k.start, k.end + 1})
	}
	drop := make([]bool, len(d.lines))
	for _, r := range rs {
		for i := r.from; i < r.to; i++ {
			drop[i] = true
		}
	}
	out := make([]string, 0, len(d.lines))
	for i, l := range d.lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return out
}

// ---- plans ----

// PlanSetPath returns content with the key at p set to value (TOML value
// text: a string, number, bool, datetime, array, inline table or array of
// inline tables). A whole array of tables is replaced. The result is
// verified (VerifyPathEdit) before it is returned.
func PlanSetPath(content string, p KeyPath, value string) (string, error) {
	if len(p.Sub) > 0 {
		return planElementSubEdit(content, p, value, false)
	}
	if p.Elem != nil {
		return planElementEdit(content, p, elemReplace, value)
	}
	want, err := DecodeTOMLValue(value)
	if err != nil {
		return "", err
	}
	d, err := parseDoc(content)
	if err != nil {
		return "", err
	}
	if p.AgentID != "" && !d.hasAgent(p.AgentID) {
		return "", fmt.Errorf("agent %q not found in config file", p.AgentID)
	}
	pd := d.find(p)
	if len(pd.outer) > 0 {
		k := pd.outer[0]
		return "", fmt.Errorf("%s is inside the value of %q (line %d); set that key instead", p, k.keyText, k.start+1)
	}

	var candidates []string
	// 0. An array set to an array: edit only the elements that differ, so
	// the others keep their layout and the comments between them (#2938).
	if next, ok := planArrayDiff(content, p, want, value); ok {
		candidates = append(candidates, next)
	}
	// 1. Rewrite the one existing assignment in place.
	if len(pd.exact) == 1 && len(pd.under) == 0 && len(pd.sections) == 0 {
		k := pd.exact[0]
		line := k.indent + k.keyText + " = " + strings.TrimSpace(value) + d.trailingComment(k)
		lines := replaceSpan(append([]string{}, d.lines...), k.start, k.end, line)
		candidates = append(candidates, strings.Join(lines, "\n"))
	}
	// 2. Remove every definition, then insert one assignment.
	base := d.lines
	if !pd.empty() {
		base = d.withoutDefs(pd)
	}
	bd, err := parseDoc(strings.Join(base, "\n"))
	if err != nil {
		return "", err
	}
	candidates = append(candidates, bd.insertCandidates(p, strings.TrimSpace(value))...)

	var lastErr error
	for _, next := range candidates {
		if lastErr = VerifyPathEdit(content, next, p, want, false); lastErr == nil {
			return next, nil
		}
	}
	return "", fmt.Errorf("cannot set %s in place without changing anything else: %w", p, lastErr)
}

// PlanUnsetPath returns content with every definition of the key at p
// removed. An absent key is an error: there is nothing to change.
func PlanUnsetPath(content string, p KeyPath) (string, error) {
	if len(p.Sub) > 0 {
		return planElementSubEdit(content, p, "", true)
	}
	if p.Elem != nil {
		return planElementEdit(content, p, elemRemove, "")
	}
	d, err := parseDoc(content)
	if err != nil {
		return "", err
	}
	if p.AgentID != "" && !d.hasAgent(p.AgentID) {
		return "", fmt.Errorf("agent %q not found in config file", p.AgentID)
	}
	pd := d.find(p)
	if len(pd.outer) > 0 {
		k := pd.outer[0]
		return "", fmt.Errorf("%s is inside the value of %q (line %d); set that key instead", p, k.keyText, k.start+1)
	}
	if pd.empty() {
		return "", fmt.Errorf("%s is not set in the config file", p)
	}
	next := strings.Join(d.withoutDefs(pd), "\n")
	if err := VerifyPathEdit(content, next, p, nil, true); err != nil {
		return "", fmt.Errorf("cannot unset %s without changing anything else: %w", p, err)
	}
	return next, nil
}

func (d *tomlDoc) hasAgent(id string) bool {
	for _, s := range d.sections {
		if s.agentID == id && len(s.rel) == 0 && s.array {
			return true
		}
	}
	return false
}

// insertCandidates lists the files that assign value at p, most natural
// layout first: into the deepest existing table holding the path, then a new
// table for the path's parent, then a dotted key in the container's own
// table.
func (d *tomlDoc) insertCandidates(p KeyPath, value string) []string {
	var out []string
	deepest := -1
	for si, s := range d.sections {
		if s.start < 0 || !s.inContainer(p) || s.array || len(s.rel) == 0 || len(s.rel) >= len(p.Keys) || !hasPrefix(p.Keys, s.rel) {
			continue
		}
		if deepest < 0 || len(s.rel) >= len(d.sections[deepest].rel) {
			deepest = si
		}
	}
	if deepest >= 0 {
		s := d.sections[deepest]
		out = append(out, d.insertKey(deepest, renderDottedKey(p.Keys[len(s.rel):])+" = "+value))
	}
	container := d.containerSection(p)
	if len(p.Keys) > 1 {
		header := renderDottedKey(p.Keys[:len(p.Keys)-1])
		leaf := renderDottedKey(p.Keys[len(p.Keys)-1:]) + " = " + value
		if p.AgentID != "" {
			header = agentsKey + "." + header
			out = append(out, d.insertSection(d.agentRegionEnd(p.AgentID), header, leaf))
		} else {
			out = append(out, d.insertSection(newSectionInsertAt(d.lines), header, leaf))
		}
	}
	if container >= 0 {
		out = append(out, d.insertKey(container, renderDottedKey(p.Keys)+" = "+value))
	}
	return out
}

// containerSection is the section holding p's container table: the agent's
// [[agents]] block, or the root table.
func (d *tomlDoc) containerSection(p KeyPath) int {
	if p.AgentID == "" {
		return 0
	}
	for si, s := range d.sections {
		if s.agentID == p.AgentID && len(s.rel) == 0 && s.array {
			return si
		}
	}
	return -1
}

// agentRegionEnd is the line after the agent's block and its sub-sections.
func (d *tomlDoc) agentRegionEnd(id string) int {
	end := -1
	for _, s := range d.sections {
		if s.agentID == id {
			end = s.end
		}
	}
	// Back off over trailing blank lines so the new table sits with the agent.
	for end > 0 && strings.TrimSpace(d.lines[end-1]) == "" {
		end--
	}
	return end
}

// insertKey inserts line into section si: after its last assignment, else at
// the end of its body before trailing blank lines.
func (d *tomlDoc) insertKey(si int, line string) string {
	s := d.sections[si]
	at := -1
	for _, k := range d.keys {
		if k.sec == si {
			at = k.end + 1
		}
	}
	if at < 0 {
		at = s.end
		for at > s.start+1 && strings.TrimSpace(d.lines[at-1]) == "" {
			at--
		}
		if at < 0 {
			at = 0
		}
	}
	lines := make([]string, 0, len(d.lines)+1)
	lines = append(lines, d.lines[:at]...)
	lines = append(lines, line)
	lines = append(lines, d.lines[at:]...)
	return strings.Join(lines, "\n")
}

// insertSection inserts a new [header] table holding line at index at, with
// a blank line on each side.
func (d *tomlDoc) insertSection(at int, header, line string) string {
	lines := insertSectionBlock(append([]string{}, d.lines...), at, header, []string{line})
	after := at + 2
	if at > 0 && strings.TrimSpace(d.lines[at-1]) != "" {
		after++
	}
	if after < len(lines) && strings.TrimSpace(lines[after]) != "" {
		lines = append(lines[:after], append([]string{""}, lines[after:]...)...)
	}
	return strings.Join(lines, "\n")
}

// ---- reading and verifying ----

// DecodeTOMLValue decodes a TOML value text (the value half of key = value).
func DecodeTOMLValue(value string) (any, error) {
	var m map[string]any
	if _, err := toml.Decode("v = "+strings.TrimSpace(value), &m); err != nil {
		return nil, fmt.Errorf("value %q is not a TOML value: %w", value, err)
	}
	return m["v"], nil
}

// decodeDoc decodes content into a generic map.
func decodeDoc(content string) (map[string]any, error) {
	var m map[string]any
	if _, err := toml.Decode(content, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// containerOf returns the map holding p's keys: the agent entry or root.
func containerOf(doc map[string]any, p KeyPath) (map[string]any, error) {
	if p.AgentID == "" {
		return doc, nil
	}
	agents, _ := doc[agentsKey].([]map[string]any)
	for _, a := range agents {
		if id, _ := a["id"].(string); id == p.AgentID {
			return a, nil
		}
	}
	return nil, fmt.Errorf("agent %q not found", p.AgentID)
}

// lookupIn walks keys from m.
func lookupIn(m map[string]any, keys []string) (any, bool) {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// deleteIn removes keys from m, then prunes the tables along the path that
// the removal left empty.
func deleteIn(m map[string]any, keys []string) {
	if len(keys) == 0 {
		return
	}
	if len(keys) == 1 {
		delete(m, keys[0])
		return
	}
	child, ok := m[keys[0]].(map[string]any)
	if !ok {
		return
	}
	deleteIn(child, keys[1:])
	if len(child) == 0 {
		delete(m, keys[0])
	}
}

// LookupPath returns the decoded value at p in content, and whether it is set.
func LookupPath(content string, p KeyPath) (any, bool, error) {
	doc, err := decodeDoc(content)
	if err != nil {
		return nil, false, fmt.Errorf("parse config: %w", err)
	}
	c, err := containerOf(doc, p)
	if err != nil {
		return nil, false, err
	}
	v, ok := lookupIn(c, p.Keys)
	if !ok || p.Elem == nil {
		return v, ok, nil
	}
	arr, isArr := asArray(v)
	if !isArr {
		return nil, false, fmt.Errorf("%s is not an array, so it has no element %s", p.Array(), p.Elem)
	}
	k, err := resolveElem(arr, *p.Elem, p.Array())
	if err != nil {
		return nil, false, err
	}
	if len(p.Sub) == 0 {
		return arr[k], true, nil
	}
	m, isTable := arr[k].(map[string]any)
	if !isTable {
		return nil, false, fmt.Errorf("%s is not a table, so it has no key %s", p.Element(), renderDottedKey(p.Sub))
	}
	v, ok = lookupIn(m, p.Sub)
	return v, ok, nil
}

// VerifyPathEdit checks that next differs from cur ONLY at p and, for a set,
// holds want there (for an unset: p is absent). It is the guard every path
// edit passes before it is offered for approval and again before it is
// written.
func VerifyPathEdit(cur, next string, p KeyPath, want any, unset bool) error {
	before, err := decodeDoc(cur)
	if err != nil {
		return fmt.Errorf("current config does not parse: %w", err)
	}
	after, err := decodeDoc(next)
	if err != nil {
		return fmt.Errorf("edited config does not parse: %w", err)
	}
	bc, err := containerOf(before, p)
	if err != nil {
		return err
	}
	ac, err := containerOf(after, p)
	if err != nil {
		return fmt.Errorf("the edit would lose %w", err)
	}
	got, ok := lookupIn(ac, p.Keys)
	switch {
	case unset && ok:
		return fmt.Errorf("%s is still set after the edit", p)
	case !unset && !ok:
		return fmt.Errorf("%s is not set after the edit", p)
	case !unset && !valuesEqual(got, want):
		return fmt.Errorf("%s would read %s, not %s", p, RenderTOMLValue(got), RenderTOMLValue(want))
	}
	deleteIn(bc, p.Keys)
	deleteIn(ac, p.Keys)
	if !reflect.DeepEqual(normValue(before), normValue(after)) {
		return fmt.Errorf("the edit would change something other than %s", p)
	}
	return nil
}

// RenderTOMLValue renders a decoded value as one line of TOML value text,
// for prompts and `foci config get`. A table renders as an inline table, an
// array of tables as an array of inline tables.
func RenderTOMLValue(v any) string {
	switch val := v.(type) {
	case nil:
		return "(unset)"
	case string:
		q, err := encodeTOMLBasicString(val)
		if err != nil {
			return fmt.Sprintf("%q", val)
		}
		return q
	case bool:
		return strconv.FormatBool(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'g', -1, 64)
	case time.Time:
		return val.Format(time.RFC3339Nano)
	case []map[string]any:
		parts := make([]string, len(val))
		for i, e := range val {
			parts[i] = RenderTOMLValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, len(val))
		for i, e := range val {
			parts[i] = RenderTOMLValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = renderDottedKey([]string{k}) + " = " + RenderTOMLValue(val[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprintf("%v", val)
	}
}

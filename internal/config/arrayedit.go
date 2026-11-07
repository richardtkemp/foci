package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// arrayedit.go edits ONE element of a TOML array in foci.toml as text (#2938),
// so every other element keeps its layout and the comments between elements
// survive. Two written forms are handled:
//
//   - an inline array, `key = [ ... ]`, on one line or many (inlineArray);
//   - an array of tables, one `[[path]]` section per element (tableArray).
//
// Both implement arrayEditor (remove, insert, replace one element by index).
// applyArrayDiff drives an editor from an old and a new element list, so a
// whole-array set changes only the elements that differ. Every caller
// verifies the decoded result; an edit these helpers get wrong is refused,
// never written.
//
// Comment rule: removing an element also removes the full-line comments
// directly above it (no blank line between, after the previous element) and
// a comment on its own last line — they describe that element. Every other
// comment stays. droppedComments lets a caller tell the user which comments
// an edit removed.

// ---- scanning ----

// scanStringEnd returns the exclusive end of the TOML string starting at s[i]
// (basic, literal, or either multi-line form).
func scanStringEnd(s string, i int) (int, error) {
	q := s[i]
	if quoteRun(s, i, q) >= 3 {
		for j := i + 3; j < len(s); j++ {
			if q == '"' && s[j] == '\\' {
				j++
				continue
			}
			if s[j] == q {
				if n := quoteRun(s, j, q); n >= 3 {
					return j + n, nil
				}
			}
		}
		return 0, errors.New("unterminated multi-line string")
	}
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '\n':
			return 0, errors.New("unterminated string")
		case q == '"' && s[j] == '\\':
			j++
		case s[j] == q:
			return j + 1, nil
		}
	}
	return 0, errors.New("unterminated string")
}

// isDateStart reports whether s[i:] starts with a YYYY-MM-DD date, whose
// time part TOML lets follow a space.
func isDateStart(s string, i int) bool {
	if len(s)-i < 10 {
		return false
	}
	for k, c := range []byte(s[i : i+10]) {
		if k == 4 || k == 7 {
			if c != '-' {
				return false
			}
		} else if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// scanValueEnd returns the exclusive end of the TOML value starting at s[i].
func scanValueEnd(s string, i int) (int, error) {
	if i >= len(s) {
		return 0, errors.New("missing value")
	}
	switch s[i] {
	case '"', '\'':
		return scanStringEnd(s, i)
	case '[', '{':
		depth := 0
		for j := i; j < len(s); {
			switch s[j] {
			case '"', '\'':
				e, err := scanStringEnd(s, j)
				if err != nil {
					return 0, err
				}
				j = e
				continue
			case '#':
				j = lineEnd(s, j)
				continue
			case '[', '{':
				depth++
			case ']', '}':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
			j++
		}
		return 0, errors.New("unterminated array or inline table")
	}
	j := i
	for j < len(s) && !strings.ContainsRune(",]}# \t\r\n", rune(s[j])) {
		j++
	}
	// A date-time may use a space between date and time.
	if isDateStart(s, i) && j-i == 10 && j+1 < len(s) && s[j] == ' ' && s[j+1] >= '0' && s[j+1] <= '9' {
		j++
		for j < len(s) && !strings.ContainsRune(",]}# \t\r\n", rune(s[j])) {
			j++
		}
	}
	if j == i {
		return 0, fmt.Errorf("unexpected %q in value", s[i])
	}
	return j, nil
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// skipSpaceAndComments skips whitespace, newlines and comments.
func skipSpaceAndComments(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '#':
			i = lineEnd(s, i)
		default:
			return i
		}
	}
	return i
}

// lineStart is the index of the first byte of the line holding s[i].
func lineStart(s string, i int) int { return strings.LastIndexByte(s[:i], '\n') + 1 }

// lineEnd is the index of the '\n' ending the line holding s[i], or len(s).
func lineEnd(s string, i int) int {
	if n := strings.IndexByte(s[i:], '\n'); n >= 0 {
		return i + n
	}
	return len(s)
}

// blankBefore reports whether only spaces and tabs precede s[i] on its line.
func blankBefore(s string, i int) bool {
	return strings.TrimLeft(s[lineStart(s, i):i], " \t") == ""
}

// indentAt is the leading whitespace of the line holding s[i].
func indentAt(s string, i int) string {
	ls := lineStart(s, i)
	return s[ls:skipSpace(s, ls)]
}

// elemSpan is one array element: s[start:end], and the index of the comma
// after it (-1 when none).
type elemSpan struct{ start, end, comma int }

// arrText is a scanned inline array: '[' at open, ']' at close.
type arrText struct {
	open, close int
	elems       []elemSpan
}

// parseArrayText scans the inline array whose '[' is s[open].
func parseArrayText(s string, open int) (arrText, error) {
	if open >= len(s) || s[open] != '[' {
		return arrText{}, errors.New("not an array")
	}
	t := arrText{open: open}
	j := open + 1
	for {
		j = skipSpaceAndComments(s, j)
		if j >= len(s) {
			return arrText{}, errors.New("unterminated array")
		}
		if s[j] == ']' {
			t.close = j
			return t, nil
		}
		end, err := scanValueEnd(s, j)
		if err != nil {
			return arrText{}, err
		}
		e := elemSpan{start: j, end: end, comma: -1}
		j = skipSpaceAndComments(s, end)
		if j < len(s) && s[j] == ',' {
			e.comma = j
			j++
		} else if j >= len(s) || s[j] != ']' {
			return arrText{}, errors.New("expected ',' or ']' in array")
		}
		t.elems = append(t.elems, e)
	}
}

// arrayElemTexts returns the text of each element of an array value text.
func arrayElemTexts(value string) ([]string, error) {
	v := strings.TrimSpace(value)
	t, err := parseArrayText(v, 0)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(t.elems))
	for i, e := range t.elems {
		out[i] = v[e.start:e.end]
	}
	return out, nil
}

// kvText is one key = value of an inline table, as written.
type kvText struct {
	keys    []string
	keyText string
	value   string
}

// splitInlineTable splits an inline table's text into its key = value pairs,
// in written order.
func splitInlineTable(text string) ([]kvText, error) {
	t := strings.TrimSpace(text)
	if t == "" || t[0] != '{' {
		return nil, errors.New("not an inline table")
	}
	if end, err := scanValueEnd(t, 0); err != nil || end != len(t) {
		return nil, errors.New("not one inline table")
	}
	var out []kvText
	j := 1
	for {
		j = skipSpaceAndComments(t, j)
		if j >= len(t) {
			return nil, errors.New("unterminated inline table")
		}
		if t[j] == '}' {
			return out, nil
		}
		keys, rest, err := parseDottedKey(t[j:])
		if err != nil {
			return nil, err
		}
		kEnd := len(t) - len(rest)
		if !strings.HasPrefix(rest, "=") {
			return nil, errors.New("expected '=' in inline table")
		}
		vs := skipSpace(t, kEnd+1)
		ve, err := scanValueEnd(t, vs)
		if err != nil {
			return nil, err
		}
		out = append(out, kvText{keys: keys, keyText: strings.TrimSpace(t[j:kEnd]), value: t[vs:ve]})
		j = skipSpaceAndComments(t, ve)
		if j < len(t) && t[j] == ',' {
			j++
		}
	}
}

// valueStartCol is the index just after the '=' of the key = value on line.
func valueStartCol(line string) (int, error) {
	trimmed := strings.TrimLeft(line, " \t")
	_, rest, err := parseDottedKey(trimmed)
	if err != nil || !strings.HasPrefix(rest, "=") {
		return 0, errors.New("not a key = value line")
	}
	return len(line) - len(rest) + 1, nil
}

// tomlComments lists the comments in content (each trimmed, '#' included),
// skipping '#' inside strings.
func tomlComments(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		switch s[i] {
		case '#':
			e := lineEnd(s, i)
			out = append(out, strings.TrimSpace(s[i:e]))
			i = e
		case '"', '\'':
			e, err := scanStringEnd(s, i)
			if err != nil {
				i++
				continue
			}
			i = e
		default:
			i++
		}
	}
	return out
}

// DroppedComments lists the comments in cur that next no longer has.
func DroppedComments(cur, next string) []string {
	have := map[string]int{}
	for _, c := range tomlComments(next) {
		have[c]++
	}
	var out []string
	for _, c := range tomlComments(cur) {
		if have[c] > 0 {
			have[c]--
			continue
		}
		out = append(out, c)
	}
	return out
}

// ---- values ----

// normValue makes decoded TOML comparable whatever its written form: an
// array of tables decodes as []map[string]any, an inline array as []any.
func normValue(v any) any {
	switch t := v.(type) {
	case []map[string]any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normValue(e)
		}
		return out
	}
	return v
}

func valuesEqual(a, b any) bool { return reflect.DeepEqual(normValue(a), normValue(b)) }

// asArray returns v's elements when v is an array.
func asArray(v any) ([]any, bool) {
	n, ok := normValue(v).([]any)
	return n, ok
}

// ---- editors ----

// arrayEditor changes one element of an array by index.
type arrayEditor interface {
	remove(k int) error
	insert(k int, text string) error // before element k; k == len appends
	replace(k int, text string) error
	result() string
}

// applyArrayDiff turns the editor's array from old into nw with the fewest
// element edits (a longest-common-subsequence diff): unchanged elements are
// not touched. texts are nw's element texts.
func applyArrayDiff(ed arrayEditor, old, nw []any, texts []string) error {
	n, m := len(old), len(nw)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if valuesEqual(old[i], nw[j]) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j, k := 0, 0, 0
	for i < n || j < m {
		var err error
		switch {
		case i < n && j < m && valuesEqual(old[i], nw[j]):
			i, j, k = i+1, j+1, k+1
			continue
		case i < n && j < m && lcs[i+1][j+1] == lcs[i][j] && elemIdentity(old[i]) == elemIdentity(nw[j]):
			err = ed.replace(k, texts[j])
			i, j, k = i+1, j+1, k+1
		case j >= m || (i < n && lcs[i+1][j] >= lcs[i][j+1]):
			err = ed.remove(k)
			i++
		default:
			err = ed.insert(k, texts[j])
			j, k = j+1, k+1
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// elemIdentity is the name (else id) of a table element, "" for anything
// else. applyArrayDiff edits an element in place only when old and new share
// it, so a replaced element keeps its comments and a different one does not
// inherit them.
func elemIdentity(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"name", "id"} {
		if s, ok := m[k].(string); ok {
			return k + "=" + s
		}
	}
	return ""
}

// attachedCommentsFrom walks up from the line starting at ls over full-line
// comments (no blank line between) that start at or after boundary, and
// returns where the first of them starts (ls when there are none).
func attachedCommentsFrom(s string, ls, boundary int) int {
	for ls > 0 {
		ps := lineStart(s, ls-1)
		if ps < boundary || !strings.HasPrefix(strings.TrimSpace(s[ps:ls-1]), "#") {
			break
		}
		ls = ps
	}
	return ls
}

// inlineArray edits an inline array held in s (an assignment's full lines);
// its '[' is at s[open].
type inlineArray struct {
	s    string
	open int
}

func (a *inlineArray) result() string { return a.s }

func (a *inlineArray) elem(k int) (arrText, elemSpan, error) {
	t, err := parseArrayText(a.s, a.open)
	if err != nil {
		return t, elemSpan{}, err
	}
	if k < 0 || k >= len(t.elems) {
		return t, elemSpan{}, fmt.Errorf("element %d out of range", k)
	}
	return t, t.elems[k], nil
}

func (a *inlineArray) replace(k int, text string) error {
	_, e, err := a.elem(k)
	if err != nil {
		return err
	}
	a.s = a.s[:e.start] + strings.TrimSpace(text) + a.s[e.end:]
	return nil
}

func (a *inlineArray) remove(k int) error {
	t, e, err := a.elem(k)
	if err != nil {
		return err
	}
	after := e.end
	if e.comma >= 0 {
		after = e.comma + 1
	}
	p := skipSpace(a.s, after)
	if p < len(a.s) && a.s[p] == '#' {
		p = lineEnd(a.s, p)
	}
	if blankBefore(a.s, e.start) && p < len(a.s) && a.s[p] == '\n' {
		// The element has its own lines: drop them, with the comments
		// directly above it.
		boundary := a.open + 1
		if k > 0 {
			boundary = t.elems[k-1].end
		}
		from := attachedCommentsFrom(a.s, lineStart(a.s, e.start), boundary)
		a.s = a.s[:from] + a.s[p+1:]
		return nil
	}
	pos := e.start
	if k == len(t.elems)-1 && e.comma < 0 && k > 0 && !strings.Contains(a.s[t.elems[k-1].end:e.start], "#") {
		pos = t.elems[k-1].end // also drop the comma before it: ["a", "b"] -> ["a"]
		a.s = a.s[:pos] + a.s[e.end:]
	} else {
		a.s = a.s[:e.start] + a.s[skipSpace(a.s, after):]
	}
	// A line the removal left blank goes too.
	if ls, le := lineStart(a.s, pos), lineEnd(a.s, pos); le < len(a.s) && ls > a.open && strings.TrimSpace(a.s[ls:le]) == "" {
		a.s = a.s[:ls] + a.s[le+1:]
	}
	return nil
}

func (a *inlineArray) insert(k int, text string) error {
	text = strings.TrimSpace(text)
	t, err := parseArrayText(a.s, a.open)
	if err != nil {
		return err
	}
	n := len(t.elems)
	if k < 0 || k > n {
		return fmt.Errorf("element %d out of range", k)
	}
	ownLineClose := strings.Contains(a.s[a.open:t.close], "\n") && blankBefore(a.s, t.close)
	switch {
	case n == 0 && ownLineClose:
		ls := lineStart(a.s, t.close)
		a.s = a.s[:ls] + indentAt(a.s, t.close) + "  " + text + ",\n" + a.s[ls:]
	case n == 0:
		a.s = a.s[:t.close] + text + a.s[t.close:]
	case k == n && ownLineClose:
		last := t.elems[n-1]
		ind := indentAt(a.s, t.close) + "  "
		if blankBefore(a.s, last.start) {
			ind = indentAt(a.s, last.start)
		}
		line := ind + text
		if last.comma >= 0 {
			line += ","
		}
		ls := lineStart(a.s, t.close)
		s := a.s[:ls] + line + "\n" + a.s[ls:]
		if last.comma < 0 {
			s = s[:last.end] + "," + s[last.end:]
		}
		a.s = s
	case k == n:
		last := t.elems[n-1]
		if last.comma >= 0 {
			a.s = a.s[:last.comma+1] + " " + text + "," + a.s[last.comma+1:]
		} else {
			a.s = a.s[:last.end] + ", " + text + a.s[last.end:]
		}
	default:
		e := t.elems[k]
		if blankBefore(a.s, e.start) {
			boundary := a.open + 1
			if k > 0 {
				boundary = t.elems[k-1].end
			}
			from := attachedCommentsFrom(a.s, lineStart(a.s, e.start), boundary)
			a.s = a.s[:from] + indentAt(a.s, e.start) + text + ",\n" + a.s[from:]
		} else {
			a.s = a.s[:e.start] + text + ", " + a.s[e.start:]
		}
	}
	return nil
}

// inlineAssignment is an inlineArray inside a whole file: lines[start..end]
// is the assignment.
type inlineAssignment struct {
	lines      []string
	start, end int
	arr        *inlineArray
}

func newInlineAssignment(lines []string, start, end int) (*inlineAssignment, error) {
	text := strings.Join(lines[start:end+1], "\n")
	col, err := valueStartCol(lines[start])
	if err != nil {
		return nil, err
	}
	open := skipSpace(text, col)
	if open >= len(text) || text[open] != '[' {
		return nil, errors.New("the value is not written as an array")
	}
	if _, err := parseArrayText(text, open); err != nil {
		return nil, err
	}
	return &inlineAssignment{lines: lines, start: start, end: end, arr: &inlineArray{s: text, open: open}}, nil
}

func (a *inlineAssignment) remove(k int) error              { return a.arr.remove(k) }
func (a *inlineAssignment) insert(k int, text string) error { return a.arr.insert(k, text) }
func (a *inlineAssignment) replace(k int, t string) error   { return a.arr.replace(k, t) }

// spanLines returns the assignment's edited lines.
func (a *inlineAssignment) spanLines() []string { return strings.Split(a.arr.s, "\n") }

func (a *inlineAssignment) result() string {
	out := make([]string, 0, len(a.lines))
	out = append(out, a.lines[:a.start]...)
	out = append(out, a.spanLines()...)
	out = append(out, a.lines[a.end+1:]...)
	return strings.Join(out, "\n")
}

// diffArrayAssignment rewrites the inline-array assignment lines[start..end]
// to value with the fewest element edits. ok is false when either side is
// not an array or the edit does not read back as value.
func diffArrayAssignment(lines []string, start, end int, value string) ([]string, bool) {
	a, err := newInlineAssignment(lines, start, end)
	if err != nil {
		return nil, false
	}
	col, _ := valueStartCol(lines[start])
	oldV, err := DecodeTOMLValue(a.arr.s[col:])
	if err != nil {
		return nil, false
	}
	newV, err := DecodeTOMLValue(value)
	if err != nil {
		return nil, false
	}
	oldArr, ok1 := asArray(oldV)
	newArr, ok2 := asArray(newV)
	texts, err := arrayElemTexts(value)
	if !ok1 || !ok2 || err != nil || len(texts) != len(newArr) {
		return nil, false
	}
	if err := applyArrayDiff(a, oldArr, newArr, texts); err != nil {
		return nil, false
	}
	got, err := DecodeTOMLValue(a.arr.s[col:])
	if err != nil || !valuesEqual(got, newV) {
		return nil, false
	}
	return a.spanLines(), true
}

// assignTrailingComment returns " # ..." when the assignment at
// lines[start..end] ends in a comment after its value.
func assignTrailingComment(lines []string, start, end int) string {
	_, old, err := valueSpan(lines, start)
	if err != nil {
		return ""
	}
	oldLines := strings.Split(strings.TrimSpace(old), "\n")
	lastVal := strings.TrimSpace(oldLines[len(oldLines)-1])
	last := lines[end]
	if end == start {
		col, err := valueStartCol(last)
		if err != nil {
			return ""
		}
		last = last[col:]
	}
	idx := strings.LastIndex(last, lastVal)
	if lastVal == "" || idx < 0 {
		return ""
	}
	if tail := strings.TrimSpace(last[idx+len(lastVal):]); strings.HasPrefix(tail, "#") {
		return " " + tail
	}
	return ""
}

// rewriteAssignment sets the assignment at lines[start..end] to value. An
// array changed into an array is edited element by element (unchanged
// elements and the comments between them stay); anything else becomes one
// line that keeps the line's indent and its trailing comment.
func rewriteAssignment(lines []string, start, end int, keyText, value string) []string {
	if span, ok := diffArrayAssignment(lines, start, end, value); ok {
		out := make([]string, 0, len(lines))
		out = append(out, lines[:start]...)
		out = append(out, span...)
		return append(out, lines[end+1:]...)
	}
	indent := lines[start][:len(lines[start])-len(strings.TrimLeft(lines[start], " \t"))]
	line := indent + keyText + " = " + strings.TrimSpace(value) + assignTrailingComment(lines, start, end)
	return replaceSpan(append([]string{}, lines...), start, end, line)
}

// tableArray edits an array of tables written as [[path]] sections.
type tableArray struct {
	content string
	p       KeyPath
}

// aotBlock is one [[path]] element: its section, header line and the
// exclusive end of its lines (its sub-tables included).
type aotBlock struct {
	sec        int
	start, end int
	sub        bool // has sub-tables
}

func (e *tableArray) result() string { return e.content }

func (e *tableArray) blocks() (*tomlDoc, []aotBlock, error) {
	d, err := parseDoc(e.content)
	if err != nil {
		return nil, nil, err
	}
	var out []aotBlock
	open := false
	for si, s := range d.sections {
		if s.start < 0 || !s.inContainer(e.p) {
			open = false
			continue
		}
		switch {
		case s.array && equalKeys(s.rel, e.p.Keys):
			out = append(out, aotBlock{sec: si, start: s.start, end: s.end})
			open = true
		case open && hasPrefix(s.rel, e.p.Keys) && len(s.rel) > len(e.p.Keys):
			out[len(out)-1].end = s.end
			out[len(out)-1].sub = true
		default:
			open = false
		}
	}
	return d, out, nil
}

// headerLine is the [[path]] header of block b without a trailing comment.
func headerLine(lines []string, b aotBlock) string {
	l := lines[b.start]
	if i := strings.Index(l, "]]"); i >= 0 {
		return l[:i+2]
	}
	return strings.TrimSpace(l)
}

// blockLines renders an inline table's text as key = value lines.
func blockLines(text string) ([]string, error) {
	kvs, err := splitInlineTable(text)
	if err != nil {
		return nil, errors.New("an element of an array of tables must be an inline table, e.g. {name = \"x\"}")
	}
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i] = kv.keyText + " = " + kv.value
	}
	return out, nil
}

func (e *tableArray) block(k int) (*tomlDoc, []aotBlock, error) {
	d, bs, err := e.blocks()
	if err != nil {
		return nil, nil, err
	}
	if k < 0 || k >= len(bs) {
		return nil, nil, fmt.Errorf("element %d out of range", k)
	}
	return d, bs, nil
}

// attachedCommentLine is the first line of the full-line comments directly
// above line i (i when there are none).
func attachedCommentLine(lines []string, i int) int {
	for i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#") {
		i--
	}
	return i
}

func (e *tableArray) remove(k int) error {
	d, bs, err := e.block(k)
	if err != nil {
		return err
	}
	b := bs[k]
	from := attachedCommentLine(d.lines, b.start)
	to := blockRemovalEnd(d.lines, b.start, b.end)
	out := append(append([]string{}, d.lines[:from]...), d.lines[to:]...)
	e.content = strings.Join(out, "\n")
	return nil
}

func (e *tableArray) insert(k int, text string) error {
	body, err := blockLines(text)
	if err != nil {
		return err
	}
	d, bs, err := e.blocks()
	if err != nil {
		return err
	}
	if len(bs) == 0 || k < 0 || k > len(bs) {
		return fmt.Errorf("element %d out of range", k)
	}
	lines := d.lines
	block := append([]string{headerLine(lines, bs[0])}, body...)
	var out []string
	if k < len(bs) {
		at := attachedCommentLine(lines, bs[k].start)
		out = append(out, lines[:at]...)
		out = append(out, block...)
		out = append(out, "")
		out = append(out, lines[at:]...)
	} else {
		last := bs[len(bs)-1]
		at := blockRemovalEnd(lines, last.start, last.end)
		for at > last.start+1 && strings.TrimSpace(lines[at-1]) == "" {
			at--
		}
		out = append(out, lines[:at]...)
		out = append(out, "")
		out = append(out, block...)
		if at < len(lines) && strings.TrimSpace(lines[at]) != "" {
			out = append(out, "")
		}
		out = append(out, lines[at:]...)
	}
	e.content = strings.Join(out, "\n")
	return nil
}

// replace keeps the element's header, and edits its key lines one by one
// when it has no sub-tables or dotted keys; otherwise it rewrites the body.
func (e *tableArray) replace(k int, text string) error {
	kvs, err := splitInlineTable(text)
	if err != nil {
		return errors.New("an element of an array of tables must be an inline table, e.g. {name = \"x\"}")
	}
	d, bs, err := e.block(k)
	if err != nil {
		return err
	}
	b := bs[k]
	var own []docKey
	simple := !b.sub
	for _, key := range d.keys {
		if key.sec == b.sec {
			own = append(own, key)
			if len(key.keys) != 1 {
				simple = false
			}
		}
	}
	newByName := map[string]kvText{}
	for _, kv := range kvs {
		if len(kv.keys) != 1 {
			simple = false
		}
		newByName[kv.keys[0]] = kv
	}
	lines := append([]string{}, d.lines...)
	if !simple {
		body, _ := blockLines(text)
		end := blockRemovalEnd(lines, b.start, b.end)
		for end > b.start+1 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		out := append(append(append([]string{}, lines[:b.start+1]...), body...), lines[end:]...)
		e.content = strings.Join(out, "\n")
		return nil
	}
	// New keys go after the last key line (or right under the header).
	at := b.start + 1
	if len(own) > 0 {
		at = own[len(own)-1].end + 1
	}
	oldNames := map[string]bool{}
	for _, key := range own {
		oldNames[key.keys[0]] = true
	}
	var added []string
	for _, kv := range kvs {
		if !oldNames[kv.keys[0]] {
			added = append(added, kv.keyText+" = "+kv.value)
		}
	}
	lines = append(lines[:at], append(added, lines[at:]...)...)
	for i := len(own) - 1; i >= 0; i-- {
		key := own[i]
		kv, ok := newByName[key.keys[0]]
		if !ok {
			lines = replaceSpan(lines, key.start, key.end, "")
			continue
		}
		oldV, err1 := DecodeTOMLValue(d.valueText(key))
		newV, err2 := DecodeTOMLValue(kv.value)
		if err1 == nil && err2 == nil && valuesEqual(oldV, newV) {
			continue
		}
		lines = rewriteAssignment(lines, key.start, key.end, key.keyText, kv.value)
	}
	e.content = strings.Join(lines, "\n")
	return nil
}

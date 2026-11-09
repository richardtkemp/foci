package config

import (
	"fmt"
	"strings"
)

// elementsubedit.go edits ONE key inside one table element of an array
// (#2991): `foci config set tts[groq-playai].max_chars 300`, `foci config
// unset tts[groq-playai].fallback`. It builds the element's new text from the
// element as written, changed only at that key, and hands it to the element
// replace (elementedit.go). The replace keeps the text of every key whose
// value is unchanged, and its result is verified like every element edit:
// the file may change only at the array, and only at that element. Before
// that, the new element must decode as the old one with exactly the key
// changed.

// planElementSubEdit sets (or, with unset, removes) the key p.Sub inside the
// element p.Elem picks.
func planElementSubEdit(content string, p KeyPath, value string, unset bool) (string, error) {
	arrP, elemP := p.Array(), p.Element()
	cur, set, err := LookupPath(content, arrP)
	if err != nil {
		return "", err
	}
	if !set {
		return "", fmt.Errorf("%s is not set in the config file", arrP)
	}
	arr, ok := asArray(cur)
	if !ok {
		return "", fmt.Errorf("%s is not an array, so it has no element %s", arrP, p.Elem)
	}
	k, err := resolveElem(arr, *p.Elem, arrP)
	if err != nil {
		return "", err
	}
	old, ok := arr[k].(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s is not a table, so it has no key %s", elemP, renderDottedKey(p.Sub))
	}

	want, _ := copyValue(old).(map[string]any)
	if unset {
		if _, ok := lookupIn(old, p.Sub); !ok {
			return "", fmt.Errorf("%s is not set in the config file", p)
		}
		deleteIn(want, p.Sub)
	} else {
		v, err := DecodeTOMLValue(value)
		if err != nil {
			return "", err
		}
		if err := setIn(want, p.Sub, v); err != nil {
			return "", fmt.Errorf("%s: %w", p, err)
		}
	}

	kvs, spaced, err := elementKVs(content, arrP, k)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	var lastErr error
	for _, cand := range subEditCandidates(kvs, p.Sub, strings.TrimSpace(value), unset, want) {
		text := renderInlineTable(cand, spaced)
		got, err := DecodeTOMLValue(text)
		if err != nil || !valuesEqual(got, want) {
			continue
		}
		next, err := planElementEdit(content, elemP, elemReplace, text)
		if err == nil {
			return next, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("cannot edit %s in place without changing anything else", p)
	}
	return "", lastErr
}

// elementKVs returns the key = value pairs of element k of the array at arrP
// as the file writes them, and whether the element is an inline table
// written with a space inside its braces.
func elementKVs(content string, arrP KeyPath, k int) ([]kvText, bool, error) {
	ed, err := arrayEditorFor(content, arrP)
	if err != nil {
		return nil, false, err
	}
	switch e := ed.(type) {
	case *inlineAssignment:
		t, err := parseArrayText(e.arr.s, e.arr.open)
		if err != nil {
			return nil, false, err
		}
		if k >= len(t.elems) {
			return nil, false, fmt.Errorf("element %d out of range", k)
		}
		raw := e.arr.s[t.elems[k].start:t.elems[k].end]
		kvs, err := splitInlineTable(raw)
		if err != nil {
			return nil, false, err
		}
		return kvs, strings.HasPrefix(raw, "{ "), nil
	case *tableArray:
		d, bs, err := e.block(k)
		if err != nil {
			return nil, false, err
		}
		b := bs[k]
		if b.sub {
			return nil, false, fmt.Errorf("element %d has sub-tables ([%s.…] sections), so one key in it cannot be edited; set the whole element instead", k, renderDottedKey(arrP.Keys))
		}
		var kvs []kvText
		for _, key := range d.keys {
			if key.sec != b.sec {
				continue
			}
			_, val, err := valueSpan(d.lines, key.start)
			if err != nil {
				return nil, false, err
			}
			kvs = append(kvs, kvText{keys: key.keys, keyText: key.keyText, value: strings.TrimSpace(val)})
		}
		return kvs, false, nil
	}
	return nil, false, fmt.Errorf("%s is not written as one array", arrP)
}

// subEditCandidates lists the element's key = value pairs after the edit,
// closest to the written text first:
//
//  1. the one pair at exactly sub gets the new value (or goes), or, when
//     nothing in the element is under sub's first key, sub is appended;
//  2. every pair under sub's first key is replaced by one pair holding that
//     key's whole new value (dotted pairs fold into an inline table).
//
// The caller keeps the first candidate that decodes as want.
func subEditCandidates(kvs []kvText, sub []string, value string, unset bool, want map[string]any) [][]kvText {
	var out [][]kvText
	exact, under, top := -1, 0, -1
	for i, kv := range kvs {
		if equalKeys(kv.keys, sub) {
			exact = i
		}
		if hasPrefix(kv.keys, sub) {
			under++
		}
		if kv.keys[0] == sub[0] && top < 0 {
			top = i
		}
	}
	switch {
	case exact >= 0 && under == 1:
		c := append([]kvText{}, kvs...)
		if unset {
			c = append(c[:exact], c[exact+1:]...)
		} else {
			c[exact].value = value
		}
		out = append(out, c)
	case top < 0 && !unset && len(sub) == 1:
		out = append(out, append(append([]kvText{}, kvs...), kvText{keys: sub, keyText: renderDottedKey(sub), value: value}))
	}

	// 2. Fold everything under sub[0] into one pair.
	at := len(kvs)
	if top >= 0 {
		at = top
	}
	var c []kvText
	for i, kv := range kvs {
		if i == at {
			c = append(c, foldedPair(kvs, sub, value, unset, want)...)
		}
		if kv.keys[0] != sub[0] {
			c = append(c, kv)
		}
	}
	if at == len(kvs) {
		c = append(c, foldedPair(kvs, sub, value, unset, want)...)
	}
	return append(out, c)
}

// foldedPair is the one pair holding sub[0]'s new value, or none when the
// edit leaves sub[0] absent.
func foldedPair(kvs []kvText, sub []string, value string, unset bool, want map[string]any) []kvText {
	v, ok := want[sub[0]]
	if !ok {
		return nil
	}
	keyText := renderDottedKey(sub[:1])
	for _, kv := range kvs {
		if len(kv.keys) == 1 && kv.keys[0] == sub[0] {
			keyText = kv.keyText
		}
	}
	text := RenderTOMLValue(v)
	if len(sub) == 1 && !unset {
		text = value // the user's text, not a re-rendering of it
	}
	return []kvText{{keys: sub[:1], keyText: keyText, value: text}}
}

// renderInlineTable writes key = value pairs as one inline table.
func renderInlineTable(kvs []kvText, spaced bool) string {
	if len(kvs) == 0 {
		return "{}"
	}
	parts := make([]string, len(kvs))
	for i, kv := range kvs {
		parts[i] = kv.keyText + " = " + kv.value
	}
	if spaced {
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// copyValue deep-copies decoded TOML.
func copyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = copyValue(e)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyValue(e)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, e := range x {
			out[i], _ = copyValue(e).(map[string]any)
		}
		return out
	}
	return v
}

// setIn sets keys in m to v, creating the tables along the path.
func setIn(m map[string]any, keys []string, v any) error {
	for i, k := range keys[:len(keys)-1] {
		child, ok := m[k]
		if !ok {
			n := map[string]any{}
			m[k] = n
			m = n
			continue
		}
		cm, ok := child.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not a table", renderDottedKey(keys[:i+1]))
		}
		m = cm
	}
	m[keys[len(keys)-1]] = v
	return nil
}

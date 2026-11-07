package config

import (
	"fmt"
	"strings"
)

// elementedit.go is the key-path half of the array element edits (#2938):
// `foci config set <array>[sel] <value>` replaces one element, `foci config
// unset <array>[sel]` removes one, `foci config add <array> <value>` appends
// one. Each plan is verified like every path edit (VerifyPathEdit): the file
// may change only at the array, and the array must read back as the old one
// with exactly that element changed.

type elemOp int

const (
	elemReplace elemOp = iota
	elemRemove
	elemAdd
)

// PlanAddElement returns content with value appended to the array at p. An
// absent array is created holding just value.
func PlanAddElement(content string, p KeyPath, value string) (string, error) {
	if p.Elem != nil {
		return "", fmt.Errorf("add appends to an array: name the array without a selector (%s), not %s", p.Array(), p)
	}
	return planElementEdit(content, p, elemAdd, value)
}

// arrayEditorFor returns an editor for the array at p as the file writes it:
// one inline-array assignment, or [[p]] sections.
func arrayEditorFor(content string, p KeyPath) (arrayEditor, error) {
	d, err := parseDoc(content)
	if err != nil {
		return nil, err
	}
	if p.AgentID != "" && !d.hasAgent(p.AgentID) {
		return nil, fmt.Errorf("agent %q not found in config file", p.AgentID)
	}
	pd := d.find(p)
	if len(pd.outer) > 0 {
		k := pd.outer[0]
		return nil, fmt.Errorf("%s is inside the value of %q (line %d); edit that key instead", p, k.keyText, k.start+1)
	}
	switch {
	case len(pd.exact) == 1 && len(pd.under) == 0 && len(pd.sections) == 0:
		k := pd.exact[0]
		a, err := newInlineAssignment(d.lines, k.start, k.end)
		if err != nil {
			return nil, fmt.Errorf("%s (line %d): %w", p, k.start+1, err)
		}
		return a, nil
	case len(pd.exact) == 0 && len(pd.under) == 0 && len(pd.sections) > 0 && d.sections[pd.sections[0]].array:
		return &tableArray{content: content, p: p}, nil
	}
	return nil, fmt.Errorf("%s is not written as one array (one key = [...] line, or [[...]] sections), so one element cannot be edited; set the whole value instead", p)
}

// planElementEdit replaces, removes or appends one element of the array p
// names.
func planElementEdit(content string, p KeyPath, op elemOp, value string) (string, error) {
	arrP := p.Array()
	cur, set, err := LookupPath(content, arrP)
	if err != nil {
		return "", err
	}
	if !set {
		if op == elemAdd {
			return PlanSetPath(content, arrP, "["+strings.TrimSpace(value)+"]")
		}
		return "", fmt.Errorf("%s is not set in the config file", arrP)
	}
	arr, ok := asArray(cur)
	if !ok {
		return "", fmt.Errorf("%s is not an array", arrP)
	}
	var elem any
	if op != elemRemove {
		if elem, err = DecodeTOMLValue(value); err != nil {
			return "", err
		}
	}
	k := len(arr)
	if op != elemAdd {
		if k, err = resolveElem(arr, *p.Elem, arrP); err != nil {
			return "", err
		}
	}
	want := append([]any{}, arr...)
	ed, err := arrayEditorFor(content, arrP)
	if err != nil {
		return "", err
	}
	switch op {
	case elemReplace:
		want[k] = elem
		err = ed.replace(k, value)
	case elemRemove:
		want = append(want[:k], want[k+1:]...)
		err = ed.remove(k)
	case elemAdd:
		want = append(want, elem)
		err = ed.insert(k, value)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	next := ed.result()
	verr := VerifyPathEdit(content, next, arrP, want, false)
	if verr != nil && len(want) == 0 {
		// Removing the last [[...]] section leaves no array at all.
		verr = VerifyPathEdit(content, next, arrP, nil, true)
	}
	if verr != nil {
		return "", fmt.Errorf("cannot edit %s in place without changing anything else: %w", p, verr)
	}
	return next, nil
}

// planArrayDiff is PlanSetPath's first candidate when an array is set to an
// array: only the elements that differ are edited. ok is false when the
// path is not an array written as one, or the value is not an array.
func planArrayDiff(content string, p KeyPath, want any, value string) (string, bool) {
	newArr, ok := asArray(want)
	if !ok {
		return "", false
	}
	cur, set, err := LookupPath(content, p)
	if err != nil || !set {
		return "", false
	}
	oldArr, ok := asArray(cur)
	if !ok {
		return "", false
	}
	texts, err := arrayElemTexts(value)
	if err != nil || len(texts) != len(newArr) {
		return "", false
	}
	ed, err := arrayEditorFor(content, p)
	if err != nil {
		return "", false
	}
	if err := applyArrayDiff(ed, oldArr, newArr, texts); err != nil {
		return "", false
	}
	return ed.result(), true
}

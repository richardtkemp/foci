package memsearch

import (
	"encoding/json"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// MinLineRunes is the shortest line the own-note index records or probes
// with (Dick's ruling, 2026-11-19 12:40: "lines of 40+ chars"). Shorter lines
// (a heading, "- ok") are too common to tell one writer from another.
const MinLineRunes = 40

// maxTextBytes and maxLinesPerText bound what one tool input or result adds
// to the index: a tool that reads a 50 MB transcript must not stall the
// stream reader or fill the database.
const (
	maxTextBytes    = 1 << 20
	maxLinesPerText = 5000
)

// quoted matches a double-quoted string (with backslash escapes) or a
// single-quoted one, for `echo "text" >> file` and `printf '%s\n' "text"`
// writes: the file gets the quoted text, not the command line.
var quoted = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|'([^']*)'`)

// ExtractLines returns the lines of text the index records: every line of
// MinLineRunes runes or more (trimmed), from every string in text when text
// is JSON (a tool's input: a Bash command with its heredoc, an Edit's
// new_string, a Write's content), else from text itself (a tool result, a
// codex command line). A quoted string of MinLineRunes or more inside a line
// is added as well (an `echo "…" >>` write), and so is a unified diff's added
// line without its "+" (a codex fileChange result). Order is kept,
// duplicates are not removed.
func ExtractLines(text string) []string {
	if len(text) > maxTextBytes {
		text = text[:maxTextBytes]
	}
	var out []string
	for _, s := range jsonStrings(text) {
		for _, l := range strings.Split(s, "\n") {
			if len(out) >= maxLinesPerText {
				return out
			}
			l = strings.TrimSpace(l)
			// A unified diff's added line ("+text", not the "+++ path"
			// header) wrote "text": a codex fileChange result is the diff.
			if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
				if a := strings.TrimSpace(l[1:]); utf8.RuneCountInString(a) >= MinLineRunes {
					out = append(out, a)
				}
			}
			if utf8.RuneCountInString(l) < MinLineRunes {
				continue
			}
			out = append(out, l)
			for _, m := range quoted.FindAllStringSubmatch(l, -1) {
				q := m[1]
				if q == "" {
					q = m[2]
				} else {
					q = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`").Replace(q)
				}
				q = strings.TrimSpace(q)
				if q != l && utf8.RuneCountInString(q) >= MinLineRunes {
					out = append(out, q)
				}
			}
		}
	}
	return out
}

// jsonStrings returns every string leaf of text parsed as a JSON object or
// array, or text itself when it is not one.
func jsonStrings(text string) []string {
	t := strings.TrimSpace(text)
	if t == "" || (t[0] != '{' && t[0] != '[' && t[0] != '"') {
		return []string{text}
	}
	var v any
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return []string{text}
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// HashLine is the index identity of one line, trimmed: 64-bit FNV-1a. An
// identity, not a security boundary: a false match needs a 64-bit collision
// with one of the family's few hundred thousand lines.
func HashLine(line string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.TrimSpace(line)))
	return int64(h.Sum64()) //nolint:gosec // G115: a hash's bits, reinterpreted as SQLite's signed INTEGER
}

// probeCount is how many lines of a hit are looked up; probeNeed of them
// must match (Dick's ruling 12:40: "2 of 3 probe lines").
const (
	probeCount = 3
	probeNeed  = 2
)

// Probes returns the lines of a memory-file hit to look up in the index: its
// probeCount longest distinct lines of MinLineRunes runes or more. The
// longest lines are the least likely to be shared by chance.
func Probes(text string) []string {
	seen := map[string]bool{}
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if utf8.RuneCountInString(l) < MinLineRunes || seen[l] {
			continue
		}
		seen[l] = true
		lines = append(lines, l)
	}
	sort.SliceStable(lines, func(i, j int) bool { return len(lines[i]) > len(lines[j]) })
	if len(lines) > probeCount {
		lines = lines[:probeCount]
	}
	return lines
}

// probesNeeded is how many of n probes must match for a hit to be own.
func probesNeeded(n int) int {
	if n < probeNeed {
		return n
	}
	return probeNeed
}

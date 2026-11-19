package memsearch

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Section is the part of a memory file around a search hit.
type Section struct {
	Heading string // the heading line's text, or "L<line>" when no heading precedes the hit
	Text    string
}

// sectionFileMax bounds how much of a memory file is read.
const sectionFileMax = 2 << 20

// highlighted matches a term the search index marked in a snippet: >term<.
var highlighted = regexp.MustCompile(`>([^<>]+)<`)

// ReadSection reads the file at path and returns the markdown section the
// hit's snippet is in: from the nearest heading above it to the next heading
// of the same or a higher level. A section longer than maxChars (runes; 0 =
// no cap) is cut to a window around the hit. The search CLI's snippet is
// ~110 characters; the judge needs the section (#3175 evidence).
func ReadSection(path, snippet string, maxChars int) (Section, error) {
	f, err := os.Open(path)
	if err != nil {
		return Section{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, sectionFileMax))
	if err != nil {
		return Section{}, err
	}
	content := string(data)
	if strings.TrimSpace(content) == "" {
		return Section{}, fmt.Errorf("%s is empty", path)
	}
	off := hitOffset(content, snippet)
	return sectionAt(content, off, maxChars), nil
}

// hitOffset finds where snippet's text is in content: the snippet without its
// markers and ellipses, else its first marked term (case-insensitive), else 0.
func hitOffset(content, snippet string) int {
	plain := strings.ReplaceAll(strings.ReplaceAll(snippet, ">", ""), "<", "")
	plain = strings.TrimSpace(strings.Trim(strings.TrimSpace(plain), "…"))
	if len(plain) >= 8 {
		if i := strings.Index(content, plain); i >= 0 {
			return i
		}
		// The first line of it, which a snippet spanning a heading split.
		if nl := strings.IndexByte(plain, '\n'); nl > 8 {
			if i := strings.Index(content, plain[:nl]); i >= 0 {
				return i
			}
		}
	}
	lower := strings.ToLower(content)
	for _, m := range highlighted.FindAllStringSubmatch(snippet, -1) {
		if i := strings.Index(lower, strings.ToLower(m[1])); i >= 0 {
			return i
		}
	}
	return 0
}

// headingLevel returns the level of a markdown ATX heading line (1-6), or 0.
func headingLevel(line string) int {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(line) || line[n] != ' ' {
		return 0
	}
	return n
}

// sectionAt returns the section of content containing byte offset off.
func sectionAt(content string, off, maxChars int) Section {
	type line struct {
		start, level int
		text         string
	}
	var lines []line
	pos := 0
	for _, l := range strings.SplitAfter(content, "\n") {
		trimmed := strings.TrimRight(l, "\r\n")
		lines = append(lines, line{start: pos, level: headingLevel(trimmed), text: trimmed})
		pos += len(l)
	}
	hitLine := 0
	for i, l := range lines {
		if l.start > off {
			break
		}
		hitLine = i
	}
	headIdx := -1
	for i := hitLine; i >= 0; i-- {
		if lines[i].level > 0 {
			headIdx = i
			break
		}
	}
	start, heading, level := 0, fmt.Sprintf("L%d", hitLine+1), 7
	if headIdx >= 0 {
		start = lines[headIdx].start
		heading = strings.TrimSpace(strings.TrimLeft(lines[headIdx].text, "#"))
		level = lines[headIdx].level
	}
	end := len(content)
	for i := hitLine + 1; i < len(lines); i++ {
		if lines[i].level > 0 && lines[i].level <= level {
			end = lines[i].start
			break
		}
	}
	text := strings.TrimSpace(content[start:end])
	if maxChars > 0 && utf8.RuneCountInString(text) > maxChars {
		text = window(content[start:end], off-start, maxChars)
	}
	return Section{Heading: heading, Text: text}
}

// window cuts s to maxChars runes around byte offset rel: a third before it,
// the rest after, marked with ellipses where cut.
func window(s string, rel, maxChars int) string {
	r := []rune(s)
	relRunes := utf8.RuneCountInString(s[:clamp(rel, len(s))])
	from := clamp(relRunes-maxChars/3, len(r))
	to := clamp(from+maxChars, len(r))
	if to-from < maxChars {
		from = clamp(to-maxChars, len(r))
	}
	out := strings.TrimSpace(string(r[from:to]))
	if from > 0 {
		out = "… " + out
	}
	if to < len(r) {
		out += " …"
	}
	return out
}

// clamp bounds v to [0, hi].
func clamp(v, hi int) int {
	if v < 0 {
		return 0
	}
	if v > hi {
		return hi
	}
	return v
}

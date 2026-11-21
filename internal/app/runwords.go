package app

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The run button on a chat code block (#2878, #2882, #3209). The server decides,
// when it sends a message, which code blocks' first command words can start a
// "!" command for the agent that owns the conversation, and the message frame
// carries those words (runnableWords). The app draws the button on a block whose
// own first word is in that list. It no longer asks the server per block: a
// failed ask used to leave a block without its button for as long as it stayed
// on screen (#3209).
//
// The list holds words, not block positions. A word's answer depends only on the
// word and the agent, so a word in the list is runnable wherever it appears in
// the message: the app's and the server's readings of the markdown need not agree
// on block boundaries. A block the server did not see gets no button, never a
// wrong one.

// runnableWords is the deduplicated list, in first-seen order, of the first
// command words of text's fenced code blocks that runnable accepts. Nil when
// none does, or runnable is nil.
func runnableWords(text string, runnable func(word string) bool) []string {
	if runnable == nil || !strings.Contains(text, "```") && !strings.Contains(text, "~~~") {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, blk := range fencedCodeBlocks(text) {
		word, ok := codeBlockRunWord(blk.code, blk.lang)
		if !ok || seen[word] {
			continue
		}
		seen[word] = true
		if runnable(word) {
			out = append(out, word)
		}
	}
	return out
}

// runnableWordsIn is runnableWords for c's agent: nil for an agent with no "!"
// support (or a test fake with none).
func (c *appConn) runnableWordsIn(text string) []string {
	if c == nil {
		return nil
	}
	ag, ok := c.agentRef.(userCommandAgent)
	if !ok {
		return nil
	}
	return runnableWords(text, ag.UserCommandRunnable)
}

type fencedBlock struct {
	lang string
	code string
}

// fencedCodeBlocks finds text's fenced code blocks (``` or ~~~, three or more).
// It reads liberally: a fence may sit in a list item or a blockquote, with any
// indent. A block with no closing fence runs to the end of the text, as in
// CommonMark. Reading more blocks than the app's renderer does is harmless (see
// the file comment); reading fewer would lose a button.
func fencedCodeBlocks(text string) []fencedBlock {
	lines := strings.Split(text, "\n")
	var out []fencedBlock
	for i := 0; i < len(lines); i++ {
		quoted, rest := fenceLinePrefix(lines[i], true)
		ch, n, info, ok := fenceOpen(rest)
		if !ok {
			continue
		}
		var body []string
		j := i + 1
		for ; j < len(lines); j++ {
			line := lines[j]
			if quoted {
				line = stripQuoteMarkers(line)
			}
			if _, inner := fenceLinePrefix(line, false); fenceCloses(inner, ch, n) {
				break
			}
			body = append(body, line)
		}
		out = append(out, fencedBlock{lang: info, code: strings.Join(body, "\n")})
		i = j
	}
	return out
}

// listMarker matches a list item's marker and the space after it.
var listMarker = regexp.MustCompile(`^(?:[-*+]|[0-9]{1,9}[.)])[ \t]+`)

// fenceLinePrefix strips a line's indent, blockquote markers and (when list) one
// list marker. quoted reports a blockquote marker.
func fenceLinePrefix(line string, list bool) (quoted bool, rest string) {
	rest = strings.TrimLeft(line, " \t")
	for strings.HasPrefix(rest, ">") {
		quoted = true
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	if list {
		if m := listMarker.FindString(rest); m != "" {
			rest = rest[len(m):]
		}
	}
	return quoted, rest
}

func stripQuoteMarkers(line string) string {
	rest := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(rest, ">") {
		return line
	}
	for strings.HasPrefix(rest, ">") {
		rest = rest[1:]
		if strings.HasPrefix(rest, " ") {
			rest = rest[1:]
		}
	}
	return rest
}

// fenceOpen reads an opening fence: the fence character, its length and the info
// string. A backtick fence's info string has no backtick (CommonMark).
func fenceOpen(s string) (ch byte, n int, info string, ok bool) {
	if len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return 0, 0, "", false
	}
	ch = s[0]
	for n < len(s) && s[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, "", false
	}
	info = strings.TrimSpace(s[n:])
	if ch == '`' && strings.ContainsRune(info, '`') {
		return 0, 0, "", false
	}
	return ch, n, info, true
}

func fenceCloses(s string, ch byte, n int) bool {
	k := 0
	for k < len(s) && s[k] == ch {
		k++
	}
	return k >= n && strings.TrimSpace(s[k:]) == ""
}

// --- the first command word: a port of the app's codeBlockRunWord (CodeBlockRun.kt) ---
// The two must agree: a block whose word the server reads differently from the
// app gets no button. CodeBlockRunTest.kt's cases are in runwords_test.go.

// shellLabels are the fence labels that say a block is shell (Dick's ruling). No
// label also qualifies; any other does not.
var shellLabels = map[string]bool{"bash": true, "sh": true, "zsh": true, "shell": true}

var shellInterpreters = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true}

// shellMetachars end a shell word when not quoted.
const shellMetachars = ";&|<>()"

var assignmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)

// codeBlockRunWord is the word the agent must be able to start for a block to
// get a run button, and false when the block's label rules it out or it has no
// command. Blank lines, comments and leading NAME=value assignments are skipped;
// a shebang naming another language rules the block out; `word(` is a function
// definition, not a command.
func codeBlockRunWord(code, lang string) (string, bool) {
	label := strings.ToLower(firstField(strings.TrimSpace(lang)))
	if label != "" && !shellLabels[label] {
		return "", false
	}
	script := strings.TrimSpace(code)
	if shebangNamesAnotherLanguage(script) {
		return "", false
	}
	i := 0
	for {
		i = skipBlanksAndComments(script, i)
		if i >= len(script) {
			return "", false
		}
		end, isAssign := assignmentEnd(script, i)
		if !isAssign {
			break
		}
		if end < 0 {
			return "", false
		}
		i = skipBlanks(script, end)
		// An assignment can end its own command: `FOO=1; ls`, `FOO=1 && ls`.
		if strings.HasPrefix(script[i:], "&&") {
			i += 2
		} else if strings.HasPrefix(script[i:], ";") && !strings.HasPrefix(script[i:], ";;") {
			i++
		}
	}
	if script[i] == '(' {
		return "(", true
	}
	end := i
	for end < len(script) {
		r, size := utf8.DecodeRuneInString(script[end:])
		if unicode.IsSpace(r) || strings.ContainsRune(shellMetachars, r) {
			break
		}
		end += size
	}
	word := script[i:end]
	if word == "" || strings.HasPrefix(script[end:], "(") {
		return "", false
	}
	return word, true
}

func firstField(s string) string {
	for i, r := range s {
		if unicode.IsSpace(r) {
			return s[:i]
		}
	}
	return s
}

func shebangNamesAnotherLanguage(script string) bool {
	rest, ok := strings.CutPrefix(script, "#!")
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(rest, "\n")
	words := strings.Fields(first)
	name := ""
	if len(words) > 0 {
		name = words[0][strings.LastIndexByte(words[0], '/')+1:]
	}
	if name == "env" {
		name = ""
		for _, w := range words[1:] {
			if !strings.HasPrefix(w, "-") && !strings.Contains(w, "=") {
				name = w
				break
			}
		}
	}
	return !shellInterpreters[name]
}

func skipBlanks(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\r':
			i++
		case strings.HasPrefix(s[i:], "\\\n"):
			i += 2
		default:
			return i
		}
	}
	return i
}

func skipBlanksAndComments(s string, i int) int {
	for i < len(s) {
		i = skipBlanks(s, i)
		if i >= len(s) {
			break
		}
		switch s[i] {
		case '\n':
			i++
		case '#':
			nl := strings.IndexByte(s[i:], '\n')
			if nl < 0 {
				return len(s)
			}
			i += nl + 1
		default:
			return i
		}
	}
	return i
}

// assignmentEnd reports where the NAME=value at start ends. isAssign false: not
// an assignment. A negative end: a value this reading cannot follow to its end,
// such as an unclosed quote.
func assignmentEnd(s string, start int) (end int, isAssign bool) {
	name := assignmentName.FindString(s[start:])
	if name == "" {
		return 0, false
	}
	i := start + len(name)
	depth := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\\':
			i += 2
		case c == '\'':
			cl := strings.IndexByte(s[i+1:], '\'')
			if cl < 0 {
				return -1, true
			}
			i += cl + 2
		case c == '"' || c == '`':
			i = closingQuote(s, i)
			if i < 0 {
				return -1, true
			}
			i++
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth == 0 {
				return i, true
			}
			depth--
			i++
		case depth > 0:
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if unicode.IsSpace(r) || strings.ContainsRune(shellMetachars, r) {
				return i, true
			}
			i += size
		}
	}
	if depth != 0 {
		return -1, true
	}
	return min(i, len(s)), true
}

// closingQuote is the index of the quote closing the one at open, or -1. A
// backslash escapes the next character.
func closingQuote(s string, open int) int {
	for i := open + 1; i < len(s); {
		switch s[i] {
		case '\\':
			i += 2
		case s[open]:
			return i
		default:
			i++
		}
	}
	return -1
}

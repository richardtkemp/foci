package pretool

import (
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/syntax"
)

// WordSpace stands in for whitespace INSIDE one shell word (a quoted
// argument) in the text Commands produces. Real spaces then only ever
// separate words, so `\S+` in a command pattern always means exactly one
// word, and text inside a quoted argument (a commit message, a todo) can
// never line up with a pattern the way a real command would.
const WordSpace = "␣"

// Commands parses script as bash and returns each simple command in it as
// text, in source order: its words joined by single spaces, with quoting
// removed.
//
//   - Every simple command is its own entry, however it is joined:
//     `cd /r && git add .` gives "cd /r" and "git add .". Commands inside
//     subshells, groups, if/for/while bodies and $(...) are included.
//   - Leading variable assignments (`X=1 git ...`) and redirections
//     (`2>/dev/null`, heredocs) are not words, so they do not appear.
//   - Quotes are removed: 'a' and "a" become a. Expansions keep their source
//     form, so "$HOME/x" becomes $HOME/x and $W stays $W.
//   - Whitespace inside one word becomes WordSpace.
//
// ok is false when the script does not parse as bash; command patterns then
// cannot match, and the call falls through to the normal permission flow.
func Commands(script string) (cmds []string, ok bool) {
	f, err := syntax.NewParser(syntax.KeepComments(false), syntax.Variant(syntax.LangBash)).
		Parse(strings.NewReader(script), "")
	if err != nil {
		return nil, false
	}
	pr := syntax.NewPrinter()
	syntax.Walk(f, func(n syntax.Node) bool {
		call, isCall := n.(*syntax.CallExpr)
		if !isCall || len(call.Args) == 0 {
			return true
		}
		words := make([]string, len(call.Args))
		for i, w := range call.Args {
			words[i] = wordText(pr, w)
		}
		cmds = append(cmds, strings.Join(words, " "))
		return true
	})
	return cmds, true
}

func wordText(pr *syntax.Printer, w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		partText(&b, pr, p, false)
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return []rune(WordSpace)[0]
		}
		return r
	}, b.String())
}

func partText(b *strings.Builder, pr *syntax.Printer, p syntax.WordPart, inDbl bool) {
	switch x := p.(type) {
	case *syntax.Lit:
		b.WriteString(unescape(x.Value, inDbl))
	case *syntax.SglQuoted:
		b.WriteString(x.Value)
	case *syntax.DblQuoted:
		for _, q := range x.Parts {
			partText(b, pr, q, true)
		}
	default:
		_ = pr.Print(b, p)
	}
}

// unescape drops the shell's backslash escapes from a literal. Outside
// double quotes a backslash escapes any character; inside, only $ ` " \ and
// newline.
func unescape(s string, inDbl bool) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (!inDbl || strings.IndexByte("$`\"\\\n", s[i+1]) >= 0) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

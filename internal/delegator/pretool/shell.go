package pretool

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/syntax"
)

// WordSpace stands in for whitespace INSIDE one shell word (a quoted
// argument) in Command.Text. Real spaces then only ever
// separate words, so `\S+` in a command pattern always means exactly one
// word, and text inside a quoted argument (a commit message, a todo) can
// never line up with a pattern the way a real command would.
const WordSpace = "␣"

// Script is a Bash command as the shell will run it (#2040): its simple
// commands, each with the facts rules decide on, taken from the parsed
// syntax tree rather than the raw text. Heredoc bodies, quoted strings and
// comments are data, so they contribute no commands and no operators.
type Script struct {
	Commands []Command
	// EndDir is the main shell's working directory after the script, in the
	// same form as Command.Dir: where the next call starts.
	EndDir      string
	EndDirKnown bool
}

// Command is one simple command in a Script.
type Command struct {
	// Text is what command patterns see: the words joined by single
	// spaces, quotes removed, whitespace inside a word shown as WordSpace.
	Text string
	// Args are the words as the program gets them (quotes removed, quoted
	// whitespace kept). A when-check gets them as $1...
	Args []string
	// Background: it runs asynchronously, in a statement ended by & (or
	// inside one), so the call can return before it finishes.
	Background bool
	// Subshell: it runs in a child shell (( ), $( ), <( ), a pipeline
	// element, or backgrounded), so a cd or variable it sets does not
	// persist into the main shell or the next call.
	Subshell bool
	// Output: its standard output reaches the tool result, that is, it is
	// not redirected (>&2 still counts), piped into another command or
	// captured by $( ) or <( ).
	Output bool
	// Dir is the directory it runs in, as far as the script shows: "" for
	// the call's cwd, a path relative to that cwd, or an absolute path, after
	// any earlier cd/pushd in the same shell. DirKnown is false once a cd
	// target is not static (cd "$W", cd -, popd). Branches are not
	// followed: an if's else sees a cd made in its then.
	Dir      string
	DirKnown bool
	// Op is the operator that joins it to the command before it: "&&",
	// "||", ";" (also a newline), "&" (the one before was backgrounded),
	// "|" or "|&", or "" for the first command. The first command of a
	// group, subshell or loop body takes the operator before that group;
	// an if's then-branch counts as "&&" and its else-branch as "||".
	Op string
	// Pipe is the Text of every command downstream of it in its pipeline,
	// in order; empty when its output is not piped.
	Pipe []string
}

// Parse parses script as bash and returns its simple commands, in source
// order, with their facts. Each command's Text is its words joined by single
// spaces, with quoting removed:
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
// ok is false when the script does not parse as bash; command patterns and
// fact filters then cannot match, and the call falls through to the normal
// permission flow.
func Parse(script string) (Script, bool) {
	f, err := syntax.NewParser(syntax.KeepComments(false), syntax.Variant(syntax.LangBash)).
		Parse(strings.NewReader(script), "")
	if err != nil {
		return Script{}, false
	}
	w := &walker{pr: syntax.NewPrinter(), home: os.Getenv("HOME")}
	main := &shellDir{known: true}
	w.stmts(f.Stmts, frame{dir: main, sink: toolSink})
	s := Script{Commands: make([]Command, len(w.cmds)), EndDir: main.path, EndDirKnown: main.known}
	for i, c := range w.cmds {
		c.cmd.Output = c.sink == toolSink
		if c.sink != nil && c.sink != toolSink {
			c.cmd.Pipe = w.downstream(c.sink)
		}
		s.Commands[i] = c.cmd
	}
	return s, true
}

// parsesAsBash reports whether script parses with the same parser Parse
// uses; config validation holds a rule's when-check to it.
func parsesAsBash(script string) error {
	_, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
	return err
}

// shellDir is one shell's working directory as the walk goes: path is ""
// (the call's cwd), relative to it, or absolute.
type shellDir struct {
	path  string
	known bool
}

func (d *shellDir) fork() *shellDir { c := *d; return &c }

// sink is where a command's standard output goes: toolSink (the tool
// result), a pipe (the commands after it in a pipeline), or nil (a file,
// /dev/null, a $( ) capture).
type sink struct {
	cmds []int // indexes of the commands that read the pipe
	next *sink // where the reading side's output goes in turn
}

var toolSink = &sink{}

// frame is the context a construct runs in.
type frame struct {
	dir        *shellDir
	sink       *sink
	subshell   bool
	background bool
}

// sub returns the frame for a child shell: its own copy of the directory.
func (f frame) sub() frame {
	f.dir = f.dir.fork()
	f.subshell = true
	return f
}

type walkedCmd struct {
	cmd  Command
	sink *sink
}

type walker struct {
	pr   *syntax.Printer
	home string
	cmds []walkedCmd
	// op is the operator in front of the next command to be recorded.
	op string
	// pipes collects the indexes of commands recorded while walking the
	// reading side of each open pipe.
	pipes []*sink
}

func (w *walker) stmts(list []*syntax.Stmt, f frame) {
	for i, st := range list {
		if i > 0 {
			w.op = ";"
			if list[i-1].Background || list[i-1].Coprocess {
				w.op = "&"
			}
		}
		w.stmt(st, f)
	}
}

func (w *walker) stmt(st *syntax.Stmt, f frame) {
	if st.Background || st.Coprocess {
		f = f.sub()
		f.background = true
	}
	for _, r := range st.Redirs {
		if redirectsStdout(r) {
			f.sink = nil
		}
	}
	if st.Cmd != nil {
		w.command(st.Cmd, f)
	}
	for _, r := range st.Redirs {
		w.words(r, f)
	}
}

func (w *walker) command(cmd syntax.Command, f frame) {
	switch x := cmd.(type) {
	case *syntax.CallExpr:
		w.call(x, f)
	case *syntax.BinaryCmd:
		switch x.Op {
		case syntax.Pipe, syntax.PipeAll:
			pipe := &sink{next: f.sink}
			left := f.sub()
			left.sink = pipe
			w.stmt(x.X, left)
			w.op = x.Op.String()
			w.pipes = append(w.pipes, pipe)
			w.stmt(x.Y, f.sub())
			w.pipes = w.pipes[:len(w.pipes)-1]
		default:
			w.stmt(x.X, f)
			w.op = x.Op.String()
			w.stmt(x.Y, f)
		}
	case *syntax.Subshell:
		w.stmts(x.Stmts, f.sub())
	case *syntax.Block:
		w.stmts(x.Stmts, f)
	case *syntax.IfClause:
		w.ifClause(x, f)
	case *syntax.WhileClause:
		w.stmts(x.Cond, f)
		w.op = "&&"
		if x.Until {
			w.op = "||"
		}
		w.stmts(x.Do, f)
	case *syntax.ForClause:
		w.words(x.Loop, f)
		w.stmts(x.Do, f)
	case *syntax.CaseClause:
		w.words(x.Word, f)
		for _, item := range x.Items {
			for _, p := range item.Patterns {
				w.words(p, f)
			}
			w.stmts(item.Stmts, f)
		}
	case *syntax.FuncDecl:
		// The body runs when the function is called, not here.
		body := f
		body.dir = f.dir.fork()
		w.stmt(x.Body, body)
	case *syntax.TimeClause:
		if x.Stmt != nil {
			w.stmt(x.Stmt, f)
		}
	case *syntax.CoprocClause:
		bg := f.sub()
		bg.background = true
		w.stmt(x.Stmt, bg)
	default:
		// [[ ]], (( )), declare/export, let: only words, which may hold
		// command substitutions.
		w.words(x, f)
	}
}

func (w *walker) ifClause(x *syntax.IfClause, f frame) {
	if len(x.Cond) > 0 {
		w.stmts(x.Cond, f)
		w.op = "&&"
	}
	w.stmts(x.Then, f)
	if x.Else != nil {
		w.op = "||"
		w.ifClause(x.Else, f)
	}
}

// words walks a node that holds only words (arguments, redirect targets,
// heredoc bodies, test and arithmetic expressions) for the command and
// process substitutions in it, each of which runs in a child shell. Any
// statement reached some other way is walked as a statement.
func (w *walker) words(n syntax.Node, f frame) {
	if n == nil {
		return
	}
	syntax.Walk(n, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CmdSubst:
			sub := f.sub()
			sub.sink = nil
			w.stmts(x.Stmts, sub)
			return false
		case *syntax.ProcSubst:
			sub := f.sub()
			sub.sink = nil
			w.stmts(x.Stmts, sub)
			return false
		case *syntax.Stmt:
			w.stmt(x, f)
			return false
		}
		return true
	})
}

func (w *walker) call(x *syntax.CallExpr, f frame) {
	for _, a := range x.Assigns {
		w.words(a, f)
	}
	if len(x.Args) == 0 {
		return
	}
	args := make([]string, len(x.Args))
	shown := make([]string, len(x.Args))
	for i, word := range x.Args {
		args[i] = wordText(w.pr, word)
		shown[i] = strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return []rune(WordSpace)[0]
			}
			return r
		}, args[i])
	}
	idx := len(w.cmds)
	w.cmds = append(w.cmds, walkedCmd{
		cmd: Command{
			Text:       strings.Join(shown, " "),
			Args:       args,
			Background: f.background,
			Subshell:   f.subshell,
			Dir:        f.dir.path,
			DirKnown:   f.dir.known,
			Op:         w.op,
		},
		sink: f.sink,
	})
	for _, p := range w.pipes {
		p.cmds = append(p.cmds, idx)
	}
	// Substitutions in the words run before the command itself, but are
	// listed after it, as Commands always has.
	for _, word := range x.Args {
		w.words(word, f)
	}
	w.chdir(x, f.dir)
}

// chdir applies a cd, pushd or popd to the shell's directory.
func (w *walker) chdir(x *syntax.CallExpr, d *shellDir) {
	name, ok := staticWord(x.Args[0], "")
	if !ok || (name != "cd" && name != "pushd" && name != "popd") {
		return
	}
	var target *syntax.Word
	for i, a := range x.Args[1:] {
		s, ok := staticWord(a, "")
		if ok && s == "--" {
			if i+2 < len(x.Args) {
				target = x.Args[i+2]
			}
			break
		}
		if ok && strings.HasPrefix(s, "-") && s != "-" {
			continue
		}
		target = a
		break
	}
	var dest string
	switch {
	case name == "popd":
		ok = false
	case target == nil && name == "cd":
		dest, ok = w.home, w.home != ""
	case target == nil:
		ok = false // pushd with no argument swaps the top two
	default:
		dest, ok = staticWord(target, w.home)
		ok = ok && dest != "-" && dest != ""
	}
	switch {
	case !ok:
		d.known = false
	case filepath.IsAbs(dest):
		d.path, d.known = filepath.Clean(dest), true
	case d.known:
		d.path = filepath.Join(d.path, dest)
		if d.path == "." {
			d.path = ""
		}
	}
}

// staticWord returns w's value when it is fixed text: literals and quoted
// literals, plus (when home is set) a leading unquoted ~ or ~/ and $HOME.
// ok is false for anything that depends on other expansions.
func staticWord(w *syntax.Word, home string) (string, bool) {
	var b strings.Builder
	for i, p := range w.Parts {
		if i == 0 && home != "" {
			if lit, isLit := p.(*syntax.Lit); isLit && (lit.Value == "~" || strings.HasPrefix(lit.Value, "~/")) {
				b.WriteString(home + unescape(lit.Value[1:], false))
				continue
			}
		}
		if !staticPart(&b, p, home, false) {
			return "", false
		}
	}
	return b.String(), true
}

func staticPart(b *strings.Builder, p syntax.WordPart, home string, inDbl bool) bool {
	switch x := p.(type) {
	case *syntax.Lit:
		b.WriteString(unescape(x.Value, inDbl))
	case *syntax.SglQuoted:
		if x.Dollar {
			return false
		}
		b.WriteString(x.Value)
	case *syntax.DblQuoted:
		for _, q := range x.Parts {
			if !staticPart(b, q, home, true) {
				return false
			}
		}
	case *syntax.ParamExp:
		if home == "" || x.Param == nil || x.Param.Value != "HOME" || x.Excl || x.Length || x.Width ||
			x.Index != nil || x.Slice != nil || x.Repl != nil || x.Exp != nil || x.Names != 0 {
			return false
		}
		b.WriteString(home)
	default:
		return false
	}
	return true
}

// redirectsStdout reports whether r sends fd 1 somewhere other than where
// it was going. A dup onto stderr (>&2) does not count: stderr reaches the
// tool result too.
func redirectsStdout(r *syntax.Redirect) bool {
	switch r.Op {
	case syntax.RdrAll, syntax.AppAll:
		return true
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.DplOut:
	default:
		return false
	}
	if r.N != nil && r.N.Value != "1" {
		return false
	}
	if r.Op == syntax.DplOut {
		to, ok := staticWord(r.Word, "")
		return !ok || to != "2"
	}
	return true
}

// downstream is the Text of every command reading s, and of every pipe
// after that.
func (w *walker) downstream(s *sink) []string {
	var out []string
	for ; s != nil && s != toolSink; s = s.next {
		for _, i := range s.cmds {
			out = append(out, w.cmds[i].cmd.Text)
		}
	}
	return out
}

func wordText(pr *syntax.Printer, w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		partText(&b, pr, p, false)
	}
	return b.String()
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

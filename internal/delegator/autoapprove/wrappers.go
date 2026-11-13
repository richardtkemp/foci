package autoapprove

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"foci/internal/execguard"
)

// ---------- Wrappers and shell interceptors (#2241) ----------
//
// A wrapper runs another command named in its own arguments. Three families
// are handled, each so that the checks judge the command that actually runs:
//
//   - runnerWrappers (command, exec, builtin, xargs) are UNWRAPPED: the inner
//     command gets every check a typed segment gets (wrappedCommandUnsafe).
//   - wrapperCommands (env, nice, sudo, …) refuse when they carry arguments
//     (commandNameUnsafe); their option grammars are too varied to unwrap.
//   - shellInterceptors (bash, sh, …) are unwrapped when the form is one
//     extractShellScript understands, and refused in every other form.
//
// Rule patterns still match the printed segment, so unwrapping can only take
// an approval away, never add one.

// literalWordText returns the shell-visible text of a wholly-literal word
// (wordIsLiteralText): quotes removed, unquoted backslash escapes stripped,
// $'…' decoded. `"eval"`, `'env'`, `l"s"` and `\ls` read as eval, env, ls
// and ls, so a quoted name is the same command as the unquoted one (#2241).
// ok is false for any word that is not wholly literal (a variable, a
// substitution), which keeps its #2228 handling.
func literalWordText(w *syntax.Word) (string, bool) {
	if w == nil || len(w.Parts) == 0 || !wordIsLiteralText(w) {
		return "", false
	}
	return resolveStaticWord(w, nil)
}

// shellOptionLetters are the single-letter shell options an interceptor may
// carry before its script and still be unwrapped. They are POSIX `set`
// letters, with the same meaning in every shell of shellInterceptors, and
// none of them reads a startup file, reads the script from stdin or changes
// how the script is parsed: errexit, nounset, xtrace, verbose, noexec,
// noglob (makes globs inert: the checker treats them as live, the safe
// direction) and noclobber. Every other letter refuses: -i and -l read
// startup files, -s reads the script from stdin, -a and -k export
// assignments the #2326 check does not see as exported, -O/+O set shopt
// options such as extglob that change parsing.
const shellOptionLetters = "euxvnfC"

// shellSetOptions are the `-o NAME` / `+o NAME` names accepted before the
// script, for the same reasons as shellOptionLetters (plus pipefail).
var shellSetOptions = map[string]bool{
	"errexit":   true,
	"nounset":   true,
	"pipefail":  true,
	"xtrace":    true,
	"verbose":   true,
	"noexec":    true,
	"noglob":    true,
	"noclobber": true,
}

// shellLongOptions are the long options accepted before the script: both
// only SKIP startup files. --rcfile, --init-file, --login, --posix,
// --version and every other long option refuse.
var shellLongOptions = map[string]bool{
	"--norc":      true,
	"--noprofile": true,
}

// extractShellScript reports whether ce is a shell interceptor (bash, sh, …)
// in a form whose script this package can check, and returns the script
// word. The command name is read from a wholly-literal word with quotes
// removed (commandBaseName), so `"bash" -c …` is the same as `bash -c …`.
//
// The shell reads all of its options first and the script is the first word
// after them (`bash -c -e S` and `bash -c -- S` both run S, measured in bash
// 5), so the parse walks every option word:
//
//   - `-c` (alone or in a bundle such as -ec) selects command mode;
//   - letters from shellOptionLetters, `-o`/`+o` with a shellSetOptions name
//     in the next word, and shellLongOptions are accepted;
//   - `--` or `-` ends the options;
//   - every option word must be wholly literal (`'-c'` reads as -c).
//
// The script is the first word after the options, and only in command mode.
// Any other shape — an unknown option, a non-literal option word, no -c (the
// script would come from a file or from stdin), -c with no script — returns
// false. The walker refuses an interceptor for which this returns false;
// interceptorWord tells the two cases apart.
func extractShellScript(ce *syntax.CallExpr) (*syntax.Word, bool) {
	if !shellInterceptors[commandBaseName(ce)] {
		return nil, false
	}
	cmdMode := false
	args := ce.Args
	i := 1
	for ; i < len(args); i++ {
		text, lit := literalWordText(args[i])
		if !lit {
			if cmdMode {
				return args[i], true // a non-literal script word
			}
			return nil, false // a non-literal option word, or a script file
		}
		if text == "--" || text == "-" {
			i++
			break
		}
		if len(text) < 2 || (text[0] != '-' && text[0] != '+') {
			if cmdMode {
				return args[i], true
			}
			return nil, false // a script file
		}
		if strings.HasPrefix(text, "--") {
			if !shellLongOptions[text] {
				return nil, false
			}
			continue
		}
		plus := text[0] == '+'
		for j := 1; j < len(text); j++ {
			switch ch := text[j]; {
			case ch == 'c' && !plus:
				cmdMode = true
			case ch == 'o':
				i++
				if i >= len(args) {
					return nil, false
				}
				name, lit := literalWordText(args[i])
				if !lit || !shellSetOptions[name] {
					return nil, false
				}
			case strings.IndexByte(shellOptionLetters, ch) >= 0:
			default:
				return nil, false
			}
		}
	}
	if cmdMode && i < len(args) {
		return args[i], true
	}
	return nil, false
}

// scriptWordUnsafe refuses an interceptor's NON-literal script word whose
// value the shell would read as an option rather than a script:
// `X=-e; bash -c $X "sed -i …"` runs the sed, because $X expands to -e
// before bash parses its options. A literal word was already classified by
// extractShellScript's option parse.
func scriptWordUnsafe(w *syntax.Word, vc *varCtx) bool {
	if wordIsLiteralText(w) {
		return false
	}
	words, ok := resolveArgWord(w, vc)
	if !ok {
		return true
	}
	if len(words) == 0 {
		return false
	}
	first := words[0].visible
	return strings.HasPrefix(first, "-") || strings.HasPrefix(first, "+")
}

// interceptorWord reports whether ce's command name (quotes removed) is a
// shell interceptor, whatever form it takes.
func interceptorWord(ce *syntax.CallExpr) bool {
	return shellInterceptors[commandBaseName(ce)]
}

// commandWordSubstitutable is the substitutability veto on a segment's
// command word. A wholly-literal word is judged on its visible text, so
// `"ls"` and `l"s"` are the same file as `ls` (#2241); any other word keeps
// the printed-token check (commandIsSubstitutable) — its resolved name was
// already judged by commandWordUnsafe (#2228).
func commandWordSubstitutable(ce *syntax.CallExpr, segment string) (bool, string, string) {
	if len(ce.Args) > 0 {
		if text, lit := literalWordText(ce.Args[0]); lit {
			return execguard.Substitutable(text, guardEnv())
		}
	}
	return commandIsSubstitutable(segment)
}

// runnerWrappers are the wrappers whose inner command is unwrapped and
// checked (#2241). command, exec and builtin are bash builtins that run the
// command that follows them; xargs runs its command with words from stdin
// appended (or substituted, with -I).
var runnerWrappers = map[string]bool{
	"command": true,
	"exec":    true,
	"builtin": true,
	"xargs":   true,
}

// wrapperOpt is one option a runner wrapper accepts: whether it takes a
// value, and the letter it is recorded under (long options map to their
// short letter, so the caller looks only at letters).
type wrapperOpt struct {
	arity  int // 0 none, 1 required (attached or next word), 2 optional (attached only)
	letter byte
}

// wrapperOptSpec is a runner wrapper's option grammar. Anything not listed
// refuses: the checker cannot tell where an unknown option's value ends and
// the command begins.
type wrapperOptSpec struct {
	short map[byte]int // letter → arity
	long  map[string]wrapperOpt
}

// runnerWrapperOpts are the option grammars, from bash's builtins (command
// [-pVv], exec [-cl] [-a name], builtin with none) and GNU findutils xargs.
// xargs --process-slot-var is left out on purpose: it sets a variable in the
// child's environment, which the #2326 rules would refuse as a prefix.
var runnerWrapperOpts = map[string]wrapperOptSpec{
	"command": {short: map[byte]int{'p': 0, 'v': 0, 'V': 0}},
	"exec":    {short: map[byte]int{'c': 0, 'l': 0, 'a': 1}},
	"builtin": {},
	"xargs": {
		short: map[byte]int{
			'0': 0, 'p': 0, 'r': 0, 't': 0, 'x': 0, 'o': 0,
			'a': 1, 'd': 1, 'E': 1, 'I': 1, 'L': 1, 'n': 1, 'P': 1, 's': 1,
			'e': 2, 'i': 2, 'l': 2,
		},
		long: map[string]wrapperOpt{
			"null":            {0, '0'},
			"interactive":     {0, 'p'},
			"no-run-if-empty": {0, 'r'},
			"verbose":         {0, 't'},
			"exit":            {0, 'x'},
			"open-tty":        {0, 'o'},
			"arg-file":        {1, 'a'},
			"delimiter":       {1, 'd'},
			"max-args":        {1, 'n'},
			"max-procs":       {1, 'P'},
			"max-chars":       {1, 's'},
			"eof":             {2, 'e'},
			"replace":         {2, 'i'},
			"max-lines":       {2, 'l'},
		},
	},
}

// scanWrapperOptions walks a runner wrapper's options (args[0] is the
// wrapper itself) and returns the index of the inner command word, 0 when
// there is none, and the options seen with their values. ok is false when an
// option is unknown, a value is missing, or an option or value word is not
// wholly literal (an unquoted variable could split and move the command).
func scanWrapperOptions(args []*syntax.Word, spec wrapperOptSpec) (inner int, seen map[byte]string, ok bool) {
	seen = map[byte]string{}
	for i := 1; i < len(args); i++ {
		text, lit := literalWordText(args[i])
		if !lit {
			return 0, nil, false
		}
		if text == "--" {
			if i+1 < len(args) {
				return i + 1, seen, true
			}
			return 0, seen, true
		}
		if len(text) < 2 || text[0] != '-' {
			return i, seen, true
		}
		if strings.HasPrefix(text, "--") {
			name, value, hasValue := strings.Cut(text[2:], "=")
			opt, known := spec.long[name]
			if !known {
				return 0, nil, false
			}
			switch {
			case opt.arity == 0 && hasValue:
				return 0, nil, false
			case opt.arity == 1 && !hasValue:
				i++
				if i >= len(args) {
					return 0, nil, false
				}
				if value, lit = literalWordText(args[i]); !lit {
					return 0, nil, false
				}
			}
			seen[opt.letter] = value
			continue
		}
		for j := 1; j < len(text); j++ {
			ch := text[j]
			arity, known := spec.short[ch]
			if !known {
				return 0, nil, false
			}
			if arity == 0 {
				seen[ch] = ""
				continue
			}
			value := text[j+1:]
			if value == "" && arity == 1 {
				i++
				if i >= len(args) {
					return 0, nil, false
				}
				if value, lit = literalWordText(args[i]); !lit {
					return 0, nil, false
				}
			}
			seen[ch] = value
			break // the rest of the token was the option's value
		}
	}
	return 0, seen, true
}

// xargsReplaceString returns the string xargs substitutes stdin into, and
// whether replace mode is on: -I R, -i[R] or --replace[=R]; the optional
// forms default to {}.
func xargsReplaceString(seen map[byte]string) (string, bool) {
	if r, ok := seen['I']; ok {
		return r, true
	}
	if r, ok := seen['i']; ok {
		if r == "" {
			r = "{}"
		}
		return r, true
	}
	return "", false
}

// wrappedCommandUnsafe judges the command a runner wrapper runs exactly as
// if it had been typed (#2241): the name gate (var-setting commands, shell
// interceptors and wrappers refuse — the inner command is not re-unwrapped),
// the #2228 command-word check, the flag and argument checks with #2216
// variable resolution, and the substitutability veto, whose reason is
// recorded on the printed segment the user is asked about. segment is the
// printed outer segment.
//
// It refuses whenever it cannot see the inner command or its arguments: an
// option it does not understand, a non-literal inner command word, or xargs
// supplying stdin words to a flag-checked command (xargs appends them, or
// substitutes them with -I, so `echo -i | xargs sed s/a/b/` runs sed -i).
// Under xargs the inner command always receives arguments, so a wrapper there
// refuses even when typed bare. A wrapper with no inner command (`exec 3<f`,
// bare `xargs`) and the lookup-only `command -v`/`-V` keep today's verdict.
func wrappedCommandUnsafe(ce *syntax.CallExpr, segment string, vc *varCtx) bool {
	name := commandBaseName(ce)
	if !runnerWrappers[name] {
		return false
	}
	inner, seen, ok := scanWrapperOptions(ce.Args, runnerWrapperOpts[name])
	if !ok {
		return true
	}
	if inner == 0 {
		return false
	}
	if name == "command" {
		if _, v := seen['v']; v {
			return false // only looks the name up
		}
		if _, v := seen['V']; v {
			return false
		}
	}
	innerCE := &syntax.CallExpr{Args: ce.Args[inner:]}
	text, lit := literalWordText(innerCE.Args[0])
	if !lit || text == "" {
		return true
	}
	base := filepath.Base(text)
	xargs := name == "xargs"
	if commandNameUnsafe(base, xargs || len(innerCE.Args) > 1, false) {
		return true
	}
	if xargs {
		if flagCheckedCommand(base) {
			return true
		}
		if r, replace := xargsReplaceString(seen); replace && (r == "" || strings.Contains(text, r)) {
			// GNU xargs does not substitute into the command word
			// (measured: `echo echo | xargs -I{} {}` fails with
			// "{}: No such file"); refusing a command word that holds
			// the replace string is fail-closed for implementations
			// that might.
			return true
		}
	}
	innerSeg := callExprCmdString(syntax.NewPrinter(), innerCE)
	if commandWordUnsafe(innerCE, innerSeg, vc) || containsUnsafeFlags(innerSeg) || expandedArgsUnsafe(innerCE, innerSeg, vc) || testCommandUnsafe(innerCE, vc) {
		return true
	}
	if sub, path, reason := execguard.Substitutable(text, guardEnv()); sub {
		vc.noteVeto(segment, path, reason)
		return true
	}
	return false
}

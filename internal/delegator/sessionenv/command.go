// command.go — the command-rewrite half of the session-env contract.
//
// opencode's injector can set the spawn environment directly (its `shell.env`
// plugin hook hands the plugin a mutable env map). Codex has no such lever:
// its PreToolUse hook may only rewrite the tool's *input*, so the only way to
// deliver a per-thread environment is to wrap the command.
//
// Both halves of that wrap live here — the writer (cmd/foci-codex-hook, which
// runs out-of-process) and the reader (internal/delegator/codex, which has to
// undo it before showing a command to the user or matching it against
// auto-approve rules). Keeping the format in one place is what stops the two
// drifting: an unwrap that no longer recognises the wrap silently degrades to
// showing agents the raw wrapper.
package sessionenv

import (
	"strings"
)

// WrapCommand renders cmd so it runs with e's variables in its environment.
//
// The nested shell is required, not cosmetic. Codex executes a bash tool call
// as `/bin/bash -lc '<command>'`; that outer bash has ALREADY started (and has
// already sourced the shared subprocess's stale BASH_ENV) by the time our
// rewritten string reaches it, so a `VAR=x` prefix inside the string cannot
// deliver BASH_ENV. Only a freshly-exec'd bash reads BASH_ENV — hence
// `env ... bash -c <original>`. Verified against codex-cli 0.145.0: exit
// status, stdout/stderr separation, and originals containing single quotes,
// `&&` and pipelines all survive the wrap.
//
// A zero entry or empty command is returned unchanged — "no per-session
// override" must cost nothing.
func WrapCommand(cmd string, e Entry) string {
	vars := e.Vars()
	if cmd == "" || len(vars) == 0 {
		return cmd
	}
	var b strings.Builder
	b.WriteString("env")
	for _, v := range vars {
		b.WriteString(" ")
		b.WriteString(v.Name)
		b.WriteString("=")
		b.WriteString(ShellQuote(v.Value))
	}
	b.WriteString(" bash -c ")
	b.WriteString(ShellQuote(cmd))
	return b.String()
}

// UnwrapCommand recovers the original command from a WrapCommand result,
// byte-for-byte. Returns (cmd, false) when cmd isn't one of ours — including
// the case where an agent genuinely ran its own `env FOO=1 bash -c ...`, which
// is why a foci variable name must be present before we claim the wrap.
func UnwrapCommand(cmd string) (string, bool) {
	words, ok := splitWords(cmd)
	if !ok || len(words) < 4 || words[0] != "env" {
		return cmd, false
	}
	i, ours := 1, false
	for i < len(words) {
		name, _, isAssign := strings.Cut(words[i], "=")
		if !isAssign || name == "" {
			break
		}
		if isSessionVar(name) {
			ours = true
		}
		i++
	}
	if !ours || i+3 != len(words) || words[i] != "bash" || words[i+1] != "-c" {
		return cmd, false
	}
	return words[i+2], true
}

// UnwrapDisplayCommand undoes the wrap inside the command string codex reports
// on item/started and on an approval request — which is not the tool input but
// the shell-quoted argv codex built from it, `/bin/bash -lc "<tool input>"`.
// Without this, every codex bash call would be shown to the user (and matched
// against auto-approve rules) as foci's wrapper rather than as the command the
// agent asked for.
//
// Anything it doesn't recognise is returned untouched.
func UnwrapDisplayCommand(display string) string {
	words, ok := splitWords(display)
	if !ok || len(words) < 3 {
		return display
	}
	inner, wrapped := UnwrapCommand(words[len(words)-1])
	if !wrapped {
		return display
	}
	return strings.Join(words[:len(words)-1], " ") + " " + ShellQuote(inner)
}

func isSessionVar(name string) bool {
	switch name {
	case "FOCI_SESSION_KEY", "FOCI_SOCK", "BASH_ENV":
		return true
	}
	return false
}

// ShellQuote renders s as a single POSIX shell word, single-quote style. The
// only character needing care inside a single-quoted string is the single
// quote itself, which is closed, escaped and reopened.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\r\"'\\$`&|;<>()*?[]#~!{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// splitWords splits a shell-quoted string into its words, honouring single
// quotes, double quotes and backslash escapes — enough to invert both
// ShellQuote and the argv rendering codex emits. Returns ok=false on an
// unterminated quote, so a string we can't confidently parse is left alone
// rather than mangled.
func splitWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			inWord = true
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && isDQEscapable(s[i+1]) {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, false
			}
		case c == '\\':
			inWord = true
			if i+1 >= len(s) {
				return nil, false
			}
			i++
			cur.WriteByte(s[i])
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, true
}

// isDQEscapable reports whether a backslash before c is an escape inside
// double quotes. Bash keeps the backslash literal before anything else.
func isDQEscapable(c byte) bool {
	return c == '"' || c == '\\' || c == '$' || c == '`' || c == '\n'
}

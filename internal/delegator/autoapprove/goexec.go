package autoapprove

import "strings"

// goEveryCommandFlags are go flags that run a program named in their value,
// refused on every go subcommand (#2736). The build flags are shared by the
// read-only `go list` and `go vet`, so each one is reachable from the
// CommonReadonlyRules set:
//   - toolexec: runs `<prog> <tool> <args>` for every compiler, assembler and
//     cgo step (`go list -export`, `go vet` both compile).
//   - exec: runs the built binary through `<prog>` (go run/test).
//   - vettool, fixtool: go vet / go fix run the named analysis program.
//   - compiler: `-compiler gccgo` runs the gccgo found on PATH.
//   - ldflags: `-extld=<prog>` names the external linker. vet and list do not
//     link today; refusing it only makes the command ask.
var goEveryCommandFlags = map[string]bool{
	"toolexec": true,
	"exec":     true,
	"vettool":  true,
	"fixtool":  true,
	"compiler": true,
	"ldflags":  true,
}

// goSubcommandFlags are flags refused only on one subcommand, because the
// same name is harmless elsewhere (`go doc -u` shows unexported symbols):
//   - env -w / -u write the go env file (os.UserConfigDir()/go/env), which
//     every later go command reads: `go env -w GOFLAGS=-toolexec=<prog>` or
//     `CC=<prog>` plants a program for the next go vet.
//   - vet -fix applies each diagnostic's fix: it edits source files.
//   - doc -http starts a documentation server (it runs pkgsite).
var goSubcommandFlags = map[string]map[string]bool{
	"env": {"w": true, "u": true},
	"vet": {"fix": true},
	"doc": {"http": true},
}

// goFlagName returns the flag name of a go command-line token as Go's flag
// parsers read it, and whether the token is a flag at all. Both parsers
// cmd/go uses (the flag package and cmd/go/internal/cmdflag) accept one or
// two leading dashes and an optional =value, and NO abbreviation (measured on
// go1.26: `-toolex=x` is "flag provided but not defined"). So -toolexec,
// --toolexec, -toolexec=x and --toolexec=x are all "toolexec"; the two-word
// form (`-toolexec x`) is the bare token.
func goFlagName(tok string) (string, bool) {
	if len(tok) < 2 || tok[0] != '-' {
		return "", false
	}
	name := tok[1:]
	if name[0] == '-' {
		name = name[1:]
	}
	if name == "" || name[0] == '-' || name[0] == '=' {
		return "", false // "--", "---x", "-=x": not a flag go would parse
	}
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		name = name[:eq]
	}
	return name, true
}

// goRunsProgram reports whether a go command line runs a program named in
// its arguments or writes persistent state (#2736): a flag from
// goEveryCommandFlags on any subcommand, or one from goSubcommandFlags on its
// subcommand. words are the command's shell-visible words, words[0] being go
// itself, so quoting changes nothing.
//
// The subcommand is the first non-flag word, stepping over the value of the
// one top-level option, -C <dir>. Every argument after it is scanned, past
// positionals and `--` too: go vet looks for -vettool anywhere on the line
// (cmd/go/internal/vet.parseToolFlag), and refusing a flag go would not
// honour is the safe direction. A flag's value word is scanned as well, so a
// value that looks like a refused flag (`-f -toolexec`) also asks.
func goRunsProgram(words []shellWord) bool {
	args := visibleTexts(words)
	if len(args) == 0 {
		return false
	}
	args = args[1:]
	sub := ""
	for i := 0; i < len(args); i++ {
		name, isFlag := goFlagName(args[i])
		if !isFlag {
			if sub == "" && args[i] != "--" {
				sub = args[i]
			}
			continue
		}
		if goEveryCommandFlags[name] {
			return true
		}
		if sub == "" {
			if name == "C" && !strings.Contains(args[i], "=") {
				i++ // -C <dir>: the next word is the directory
			}
			continue
		}
		if goSubcommandFlags[sub][name] {
			return true
		}
	}
	return false
}

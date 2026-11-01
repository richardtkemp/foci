package autoapprove

import "strings"

// gitTopValueOptions are git's top-level options that take their value as
// the NEXT word (git.c handle_options). Every other top-level option is a
// single word (--no-pager, -p, --git-dir=<dir>, …); git rejects one it does
// not know, so treating an unknown option as a single word cannot hide a
// subcommand that runs.
var gitTopValueOptions = map[string]bool{
	"-C":             true,
	"-c":             true,
	"--git-dir":      true,
	"--work-tree":    true,
	"--namespace":    true,
	"--config-env":   true,
	"--attr-source":  true,
	"--super-prefix": true,
}

// gitProgramOptions describes how one git subcommand can be told, in its own
// arguments, to run a program or shell command.
type gitProgramOptions struct {
	// longFlags: long options whose value is a program, a command line or
	// something that makes git run one (clone's hook template directory and
	// per-clone config). Matched with abbreviations: parse-options, perl's
	// Getopt::Long (send-email) and the shell scripts' prefix-tolerant
	// parsing accept unambiguous prefixes, and a prefix git would call
	// ambiguous only refuses.
	longFlags []string
	// shortFlags: short option letters whose value is a program
	// (rebase -x, difftool -x, clone -u). Found anywhere in a bundle
	// (-ix ./x) or with the value attached (-x./x).
	shortFlags string
	// shortValue: the subcommand's OTHER short letters that take a value; the
	// rest of the bundle after one is that value (-Xfix is -X "fix", not -x).
	shortValue string
	// actions: a first positional word that names an action running a
	// program given after it (bisect run, submodule foreach).
	actions []string
}

// gitProgramSubcommands maps each git subcommand that can run a program named
// in its arguments to the options that do it (#2326). Dick's broad rule
// `Bash:git -C /home/rich/git/ *` stays (his ruling, 2026-10-13); this table
// is what keeps it from running an arbitrary program.
var gitProgramSubcommands = map[string]gitProgramOptions{
	"rebase":     {longFlags: []string{"--exec"}, shortFlags: "x", shortValue: "sXCS"},
	"bisect":     {actions: []string{"run"}},
	"submodule":  {actions: []string{"foreach"}},
	"difftool":   {longFlags: []string{"--extcmd"}, shortFlags: "x", shortValue: "tO"},
	"fetch":      {longFlags: []string{"--upload-pack"}},
	"pull":       {longFlags: []string{"--upload-pack"}},
	"ls-remote":  {longFlags: []string{"--upload-pack", "--exec"}, shortFlags: "u"},
	"clone":      {longFlags: []string{"--upload-pack", "--template", "--config"}, shortFlags: "u", shortValue: "objc"},
	"push":       {longFlags: []string{"--receive-pack", "--exec"}, shortValue: "o"},
	"archive":    {longFlags: []string{"--exec"}, shortValue: "o"},
	"fetch-pack": {longFlags: []string{"--upload-pack", "--exec"}},
	"send-pack":  {longFlags: []string{"--receive-pack", "--exec"}},
	"send-email": {longFlags: []string{"--smtp-server", "--sendmail-cmd", "--to-cmd", "--cc-cmd", "--header-cmd"}},
	"filter-branch": {longFlags: []string{
		"--setup", "--env-filter", "--tree-filter", "--index-filter", "--parent-filter",
		"--msg-filter", "--commit-filter", "--tag-name-filter",
	}},
}

// gitRunsNamedProgram reports whether a git command line runs a program or
// shell command named in its own arguments (#2326): a subcommand option from
// gitProgramSubcommands, or the top-level --exec-path=<dir>, which makes git
// run its non-builtin subcommands from that directory. words are the
// command's shell-visible words, words[0] being git itself, so quoting
// ('--exec', "run") changes nothing.
//
// The subcommand is found the way git finds it: skip the top-level options,
// stepping over the value of each one that takes the next word. The
// subcommand's own arguments are then scanned without stopping at `--`: a
// program option after it is not one git honours, and refusing it is the
// safe direction.
func gitRunsNamedProgram(words []shellWord) bool {
	args := visibleTexts(words)
	if len(args) == 0 {
		return false
	}
	args = args[1:]
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") {
			break
		}
		if strings.HasPrefix(a, "--exec-path=") {
			return true
		}
		if gitTopValueOptions[a] {
			i += 2
			continue
		}
		i++
	}
	if i < len(args) && args[i] == "--" {
		i++
	}
	if i >= len(args) {
		return false
	}
	spec, ok := gitProgramSubcommands[args[i]]
	if !ok {
		return false
	}
	return spec.argsRunProgram(args[i+1:])
}

// argsRunProgram scans one subcommand's arguments for an option or action
// that runs a program.
func (spec gitProgramOptions) argsRunProgram(args []string) bool {
	seenPositional := false
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--"):
			for _, lf := range spec.longFlags {
				if longFlagMatches(a, lf, true) {
					return true
				}
			}
		case len(a) >= 2 && a[0] == '-':
			if spec.shortBundleRunsProgram(a[1:]) {
				return true
			}
		default:
			if !seenPositional {
				seenPositional = true
				for _, act := range spec.actions {
					if a == act {
						return true
					}
				}
			}
		}
	}
	return false
}

// shortBundleRunsProgram scans a short-option bundle (the text after '-') the
// way parse-options does: letter by letter until a letter that takes a value,
// whose value is the rest of the bundle.
func (spec gitProgramOptions) shortBundleRunsProgram(bundle string) bool {
	for j := 0; j < len(bundle); j++ {
		ch := bundle[j]
		if strings.IndexByte(spec.shortFlags, ch) >= 0 {
			return true
		}
		if strings.IndexByte(spec.shortValue, ch) >= 0 {
			return false
		}
	}
	return false
}

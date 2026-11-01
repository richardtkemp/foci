package autoapprove

import (
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ReadonlyGitRule is the built-in rule that auto-approves read-only git
// commands (#2734). Unlike a prefix rule ("Bash:git log", which would also
// approve `git log --output=<file>`), it approves a git command only when
// readonlyGitArgsSafe accepts its whole argument vector: a top-level option
// from a short allowlist, then a subcommand from readonlyGitSubcommands whose
// own check accepts the subcommand's arguments. Every other git command falls
// through to the remaining rules, so a broader user rule (Dick's
// `Bash:git -C /home/rich/git/ *`) keeps its own meaning.
//
// What it refuses (each exec or write vector the command line can name):
//
//   - git's -c/--config-env and the `config` subcommand word (already refused
//     for every git rule by unsafeFlags), --exec-path and every other
//     top-level option outside the allowlist (-p/--paginate, --attr-source,
//     --help, …);
//   - `--help` anywhere: git turns it into `git help`, which runs the
//     configured man viewer or browser;
//   - --output (writes a file), --ext-diff (runs the external diff driver),
//     --textconv (runs a textconv driver), --show-signature and the %G…
//     format placeholders (run gpg.program), cat-file --filters (runs a
//     filter driver), grep -O/--open-files-in-pager (already refused for
//     every git rule by gitGrepPagerUnsafe);
//   - the write forms of branch, tag, reflog and remote: branch/tag take
//     only list options, and a name argument only with --list/-l (`git
//     branch -v <name>` CREATES <name> on git 2.43); reflog takes only show
//     or exists; remote takes only -v and get-url (remote show contacts the
//     remote);
//   - any argument that is not plain literal text (a variable or
//     substitution) and any live glob or brace: the check reads the words as
//     typed, so a word whose value is decided by the shell cannot be judged.
//
// What it cannot refuse: git also runs programs named in its CONFIGURATION
// (repo .git/config, ~/.gitconfig, /etc/gitconfig, plus .gitattributes and
// .git/hooks). Measured on git 2.43 with stdout not a terminal (as in every
// backend's shell tool): core.fsmonitor runs on status, diff, blame,
// ls-files, grep and describe --dirty; filter.<driver>.clean on status,
// diff and blame of a modified file; diff.external on diff;
// diff.<driver>.textconv on log -p, show and blame; .git/hooks/
// post-index-change on status and describe --dirty; gpg.program on log/show
// of a signed commit when log.showSignature is set. log without -p,
// rev-parse, ls-tree, cat-file -p, branch, tag -l, reflog, shortlog and
// remote -v ran none of them. The pager (core.pager, pager.<cmd>) runs only
// when stdout is a terminal, which an approved command cannot arrange
// (output redirects other than /dev/null are refused). No command-line form
// can switch the configured drivers off, so this rule is only as safe as
// the session's inability to write git configuration without a prompt. A
// read-only explore session cannot write at all. A session with Edit or
// Write auto-approved over a repository (every delegated agent's workspace
// rule) can plant such configuration and then run it through an approved
// git command; Dick's household config already auto-approves `git status *`,
// `git log *` and `git -C /home/rich/git/ *` for every agent, so there the
// rule adds no new path. A deployment without such rules gains one.
const ReadonlyGitRule = "Bash:" + readonlyGitPattern

// readonlyGitPattern is ReadonlyGitRule's pattern. As a prefix pattern it
// could match no real command (a command word cannot contain "git (" unquoted),
// so reserving it changes no existing rule's meaning.
const readonlyGitPattern = "git (read-only)"

// gitReadonlyTopFlags are the top-level git options the rule accepts as a
// single word. git's top-level options reject abbreviations (see
// unsafeFlags), so exact spellings are the whole set.
var gitReadonlyTopFlags = map[string]bool{
	"--no-pager":           true,
	"--no-optional-locks":  true,
	"--literal-pathspecs":  true,
	"--glob-pathspecs":     true,
	"--noglob-pathspecs":   true,
	"--icase-pathspecs":    true,
	"--no-replace-objects": true,
	"--bare":               true,
}

// gitReadonlyTopValueFlags take a directory: the next word, or attached
// after '=' for the long forms (-C has no attached form in git.c).
var gitReadonlyTopValueFlags = map[string]bool{
	"-C":          true,
	"--git-dir":   true,
	"--work-tree": true,
}

// readonlyGitSubcommands maps each subcommand the rule approves to the check
// of its arguments (the words after the subcommand). A check returns true
// when the arguments are safe.
var readonlyGitSubcommands = map[string]func(args []string) bool{
	"status":    func(args []string) bool { return !gitAnyHelp(args) },
	"log":       gitLogArgsSafe,
	"show":      gitLogArgsSafe,
	"shortlog":  gitLogArgsSafe,
	"diff":      gitLogArgsSafe,
	"blame":     func(args []string) bool { return !gitAnyHelp(args) && !gitAnyLongFlag(args, "--textconv") },
	"rev-parse": func(args []string) bool { return !gitAnyHelp(args) },
	"ls-files":  func(args []string) bool { return !gitAnyHelp(args) },
	"ls-tree":   func(args []string) bool { return !gitAnyHelp(args) },
	"cat-file": func(args []string) bool {
		return !gitAnyHelp(args) && !gitAnyLongFlag(args, "--textconv", "--filters")
	},
	"describe": func(args []string) bool { return !gitAnyHelp(args) },
	"grep":     func(args []string) bool { return !gitAnyHelp(args) && !gitAnyLongFlag(args, "--textconv") },
	"branch":   gitBranchArgsSafe,
	"tag":      gitTagArgsSafe,
	"reflog":   gitReflogArgsSafe,
	"remote":   gitRemoteArgsSafe,
}

// ReadonlyGitSubcommands returns the subcommands ReadonlyGitRule approves,
// sorted, for prompts that list the rule.
func ReadonlyGitSubcommands() []string {
	names := make([]string, 0, len(readonlyGitSubcommands))
	for name := range readonlyGitSubcommands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DisplayRule renders an auto-approve rule for prose: the "Bash:" prefix is
// dropped, and ReadonlyGitRule is spelled out with its subcommands and the
// list-only forms. Other rules pass through unchanged.
func DisplayRule(rule string) string {
	if rule == ReadonlyGitRule {
		return "git (read-only: " + strings.Join(ReadonlyGitSubcommands(), ", ") +
			"; branch and tag list only, remote -v/get-url, reflog show; " +
			"-C <dir> allowed; no --output, --ext-diff, --textconv, --show-signature)"
	}
	return strings.TrimPrefix(rule, "Bash:")
}

// readonlyGitSegmentSafe reports whether ReadonlyGitRule approves the
// segment printed from ce. Only wholly literal words are judged: a variable,
// substitution, live glob or live brace makes the shell, not the typed text,
// decide what git receives.
func readonlyGitSegmentSafe(segment string, ce *syntax.CallExpr) bool {
	for _, w := range ce.Args {
		if !wordIsLiteralText(w) {
			return false
		}
	}
	words := scanShellWords(segment)
	if len(words) != len(ce.Args) || len(words) == 0 {
		return false
	}
	for _, w := range words {
		if w.hasLiveGlob || w.hasLiveBrace || w.hasLiveSubst {
			return false
		}
	}
	args := visibleTexts(words)
	if args[0] != "git" {
		return false
	}
	return readonlyGitArgsSafe(args[1:])
}

// readonlyGitArgsSafe judges git's argument vector (without "git" itself):
// allowlisted top-level options, then an allowlisted subcommand whose check
// accepts the rest.
func readonlyGitArgsSafe(args []string) bool {
	i := 0
	for i < len(args) {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			break
		}
		switch {
		case gitReadonlyTopFlags[a]:
			i++
		case gitReadonlyTopValueFlags[a]:
			if i+1 >= len(args) {
				return false
			}
			i += 2
		case strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			i++
		default:
			return false // -c, --exec-path, -p, --help, --, anything unknown
		}
	}
	if i >= len(args) {
		return false
	}
	check, ok := readonlyGitSubcommands[args[i]]
	if !ok {
		return false
	}
	return check(args[i+1:])
}

// gitAnyHelp reports whether any argument asks for help in a form that can
// reach `git help` (which runs the configured man viewer): --help or an
// abbreviation of it. -h (usage text, printed by git itself) is not one.
func gitAnyHelp(args []string) bool {
	return gitAnyLongFlag(args, "--help")
}

// gitAnyLongFlag reports whether any argument is one of flags, in the exact
// spelling, the --flag=value form or an abbreviation (parse-options accepts
// unambiguous prefixes; the revision and diff parsers do not, and matching
// prefixes anyway only refuses more). The scan does not stop at `--`: a flag
// after it is a path to git, and refusing it is the safe direction.
func gitAnyLongFlag(args []string, flags ...string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		for _, f := range flags {
			if longFlagMatches(a, f, true) {
				return true
			}
		}
	}
	return false
}

// gitLogArgsSafe judges the arguments of the log family (log, show,
// shortlog) and diff: no file output, no external diff or textconv driver
// named on the command line, no signature verification.
func gitLogArgsSafe(args []string) bool {
	if gitAnyHelp(args) || gitAnyLongFlag(args, "--output", "--ext-diff", "--textconv", "--show-signature") {
		return false
	}
	for _, a := range args {
		// %G? %GG %GS %GK … in a --format/--pretty string verify the
		// signature with gpg.program. Refused wherever it appears.
		if strings.Contains(a, "%G") {
			return false
		}
	}
	return true
}

// gitRefListOption describes the options branch and tag accept in list mode.
// Spellings are exact: an abbreviation could resolve to a write option
// (--c is --copy as well as --contains), so it is prompted.
type gitRefListOption struct {
	longFlags  map[string]bool // single-word long options
	valueFlags map[string]bool // long options taking a value, attached after '=' or as the next word
	shortFlags string          // single-letter options, bundleable
}

// gitBranchList: the options of `git branch` that only list or select.
var gitBranchList = gitRefListOption{
	longFlags: map[string]bool{
		"--list": true, "--all": true, "--remotes": true, "--verbose": true,
		"--show-current": true, "--color": true, "--no-color": true,
		"--column": true, "--no-column": true, "--ignore-case": true,
		"--abbrev": true, "--no-abbrev": true, "--quiet": true, "--omit-empty": true,
	},
	valueFlags: map[string]bool{
		"--contains": true, "--no-contains": true, "--merged": true, "--no-merged": true,
		"--points-at": true, "--format": true, "--sort": true,
	},
	shortFlags: "arvlqi",
}

// gitTagList: the options of `git tag` that only list or select. -n<num>
// (annotation lines) is handled by the bundle scan.
var gitTagList = gitRefListOption{
	longFlags: map[string]bool{
		"--list": true, "--color": true, "--no-color": true, "--column": true,
		"--no-column": true, "--ignore-case": true, "--omit-empty": true,
	},
	valueFlags: map[string]bool{
		"--contains": true, "--no-contains": true, "--merged": true, "--no-merged": true,
		"--points-at": true, "--format": true, "--sort": true,
	},
	shortFlags: "lin",
}

// refListArgsSafe judges branch/tag arguments: every option from the list
// set, and a positional argument (a pattern in list mode, a NAME TO CREATE
// otherwise) only when --list or -l is present. A value flag's next word is
// its value, not a positional — what git does with --contains <commit>.
// "signature" in any word is refused: newer git's %(signature) ref-filter
// atom (--format, --sort) runs gpg.program.
func refListArgsSafe(spec gitRefListOption, args []string) bool {
	listMode, positional := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.Contains(a, "signature") {
			return false
		}
		switch {
		case a == "--":
			positional = positional || i+1 < len(args)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			name, _, attached := strings.Cut(a, "=")
			switch {
			case spec.valueFlags[name]:
				if !attached {
					i++ // the next word is the option's value
				}
			case spec.longFlags[name]:
				// --color=always, --column=…, --abbrev=7: attached value.
				if name == "--list" {
					listMode = true
				}
			default:
				return false
			}
		case len(a) >= 2 && a[0] == '-':
			bundle := a[1:]
			for j := 0; j < len(bundle); j++ {
				ch := bundle[j]
				if ch == 'n' && strings.IndexByte(spec.shortFlags, 'n') >= 0 {
					// tag -n[<num>]: the rest of the bundle must be digits.
					if strings.Trim(bundle[j+1:], "0123456789") != "" {
						return false
					}
					break
				}
				if strings.IndexByte(spec.shortFlags, ch) < 0 {
					return false
				}
				if ch == 'l' {
					listMode = true
				}
			}
		default:
			positional = true
		}
	}
	return listMode || !positional
}

func gitBranchArgsSafe(args []string) bool { return refListArgsSafe(gitBranchList, args) }
func gitTagArgsSafe(args []string) bool    { return refListArgsSafe(gitTagList, args) }

// gitReflogArgsSafe accepts `git reflog [show|exists] …` with log options:
// the first word that is not an option must be show or exists, or absent
// (bare `git reflog` is show). Anything else (expire, delete, drop, or a
// ref git would take as show) is prompted: git dispatches on that word.
func gitReflogArgsSafe(args []string) bool {
	if !gitLogArgsSafe(args) {
		return false
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a == "show" || a == "exists"
	}
	return true
}

// gitRemoteArgsSafe accepts `git remote`, `git remote -v`/`--verbose`, and
// `git remote get-url [--push] [--all] <name>`. Every other action writes
// the configuration (add, rename, remove, set-url, …) or contacts the remote
// (show, prune, update), which runs core.sshCommand or a remote helper.
func gitRemoteArgsSafe(args []string) bool {
	i := 0
	for i < len(args) && (args[i] == "-v" || args[i] == "--verbose") {
		i++
	}
	if i == len(args) {
		return true
	}
	if args[i] != "get-url" {
		return false
	}
	names := 0
	for _, a := range args[i+1:] {
		switch {
		case a == "--push" || a == "--all":
		case strings.HasPrefix(a, "-"):
			return false
		default:
			names++
		}
	}
	return names == 1
}

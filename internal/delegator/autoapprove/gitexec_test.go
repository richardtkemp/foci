package autoapprove

import "testing"

// broadGitRule is Dick's broad operator rule from foci.toml, kept by #2326.
const broadGitRule = "Bash:git -C /home/rich/git/ *"

// TestGitNamedProgramAsks proves requirement 3 of #2326: under the broad
// git rule, a git subcommand that runs a program or shell command named in
// its arguments asks, in every spelling git's own option parsing accepts —
// separate, attached and = values, bundled short options, abbreviated long
// options — and after top-level options. The rule's text only pins the first
// -C, so a second -C reaches any repository.
func TestGitNamedProgramAsks(t *testing.T) {
	rules := parseAutoApproveRules([]string{broadGitRule})
	const pre = "git -C /home/rich/git/ -C foci "
	for _, rest := range []string{
		"rebase --exec ./x HEAD~1",
		"rebase -x ./x HEAD~1",
		"rebase -x./x HEAD~1",
		"rebase --exec=./x HEAD~1",
		"rebase --exe ./x HEAD~1",
		"rebase -ix ./x HEAD~1",
		"rebase -i --exec ./x HEAD~1",
		"bisect run ./x",
		"submodule foreach ./x",
		"difftool -x ./x",
		"difftool --extcmd=./x",
		"filter-branch --tree-filter ./x HEAD",
		"fetch --upload-pack=./x origin",
		"ls-remote --upload-pack=./x .",
		"push --receive-pack=./x origin",
		"archive --remote=. --exec=./x HEAD",
	} {
		assertApproval(t, rules, pre+rest, false)
	}
	assertApproval(t, rules, "git -C /home/rich/git/ --no-pager -C foci bisect run ./x", false)
	assertApproval(t, rules, "git -C /home/rich/git/ -p -C foci bisect run ./x", false)
	assertApproval(t, rules, "git -C /home/rich/git/ -C foci --no-pager rebase -x ./x HEAD~1", false)

	blanket := parseAutoApproveRules([]string{"Bash"})
	assertApproval(t, blanket, "git rebase --exec ./x HEAD~1", false)
	assertApproval(t, blanket, "git bisect run ./x", false)
}

// TestGitNamedProgramOtherFormsAsk covers the rest of the class beyond the
// spec's rows: the clone, pull, push and pack forms that name the remote-side
// program, clone's hook template directory and per-clone config, send-email's
// command options, the top-level --exec-path, and quoted spellings — quoting
// does not change what git receives.
func TestGitNamedProgramOtherFormsAsk(t *testing.T) {
	rules := parseAutoApproveRules([]string{broadGitRule})
	const pre = "git -C /home/rich/git/ -C foci "
	for _, rest := range []string{
		"pull --upload-pack=./x origin",
		"clone -u ./x origin dst",
		"clone --upload-pack ./x origin dst",
		"clone --template=./t origin dst",
		"clone --config=core.fsmonitor=./x origin dst",
		"push --exec=./x origin",
		"fetch-pack --upload-pack=./x origin",
		"send-pack --receive-pack=./x origin",
		"ls-remote -u ./x .",
		"send-email --sendmail-cmd=./x a.patch",
		"filter-branch --msg-filter ./x HEAD",
		"filter-branch --setup ./x HEAD",
		`"rebase" '--exec' ./x HEAD~1`,
		`bisect "run" ./x`,
	} {
		assertApproval(t, rules, pre+rest, false)
	}
	assertApproval(t, rules, "git -C /home/rich/git/ --exec-path=./d -C foci status", false)
}

// TestGitBroadRuleStillApproves is a characterisation test: the ordinary
// git commands requirement 3 names as still approved under the broad rule,
// including subcommands whose names or arguments merely look like the
// refused forms.
func TestGitBroadRuleStillApproves(t *testing.T) {
	rules := parseAutoApproveRules([]string{broadGitRule})
	const pre = "git -C /home/rich/git/ -C foci "
	for _, rest := range []string{
		"status",
		"rebase main",
		"pull --rebase",
		"bisect start",
		"bisect good",
		"log --grep=run",
		"commit -m 'bisect run x'",
		"rebase -i main",
		"fetch origin",
		"push origin HEAD",
	} {
		assertApproval(t, rules, pre+rest, true)
	}
}

package autoapprove

import "testing"

// assertApprovalEnv is assertApproval with a backend environment snapshot:
// the #2326 inherited-variable rows depend on which names the backend's
// environment already exports.
func assertApprovalEnv(t *testing.T, rules []Rule, env map[string]string, cmd string, want bool) {
	t.Helper()
	got, _ := MatchWithEnv(rules, "Bash", bashInput(t, cmd), env)
	if got != want {
		t.Errorf("MatchWithEnv(rules, Bash, %q, env) = %v, want %v", cmd, got, want)
	}
}

// envWithHome is a backend environment that exports HOME and does not
// contain W, as requirement 7 asks for the inherited-variable rows.
func envWithHome() map[string]string {
	return map[string]string{"HOME": "/home/foci", "LANG": "C.UTF-8"}
}

// TestEnvPrefixAlwaysAsks proves requirement 1 of #2326: a command segment
// that carries ANY environment-variable prefix assignment — whatever the
// name or value, empty and += included — is not auto-approved under any rule,
// in every segment position the walker reaches (lists, pipes, subshells,
// command substitutions, loop bodies, -c scripts). A prefix can make an
// approved program run another program (GOFLAGS=-toolexec) or load a config
// (RIPGREP_CONFIG_PATH, HOME), so the prefix-stripped match is not a verdict
// about what runs.
func TestEnvPrefixAlwaysAsks(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`GOFLAGS=-toolexec=./x go list -export .`,
		`RIPGREP_CONFIG_PATH=./c rg foo`,
		`FOO=1 ls`,
		`LANG=C ls`,
		`Y=1 X=2 ls`,
		`X= ls`,
		`GOFLAGS+=-toolexec=./x go vet ./...`,
		`ls && GOFLAGS=-toolexec=./x go vet ./...`,
		`(GOFLAGS=-toolexec=./x go vet ./...)`,
		`echo $(RIPGREP_CONFIG_PATH=./c rg foo)`,
		`ls | RIPGREP_CONFIG_PATH=./c rg foo`,
		`bash -c 'GOFLAGS=-toolexec=./x go vet ./...'`,
		`for f in a; do GOFLAGS=-toolexec=./x go vet ./...; done`,
	} {
		assertApprovalEnv(t, readonly, envWithHome(), cmd, false)
	}
	gitLog := parseAutoApproveRules([]string{"Bash:git log *"})
	assertApprovalEnv(t, gitLog, envWithHome(), `HOME=./e git log -1`, false)
	blanket := parseAutoApproveRules([]string{"Bash"})
	assertApprovalEnv(t, blanket, envWithHome(), `FOO=1 ls`, false)
}

// TestExportedChangeBeforeCommandAsks proves requirement 2 of #2326: a
// variable that reaches a LATER command's environment without a prefix —
// through an exporting declaration, or a plain assignment to a name the
// backend environment already exports (bash keeps it exported) — makes the
// request ask.
func TestExportedChangeBeforeCommandAsks(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	for _, cmd := range []string{
		`export GOFLAGS=-toolexec=./x; go list -export .`,
		`declare -x GOFLAGS=-toolexec=./x; go list -export .`,
		`typeset -x RIPGREP_CONFIG_PATH=./c; rg foo`,
		`export RIPGREP_CONFIG_PATH=./c && rg foo`,
		`RIPGREP_CONFIG_PATH=./c; export RIPGREP_CONFIG_PATH; rg foo`,
		`HOME=/tmp/x; rg foo`,
	} {
		assertApprovalEnv(t, readonly, envWithHome(), cmd, false)
	}
	gitLog := parseAutoApproveRules([]string{"Bash:git log *"})
	assertApprovalEnv(t, gitLog, envWithHome(), `export GIT_PAGER=./x; git log -1`, false)
	assertApprovalEnv(t, gitLog, envWithHome(), `HOME=./e; git log -1`, false)
}

// TestExportedChangeOtherShapesAsk extends requirement 2 to the other ways a
// request can change an exported variable before a command runs: a for-loop
// iterator, an arithmetic assignment, a ${VAR:=…} default, a declaration
// reached through `builtin`/`command`, an export inside a loop whose body
// runs the command again, a command whose own argument expansion does the
// change, and the same shapes inside a -c script.
func TestExportedChangeOtherShapesAsk(t *testing.T) {
	gitLog := parseAutoApproveRules([]string{"Bash:git log *", "Bash:echo *", "Bash:ls *"})
	env := envWithHome()
	env["EMPTY"] = ""
	for _, cmd := range []string{
		`for HOME in ./e; do git log -1; done`,
		`((HOME=5)); git log -1`,
		`let HOME=5; git log -1`,
		`echo ${EMPTY:=./e}; git log -1`,
		`ls $((HOME=5))`,
		`declare HOME=./e; git log -1`,
		`readonly HOME=./e; git log -1`,
		`for i in 1 2; do git log -1; export GIT_PAGER=./x; done`,
		`bash -c 'export GIT_PAGER=./x; git log -1'`,
		`bash -c 'HOME=./e; git log -1'`,
	} {
		assertApprovalEnv(t, gitLog, env, cmd, false)
	}
	blanket := parseAutoApproveRules([]string{"Bash"})
	assertApprovalEnv(t, blanket, env, `builtin export GIT_PAGER=./x; git log -1`, false)
	assertApprovalEnv(t, blanket, env, `command export GIT_PAGER=./x; git log -1`, false)
}

// TestEnvChangeScopeOutStaysApproved is a characterisation test for the rows
// #2326 keeps: the same commands without a prefix, a plain assignment to a
// name the backend environment does not hold, a declaration or assignment
// with no later command, and a command that runs BEFORE the export.
func TestEnvChangeScopeOutStaysApproved(t *testing.T) {
	readonly := parseAutoApproveRules(CommonReadonlyRules)
	env := envWithHome()
	for _, cmd := range []string{
		`go list -export .`,
		`rg foo`,
		`ls`,
		`X=1; ls`,
	} {
		assertApprovalEnv(t, readonly, env, cmd, true)
	}
	gitLog := parseAutoApproveRules([]string{"Bash:git log *"})
	assertApprovalEnv(t, gitLog, env, `git log -1`, true)
	gitAny := parseAutoApproveRules([]string{"Bash:git *"})
	assertApprovalEnv(t, gitAny, env, `W=/srv/x; git -C $W log`, true)
	blanket := parseAutoApproveRules([]string{"Bash"})
	assertApprovalEnv(t, blanket, env, `export FOO=1`, true)
	assertApprovalEnv(t, blanket, env, `X=1`, true)
	assertApprovalEnv(t, blanket, env, `ls; export FOO=1`, true)
}

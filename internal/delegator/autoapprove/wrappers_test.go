package autoapprove

import (
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/execguard"
)

// #2241: a command reached through a wrapper, a shell-interceptor option or a
// quoted name gets the same checks as when typed, and the built-in read-only
// commands lose their remaining exec and write flags.
//
// "Under Bash" is a tool-name-only rule; "under read-only" is
// CommonReadonlyRules. Red rows (a different verdict on the code before
// #2241) and characterisation rows (the same verdict) live in separate
// functions; the characterisation functions are listed in
// .factory/red-gate-exempt.txt.

func bashAnyRules() []Rule { return Compile([]string{"Bash"}) }

func readonlyRules() []Rule { return Compile(CommonReadonlyRules) }

// TestWrapper2241_BashNotApproved: every row was approved under Bash before
// #2241.
func TestWrapper2241_BashNotApproved(t *testing.T) {
	rules := bashAnyRules()
	for _, cmd := range []string{
		// Requirement 1: command / exec / builtin.
		`command sed -i s/a/b/ f`,
		`command -p sed -i s/a/b/ f`,
		`command -- sed -i s/a/b/ f`,
		`command -p -- sed -i s/a/b/ f`,
		`exec sed -i s/a/b/ f`,
		`exec -a x sed -i s/a/b/ f`,
		`exec -cl sed -i s/a/b/ f`,
		`exec "sed" -i s/a/b/ f`,
		`builtin eval "sed -i s/a/b/ f"`,
		`command eval "sed -i s/a/b/ f"`,
		`command bash -c "sed -i s/a/b/ f"`,
		`command env sed -i s/a/b/ f`,
		`command command sed -i s/a/b/ f`,
		`command -x sed`,
		`C=sed; command $C -i s/a/b/ f`,
		`X=-i; command sed $X s/a/b/ f`,
		`\command sed -i s/a/b/ f`,
		`command rg foo *`,
		// Requirement 2: xargs.
		`echo f | xargs sed -i s/a/b/`,
		`echo -i | xargs sed s/a/b/`,
		`echo a | xargs -I{} rg {} f`,
		`xargs bash -c 'ls'`,
		`xargs --frob cat`,
		`echo sed | xargs -I% % -i f`,
		`echo rm | xargs env`,
		`xargs --process-slot-var=LD_PRELOAD cat`,
		// Requirement 3: interceptor forms the checker does not unwrap,
		// and scripts after options.
		`bash -e -c "sed -i s/a/b/ f"`,
		`bash -ec "sed -i s/a/b/ f"`,
		`bash -ce "sed -i s/a/b/ f"`,
		`bash '-c' "sed -i s/a/b/ f"`,
		`bash --rcfile x -c ls`,
		`echo "sed -i s/a/b/ f" | bash`,
		`bash -s <<< "sed -i s/a/b/ f"`,
		`bash -c -e "sed -i s/a/b/ f"`,
		`bash -c -- "sed -i s/a/b/ f"`,
		`bash -i -c 'ls'`,
		`bash -l -c 'ls'`,
		`bash --login -c 'ls'`,
		`bash --init-file x -c 'ls'`,
		`bash -O extglob -c 'ls'`,
		`bash +O extglob -c 'ls'`,
		`bash -o history -c 'ls'`,
		`bash -a -c 'ls'`,
		`bash x.sh`,
		`bash -- x.sh`,
		`bash --version`,
		`bash`,
		`sh`,
		`bash -c`,
		`X=-e; bash -c $X "sed -i s/a/b/ f"`,
		// Requirement 4: wrappers reached through a variable.
		`C=command; $C sed -i s/a/b/ f`,
		`C=exec; $C ls`,
		`C=xargs; echo f | $C cat`,
		`C=builtin; $C cd /tmp`,
		// Requirement 5: other exec wrappers.
		`stdbuf -o0 sed -i s/a/b/ f`,
		`busybox sed -i s/a/b/ f`,
		`sudo sed -i s/a/b/ f`,
		// Requirement 6: a quoted literal command name.
		`"eval" "sed -i s/a/b/ f"`,
		`'env' sed -i s/a/b/ f`,
		`"bash" -c "sed -i s/a/b/ f"`,
		`"source" x`,
		`"command" sed -i s/a/b/ f`,
		`e"va"l "sed -i s/a/b/ f"`,
		// Requirement 9: every segment.
		`bash -c "command sed -i s/a/b/ f"`,
		`ls && exec sed -i s/a/b/ f`,
		`ls | exec sed -i s/a/b/ f`,
		`(command sed -i s/a/b/ f)`,
		`for i in 1; do command sed -i s/a/b/ f; done`,
		`echo $(command sed -i s/a/b/ f)`,
		`bash -e -c "command sed -i s/a/b/ f"`,
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestWrapper2241_ExecWrappersWithArgsNotApproved: requirement 5. Every
// listed exec wrapper refuses when it carries arguments; each was approved
// under Bash before #2241.
func TestWrapper2241_ExecWrappersWithArgsNotApproved(t *testing.T) {
	rules := bashAnyRules()
	for _, w := range []string{
		"stdbuf", "busybox", "chroot", "unshare", "nsenter", "prlimit", "chrt",
		"setpriv", "runuser", "su", "sudo", "doas", "systemd-run", "unbuffer",
		"parallel", "ltrace", "valgrind", "firejail",
	} {
		assertApproval(t, rules, w+" ls", false)
	}
}

// TestWrapper2241_ReadonlyNewApprovals: requirement 3 under read-only. The
// interceptor forms with options are unwrapped and their script matched as
// `bash -c` is; none was approved before #2241.
func TestWrapper2241_ReadonlyNewApprovals(t *testing.T) {
	rules := readonlyRules()
	for _, cmd := range []string{
		`bash -euo pipefail -c "ls"`,
		`bash -e -c 'ls'`,
		`bash '-c' "ls"`,
		`sh -euc 'ls'`,
		`bash -o pipefail -c 'ls'`,
		`bash --norc --noprofile -c 'ls'`,
		`bash -c -e 'ls'`,
	} {
		assertApproval(t, rules, cmd, true)
	}
}

// TestWrapper2241_ReadonlyNotApproved: requirement 7 (and `file *`). Each
// row was approved under read-only before #2241.
func TestWrapper2241_ReadonlyNotApproved(t *testing.T) {
	rules := readonlyRules()
	for _, cmd := range []string{
		`rg --hostname-bin=./x.sh foo`,
		`rg --hostname-bin ./x.sh foo`,
		`find . -fprint0 out`,
		`yq -s '.a' f.yaml`,
		`yq --split-exp '.a' f.yaml`,
		`yq --split-exp-file x f.yaml`,
		`ss -D out`,
		`ss --diag=out`,
		`ss --di=out`,
		`ss -K`,
		`ss --kill`,
		`file -C -m magic`,
		`file --compile -m magic`,
		`file --comp -m magic`,
		`file *`,
	} {
		assertApproval(t, rules, cmd, false)
	}
}

// TestWrapper2241_Characterisation_BashApproved: rows approved under Bash
// before and after #2241 — a wrapper with no inner command, a lookup-only
// `command -v`, xargs on a command without flag machinery, and the
// interceptor forms whose script is safe.
func TestWrapper2241_Characterisation_BashApproved(t *testing.T) {
	rules := bashAnyRules()
	for _, cmd := range []string{
		`command ls`,
		`exec ls`,
		`command -v sed`,
		`command -pv sed`,
		`command -V sed`,
		`exec 3<f`,
		`command`,
		`exec`,
		`builtin cd /tmp`,
		`echo f | xargs cat`,
		`xargs -0 cat`,
		`xargs -n 1 cat`,
		`xargs -n1 -r cat`,
		`ls | xargs -I{} cat {}`,
		`xargs`,
		`bash -e -c "ls"`,
		`bash -ec "ls"`,
		`bash '-c' "ls"`,
		`bash -c -e "ls"`,
		`bash -c -- "ls"`,
		`bash -c 'ls' -e`,
		`sh -c 'ls'`,
		`"bash" -c "ls"`,
		`stdbuf`,
		`sudo`,
	} {
		assertApproval(t, rules, cmd, true)
	}
}

// TestWrapper2241_Characterisation_NotApproved: rows refused before and
// after #2241.
func TestWrapper2241_Characterisation_NotApproved(t *testing.T) {
	bash, ro := bashAnyRules(), readonlyRules()
	for _, cmd := range []string{
		`env sed -i s/a/b/ f`,
		`nohup ls`,
	} {
		assertApproval(t, bash, cmd, false)
		assertApproval(t, ro, cmd, false)
	}
	for _, cmd := range []string{
		// Requirement 8: unwrapping never adds approval.
		`command ls`,
		`echo f | xargs cat`,
		// Requirement 6: no rule matches the printed text.
		`"ls" -la`,
		// Requirement 7: go flags already closed by #2736.
		`go vet -toolexec=./x.sh ./...`,
		`go list -toolexec ./x.sh -export .`,
		`go vet --vettool=./x.sh ./...`,
		`go vet --toolexec=./x.sh ./...`,
		`go env -w GOFLAGS=x`,
		`go env -u GOFLAGS`,
		`go env --w GOFLAGS=x`,
		`go -C /tmp env -w GOFLAGS=x`,
	} {
		assertApproval(t, ro, cmd, false)
	}
}

// TestWrapper2241_Characterisation_ReadonlyApproved: requirement 7's "still
// approved" rows, and `bash -c 'ls' x`.
func TestWrapper2241_Characterisation_ReadonlyApproved(t *testing.T) {
	rules := readonlyRules()
	for _, cmd := range []string{
		`go env GOPATH`,
		`go env -json`,
		`go list -m -u all`,
		`find . -name x -print0`,
		`yq '.a' f.yaml`,
		`ss -tlnp`,
		`file f`,
		`file ./*`,
		`rg foo f`,
		`bash -c 'ls' x`,
	} {
		assertApproval(t, rules, cmd, true)
	}
}

// lsWritableEnv makes ~/.local/bin/ls the PATH winner for ls, and writable.
func lsWritableEnv() execguard.Env {
	const shadow = "/home/foci/.local/bin/ls"
	return execguard.Env{
		CanWrite:     func(p string) bool { return p == shadow },
		PathDirs:     []string{"/home/foci/.local/bin", "/usr/bin"},
		IsExecutable: func(p string) bool { return p == shadow || filepath.Dir(p) == "/usr/bin" },
		HomeDir:      "/home/foci",
	}
}

func assertVetoed(t *testing.T, rules []Rule, cmd string) {
	t.Helper()
	ok, reason := MatchWithEnv(rules, "Bash", bashInput(t, cmd), nil)
	if ok {
		t.Errorf("%q: approved; want vetoed (writable ~/.local/bin/ls)", cmd)
		return
	}
	if !strings.Contains(reason, "/home/foci/.local/bin/ls") {
		t.Errorf("%q: reason %q does not name /home/foci/.local/bin/ls", cmd, reason)
	}
}

// TestWrapper2241_VetoJudgesInnerCommand: requirements 1 and 6. The
// substitutability veto judges the command the wrapper runs, and a quoted
// literal name as its visible text. Each row was approved under Bash before
// #2241.
func TestWrapper2241_VetoJudgesInnerCommand(t *testing.T) {
	withGuardEnv(t, lsWritableEnv())
	rules := bashAnyRules()
	for _, cmd := range []string{
		`command ls -la`,
		`exec ls`,
		`echo x | xargs ls`,
		`exec -a x ls`,
		`"ls" -la`,
		`l"s" -la`,
		`'ls'`,
	} {
		assertVetoed(t, rules, cmd)
	}
}

// TestWrapper2241_Characterisation_VetoTypedName: the typed forms were vetoed
// before #2241 too, and the wrapper forms are approved when ls is not
// writable (the control: the veto above is not a blanket refusal).
func TestWrapper2241_Characterisation_VetoTypedName(t *testing.T) {
	rules := bashAnyRules()
	t.Run("writable", func(t *testing.T) {
		withGuardEnv(t, lsWritableEnv())
		assertVetoed(t, rules, `ls -la`)
		assertVetoed(t, rules, `\ls -la`)
	})
	t.Run("read-only", func(t *testing.T) {
		env := lsWritableEnv()
		env.CanWrite = func(string) bool { return false }
		withGuardEnv(t, env)
		for _, cmd := range []string{`command ls -la`, `exec ls`, `echo x | xargs ls`, `"ls" -la`, `l"s" -la`} {
			assertApproval(t, rules, cmd, true)
		}
	})
}

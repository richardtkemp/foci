package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/execguard"
	"foci/internal/shellenv"
)

// guardEnvFor builds a map-backed execguard.Env: PATH directories, writable
// paths, executable files. No test touches the host's real filesystem or
// PATH, and the fake probe below hands this view to runConfigCheckWith, so
// the preview's findings never depend on the host either.
func guardEnvFor(pathDirs, writable, executables []string) execguard.Env {
	w := make(map[string]bool, len(writable))
	for _, p := range writable {
		w[p] = true
	}
	x := make(map[string]bool, len(executables))
	for _, p := range executables {
		x[p] = true
	}
	return execguard.Env{
		CanWrite:     func(p string) bool { return w[p] },
		PathDirs:     pathDirs,
		IsExecutable: func(p string) bool { return x[p] },
		HomeDir:      "/home/foci",
	}
}

// mdsMdqEnv is the ticket's repro in miniature: mds and mdq are executable
// and writable in /home/foci/bin, so the veto refuses them every time.
func mdsMdqEnv() execguard.Env {
	return guardEnvFor(
		[]string{"/home/foci/bin"},
		[]string{"/home/foci/bin/mds", "/home/foci/bin/mdq"},
		[]string{"/home/foci/bin/mds", "/home/foci/bin/mdq"},
	)
}

// delegatedConfigTOML is validConfigTOML with the agent on a delegated
// backend. The built-in groups reach a match-time veto only through
// buildAutoApproveRules, which runs for delegated agents alone
// (agentConsultsAllowlist), so every fixture that expects — or must be seen
// not to suppress — built-in lines needs one.
const delegatedConfigTOML = `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "main"
backend = "claude-code"
`

// envFileOverlays records what planAutoApproveCheck installs as the operator
// population, without touching procx's process-wide state.
type envFileOverlays struct {
	calls []map[string]string
}

// fakeCheckDeps are hermetic checkDeps: never root, no env file loaded, a
// fixed probe view, a recording overlay install. Tests override fields.
func fakeCheckDeps(env execguard.Env) (checkDeps, *envFileOverlays) {
	ov := &envFileOverlays{}
	return checkDeps{
		euid:           func() int { return 1000 },
		loadEnvFile:    func(*string) (map[string]string, string, error) { return nil, "", nil },
		setOperatorEnv: func(m map[string]string) { ov.calls = append(ov.calls, m) },
		probe:          func() execguard.Env { return env },
	}, ov
}

// runConfigCheckDeps runs the check with the given deps and returns the exit
// code plus stderr.
func runConfigCheckDeps(t *testing.T, cfgTOML string, d checkDeps) (int, string) {
	t.Helper()
	path := writeTempConfig(t, cfgTOML)
	var stdout, stderr strings.Builder
	code := runConfigCheckWith(path, &stdout, &stderr, d)
	return code, stderr.String()
}

// TestRunConfigCheck_SubstitutableBuiltinWarned is the ticket's repro: with
// mds and mdq executable and writable on the judge's PATH, and no
// auto_approve entries at all, -check-config still names both as vetoed
// read-only built-ins — with the reason and the path execguard gave — and
// summarises them.
func TestRunConfigCheck_SubstitutableBuiltinWarned(t *testing.T) {
	t.Parallel()
	env := mdsMdqEnv()
	d, _ := fakeCheckDeps(env)

	code, out := runConfigCheckDeps(t, delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warnings never change it); stderr:\n%s", code, out)
	}
	for _, cmd := range []string{"mds", "mdq"} {
		_, path, reason := execguard.Substitutable(cmd, env)
		if path == "" || reason == "" {
			t.Fatalf("control failed: execguard gave no verdict for %s", cmd)
		}
		want := `config check warning: built-in read-only group: command "` + cmd + `" will be vetoed at match time — `
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks the %s line prefix %q:\n%s", cmd, want, out)
		}
		if i := strings.Index(out, want); i >= 0 {
			line := out[i : strings.IndexByte(out[i:], '\n')+i]
			if !strings.Contains(line, reason) || !strings.Contains(line, path) {
				t.Errorf("%s line must carry execguard's reason %q and path %q:\n%s", cmd, reason, path, line)
			}
		}
	}
	if want := "2 command(s) will stop auto-approving under this config: mdq, mds"; !strings.Contains(out, want) {
		t.Errorf("stderr lacks summary %q (sorted tokens):\n%s", want, out)
	}
}

// TestRunConfigCheck_WritableEntryWarnedAndSummarised proves substitutable
// config entries are named per scope — the global block as global, the agent
// as agents[id] — with the reason and path, and that the summary collects
// the failing command tokens sorted.
func TestRunConfigCheck_WritableEntryWarnedAndSummarised(t *testing.T) {
	t.Parallel()
	// PATH carries no executables, so the built-in groups stay silent and
	// the test judges the entries alone.
	d, _ := fakeCheckDeps(guardEnvFor([]string{"/usr/bin"}, []string{"/bad/x.sh", "/bad/a.sh"}, nil))

	code, out := runConfigCheckDeps(t, validConfigTOML+`
[permissions]
auto_approve = ["Bash:/bad/x.sh *"]

[[agents]]
id = "clutch"

[agents.permissions]
auto_approve = ["Bash:/bad/a.sh *"]
`, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	for _, row := range []struct{ scope, rule, cmd string }{
		{"global", "Bash:/bad/x.sh *", "/bad/x.sh"},
		{"agents[clutch]", "Bash:/bad/a.sh *", "/bad/a.sh"},
	} {
		want := `config check warning: ` + row.scope + `: auto_approve entry "` + row.rule + `" will be ignored at startup — `
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
			continue
		}
		i := strings.Index(out, want)
		line := out[i : strings.IndexByte(out[i:], '\n')+i]
		if !strings.Contains(line, "("+row.cmd+")") {
			t.Errorf("%s line must name the path %s:\n%s", row.scope, row.cmd, line)
		}
		if !strings.Contains(line, "writable by the foci process") {
			t.Errorf("%s line must carry execguard's reason:\n%s", row.scope, line)
		}
	}
	if want := "2 command(s) will stop auto-approving under this config: /bad/a.sh, /bad/x.sh"; !strings.Contains(out, want) {
		t.Errorf("stderr lacks summary %q:\n%s", want, out)
	}
}

// TestRunConfigCheck_UnlocatableEntryApprovedUnchecked proves an entry whose
// command is on no directory of PATH is named as approved UNCHECKED, and
// that nothing stops auto-approving for it (no summary line).
func TestRunConfigCheck_UnlocatableEntryApprovedUnchecked(t *testing.T) {
	t.Parallel()
	d, _ := fakeCheckDeps(guardEnvFor([]string{"/usr/bin"}, nil, []string{"/usr/bin/git"}))

	code, out := runConfigCheckDeps(t, validConfigTOML+`
[permissions]
auto_approve = ["Bash:mds *"]
`, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	want := `config check warning: global: auto_approve entry "Bash:mds *" names "mds", which is on no directory of PATH; it will be approved unchecked`
	if !strings.Contains(out, want) {
		t.Errorf("stderr lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "will stop auto-approving") {
		t.Errorf("an unlocatable command does not stop auto-approving; summary printed:\n%s", out)
	}
}

// TestRunConfigCheck_AutoApproveWarningsAfterBackendConfigBeforePretool pins
// the ordering: the auto-approve preview sits between the backend_config
// warning and the pretool example lines, and never changes the exit code for
// its own findings (the 1 here comes from the pretool failure fixture).
func TestRunConfigCheck_AutoApproveWarningsAfterBackendConfigBeforePretool(t *testing.T) {
	t.Parallel()
	d, _ := fakeCheckDeps(guardEnvFor([]string{"/usr/bin"}, []string{"/bad/x.sh"}, nil))

	code, out := runConfigCheckDeps(t, validConfigTOML+`
[permissions]
auto_approve = ["Bash:/bad/x.sh *"]

[[agents]]
id = "cc"
backend = "claude-code"

[agents.backend_config]
hostname = "0.0.0.0"

[[agents.backend_config.pretool_rules]]
name = "no_rm"
tool = "Bash"
command = 'rm( |$)'
deny_examples = ["ls"]
reason = "r"
`, d)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (the pretool failure, not the warnings); stderr:\n%s", code, out)
	}
	iBackend := strings.Index(out, "backend_config")
	iEntry := strings.Index(out, "auto_approve entry")
	iPretool := strings.Index(out, `pretool_rules "no_rm": deny example`)
	if iBackend < 0 || iEntry < 0 || iPretool < 0 {
		t.Fatalf("missing a section: backend=%d entry=%d pretool=%d\n%s", iBackend, iEntry, iPretool, out)
	}
	if !(iBackend < iEntry && iEntry < iPretool) {
		t.Errorf("auto-approve warnings must sit between backend_config and pretool; got backend=%d entry=%d pretool=%d\n%s", iBackend, iEntry, iPretool, out)
	}
}

// TestRunConfigCheck_AgentTurnsReadonlyOn_GivesBuiltinLine proves a group
// disabled globally is still reported when one agent turns it on: that
// agent's shells run the group, so the veto hits its commands.
func TestRunConfigCheck_AgentTurnsReadonlyOn_GivesBuiltinLine(t *testing.T) {
	t.Parallel()
	d, _ := fakeCheckDeps(mdsMdqEnv())

	_, out := runConfigCheckDeps(t, `[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[permissions]
auto_approve_common_readonly = false

[[agents]]
id = "reader"
backend = "claude-code"

[agents.permissions]
auto_approve_common_readonly = true
`, d)
	if !strings.Contains(out, `built-in read-only group: command "mds"`) {
		t.Errorf("a per-agent true under a global false must give the builtin line:\n%s", out)
	}
}

// TestRunConfigCheck_APIOnlyAndSkipAgents_NoBuiltinLines pins the serving
// condition: the built-in groups reach a match-time veto only through
// buildAutoApproveRules, which runs for delegated agents whose backends
// still ask for approval. A config of API agents only, or of delegated
// agents with skip_permissions (which never prompt), has no shell the groups
// are live in — veto lines for it would be false, and there is no summary
// because nothing stops auto-approving.
func TestRunConfigCheck_APIOnlyAndSkipAgents_NoBuiltinLines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		toml string
	}{
		{"API-only config", validConfigTOML},
		{"API agent turns read-only on under a global off", `
[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[permissions]
auto_approve_common_readonly = false

[[agents]]
id = "main"

[agents.permissions]
auto_approve_common_readonly = true
`},
		{"skip_permissions agent", delegatedConfigTOML + `
[agents.backend_config]
skip_permissions = true
`},
	}
	for _, tc := range cases {
		d, _ := fakeCheckDeps(mdsMdqEnv())
		code, out := runConfigCheckDeps(t, tc.toml, d)
		if code != 0 {
			t.Fatalf("%s: exit = %d, want 0; stderr:\n%s", tc.name, code, out)
		}
		if strings.Contains(out, "built-in") {
			t.Errorf("%s: no shell of these agents consults the built-in groups, so no veto line may appear:\n%s", tc.name, out)
		}
		if strings.Contains(out, "will stop auto-approving under this config") {
			t.Errorf("%s: nothing stops auto-approving, so there is no summary:\n%s", tc.name, out)
		}
	}
}

// TestRunConfigCheck_SharedCommandNamedOncePerGroup pins the per-command
// report on the -check-config side: six read-only rules share the token
// `go`, the veto judges the token, so exactly one line names it.
func TestRunConfigCheck_SharedCommandNamedOncePerGroup(t *testing.T) {
	t.Parallel()
	// Only go is executable-and-writable on the fake PATH: mds/mdq stay
	// silent, so the count is about go alone.
	env := guardEnvFor(
		[]string{"/home/foci/bin"},
		[]string{"/home/foci/bin/go"},
		[]string{"/home/foci/bin/go"},
	)
	d, _ := fakeCheckDeps(env)

	code, out := runConfigCheckDeps(t, delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	if got := strings.Count(out, `built-in read-only group: command "go" will be vetoed`); got != 1 {
		t.Errorf("go named %d time(s) in the veto lines, want exactly 1 (six rules share the token):\n%s", got, out)
	}
	if want := "1 command(s) will stop auto-approving under this config: go"; !strings.Contains(out, want) {
		t.Errorf("stderr lacks summary %q:\n%s", want, out)
	}
}

// TestRunConfigCheck_SafeWriteEnabled_WritableCurlNamed proves the safe-write
// group is reported under its own name when enabled and its command is
// substitutable.
func TestRunConfigCheck_SafeWriteEnabled_WritableCurlNamed(t *testing.T) {
	t.Parallel()
	env := guardEnvFor(
		[]string{"/home/foci/bin", "/usr/bin"},
		[]string{"/home/foci/bin/curl"},
		[]string{"/home/foci/bin/curl"},
	)
	d, _ := fakeCheckDeps(env)

	_, out := runConfigCheckDeps(t, delegatedConfigTOML+`
[permissions]
auto_approve_common_safe_write = true
`, d)
	want := `config check warning: built-in safe-write group: command "curl" will be vetoed at match time — `
	if !strings.Contains(out, want) {
		t.Errorf("stderr lacks the safe-write curl line %q:\n%s", want, out)
	}
	if i := strings.Index(out, want); i >= 0 {
		line := out[i : strings.IndexByte(out[i:], '\n')+i]
		if !strings.Contains(line, "/home/foci/bin/curl") || !strings.Contains(line, "writable by the foci process") {
			t.Errorf("safe-write line must carry the path and reason:\n%s", line)
		}
	}
}

// TestRunConfigCheck_AgentNamedGlobalLabeledAsAgent pins requirement 4's
// check-config side: an agent whose id is "global" is labelled agents[global],
// and the global block is still labelled global — the two never collide.
func TestRunConfigCheck_AgentNamedGlobalLabeledAsAgent(t *testing.T) {
	t.Parallel()
	d, _ := fakeCheckDeps(guardEnvFor([]string{"/usr/bin"}, []string{"/bad/g.sh", "/bad/a.sh"}, nil))

	_, out := runConfigCheckDeps(t, `[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[permissions]
auto_approve = ["Bash:/bad/g.sh *"]

[[agents]]
id = "global"

[agents.permissions]
auto_approve = ["Bash:/bad/a.sh *"]
`, d)
	if !strings.Contains(out, `config check warning: global: auto_approve entry "Bash:/bad/g.sh *"`) {
		t.Errorf("the global block must be labelled global:\n%s", out)
	}
	if !strings.Contains(out, `config check warning: agents[global]: auto_approve entry "Bash:/bad/a.sh *"`) {
		t.Errorf("the agent named global must be labelled as that agent:\n%s", out)
	}
}

// TestRunConfigCheck_RootSkipsAutoApproveNeverRunsEnvFile pins the root rule:
// exactly one warning, nothing probed, and the env file never sourced — not
// even attempted, which is what the never-called recording loader proves.
func TestRunConfigCheck_RootSkipsAutoApproveNeverRunsEnvFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	rc := filepath.Join(dir, "rc")
	if err := os.WriteFile(rc, []byte("touch '"+marker+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ov := &envFileOverlays{}
	loaded := 0
	d := checkDeps{
		euid: func() int { return 0 },
		loadEnvFile: func(*string) (map[string]string, string, error) {
			loaded++
			env, err := shellenv.Capture(rc)
			return env, "", err
		},
		setOperatorEnv: func(m map[string]string) { ov.calls = append(ov.calls, m) },
		probe:          func() execguard.Env { return mdsMdqEnv() },
	}

	code, out := runConfigCheckDeps(t, "shell_env_file = '"+rc+"'\n"+delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	if got := strings.Count(out, "auto_approve was not checked"); got != 1 {
		t.Fatalf("%d not-checked warnings, want exactly 1:\n%s", got, out)
	}
	for _, want := range []string{"runs as root", "as the service user"} {
		if !strings.Contains(out, want) {
			t.Errorf("root warning lacks %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"vetoed", "will be ignored", "command(s) will stop auto-approving under this config"} {
		if strings.Contains(out, banned) {
			t.Errorf("root case must not probe; found %q:\n%s", banned, out)
		}
	}
	if loaded != 0 {
		t.Errorf("the env file was loaded %d time(s); as root it must never be", loaded)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the env file ran (marker exists): as root it must never be sourced")
	}
	if len(ov.calls) != 0 {
		t.Errorf("overlay installed %d time(s); as root nothing is derived", len(ov.calls))
	}
}

// TestRunConfigCheck_UntrustedEnvFileNotCheckedNeverRun pins the non-root
// fail-closed rule end to end through the real gate: a world-writable env
// file is neither sourced nor probed, and the single warning names the
// concrete reason and the full refusal rule.
func TestRunConfigCheck_UntrustedEnvFileNotCheckedNeverRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	rc := filepath.Join(dir, "rc")
	if err := os.WriteFile(rc, []byte("touch '"+marker+"'\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rc, 0o666); err != nil { // umask-proof the mode
		t.Fatal(err)
	}
	ov := &envFileOverlays{}
	d := checkDeps{
		euid:           func() int { return 1000 },
		loadEnvFile:    prodCheckDeps.loadEnvFile, // the real Resolve+gate+Capture composition
		setOperatorEnv: func(m map[string]string) { ov.calls = append(ov.calls, m) },
		probe:          func() execguard.Env { return mdsMdqEnv() },
	}

	code, out := runConfigCheckDeps(t, "shell_env_file = '"+rc+"'\n"+delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	if got := strings.Count(out, "auto_approve was not checked"); got != 1 {
		t.Fatalf("%d not-checked warnings, want exactly 1:\n%s", got, out)
	}
	for _, want := range []string{"world-writable", rc, "group-writable", "symlink", `".."`, "not a relative path"} {
		if !strings.Contains(out, want) {
			t.Errorf("not-checked warning lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "vetoed") {
		t.Errorf("an untrusted env file must not be probed; found veto lines:\n%s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the env file ran (marker exists): an untrusted file must never be sourced")
	}
	if len(ov.calls) != 0 {
		t.Errorf("overlay installed %d time(s); a refused check derives nothing", len(ov.calls))
	}
}

// TestRunConfigCheck_RelativeEnvFileNeverRunsPathFile pins the bare-relative
// rule: bash sources the first PATH match for a name with no slash, so the
// check refuses to judge — and never runs the PATH file.
func TestRunConfigCheck_RelativeEnvFileNeverRunsPathFile(t *testing.T) {
	// Not parallel: it repoints PATH for the duration.
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	rc := filepath.Join(dir, "checkrc.sh")
	if err := os.WriteFile(rc, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ov := &envFileOverlays{}
	d := checkDeps{
		euid:           func() int { return 1000 },
		loadEnvFile:    prodCheckDeps.loadEnvFile,
		setOperatorEnv: func(m map[string]string) { ov.calls = append(ov.calls, m) },
		probe:          func() execguard.Env { return mdsMdqEnv() },
	}

	code, out := runConfigCheckDeps(t, "shell_env_file = \"checkrc.sh\"\n"+delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	if got := strings.Count(out, "auto_approve was not checked"); got != 1 {
		t.Fatalf("%d not-checked warnings, want exactly 1:\n%s", got, out)
	}
	for _, want := range []string{"bare relative name", "checkrc.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("not-checked warning lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the PATH file ran (marker exists): a bare relative name must never be sourced")
	}
	if strings.Contains(out, "vetoed") {
		t.Errorf("a refused check must not probe:\n%s", out)
	}
}

// TestRunConfigCheck_RelativeSlashEnvFileNotCheckedNeverRun pins the
// relative-with-slash rule end to end through the real loader composition:
// bash's "." resolves "rcd/rc" against the cwd, but the daemon's shells run
// from the service's WorkingDirectory, not the check's. Judging it against
// the check's cwd made the verdict depend on where the operator stood (one
// cwd sourced the file, another called it missing and claimed that "is also
// what the daemon would run with"). So it is refused: never sourced — even
// when a tight file sits at that path under the check's cwd — nothing is
// probed, and the single warning says why.
func TestRunConfigCheck_RelativeSlashEnvFileNotCheckedNeverRun(t *testing.T) {
	// Not parallel: it changes the working directory for the duration.
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	rcd := filepath.Join(dir, "rcd")
	if err := os.Mkdir(rcd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rcd, "rc"), []byte("touch '"+marker+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { // umask-proof the chain
		t.Fatal(err)
	}
	t.Chdir(dir)
	// A tight chain judged as its real owner up to dir: every refusal the
	// trust walk has stays silent, so only the relative-path rule can stop
	// the source.
	uid := os.Getuid()
	gate := envFileGate{euid: func() int { return uid }, walkRoot: dir}
	ov := &envFileOverlays{}
	d := checkDeps{
		euid: func() int { return uid },
		loadEnvFile: func(cfg *string) (map[string]string, string, error) {
			return classifyEnvFile(shellenv.LoadGated(cfg, gate.check))
		},
		setOperatorEnv: func(m map[string]string) { ov.calls = append(ov.calls, m) },
		probe:          func() execguard.Env { return mdsMdqEnv() },
	}

	code, out := runConfigCheckDeps(t, "shell_env_file = \"rcd/rc\"\n"+delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	if got := strings.Count(out, "auto_approve was not checked"); got != 1 {
		t.Fatalf("%d not-checked warnings, want exactly 1:\n%s", got, out)
	}
	for _, want := range []string{"relative path", `"rcd/rc"`, "WorkingDirectory"} {
		if !strings.Contains(out, want) {
			t.Errorf("not-checked warning lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "vetoed") {
		t.Errorf("a refused check must not probe:\n%s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the env file ran (marker exists): a relative path must never be sourced")
	}
	if len(ov.calls) != 0 {
		t.Errorf("overlay installed %d time(s); a refused check derives nothing", len(ov.calls))
	}
}

// TestRunConfigCheck_DotDotEnvFileNotCheckedNeverRun pins the review's
// escape, end to end through the real loader composition: a shell_env_file
// with a ".." after a symlinked component names a file in a directory the
// lexical trust walk never visits — the kernel resolves ".." AFTER symlinks,
// while filepath.Dir cleans "lnk/.." away — so the gate refuses ".."
// outright: the file is never sourced, nothing is probed, and the single
// warning says why.
func TestRunConfigCheck_DotDotEnvFileNotCheckedNeverRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	// The real rc lives in a group-writable directory the WRITTEN path never
	// names. The kernel resolves ".." AFTER symlinks: trusted/lnk/../rc is
	// gwdir/rc (lnk -> gwdir/sub, then ".." -> gwdir), so that is the file
	// the shells run and where the marker-writing rc must sit. Before the
	// refusal the walk judged only dir and dir/trusted (both tight) and this
	// file was sourced — with walkRoot=dir this fixture reproduces that.
	gw := filepath.Join(dir, "gwdir")
	sub := filepath.Join(gw, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(gw, "rc")
	if err := os.WriteFile(rc, []byte("touch '"+marker+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gw, 0o770); err != nil { // umask-proof the mode
		t.Fatal(err)
	}
	trusted := filepath.Join(dir, "trusted")
	if err := os.Mkdir(trusted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sub, filepath.Join(trusted, "lnk")); err != nil {
		t.Fatal(err)
	}
	written := trusted + "/lnk/../rc"
	// Judged as the fixture's real owner (files are created as os.Getuid on
	// every host, root included), so the pre-fix walk had no ownership arm
	// to refuse on and the file really was sourced — that is the escape
	// this test pins; the ".." refusal fires before any ownership check.
	uid := os.Getuid()
	gate := envFileGate{
		euid:     func() int { return uid },
		walkRoot: dir,
	}
	ov := &envFileOverlays{}
	d := checkDeps{
		euid: func() int { return uid },
		loadEnvFile: func(cfg *string) (map[string]string, string, error) {
			return classifyEnvFile(shellenv.LoadGated(cfg, gate.check))
		},
		setOperatorEnv: func(m map[string]string) { ov.calls = append(ov.calls, m) },
		probe:          func() execguard.Env { return mdsMdqEnv() },
	}

	code, out := runConfigCheckDeps(t, "shell_env_file = '"+written+"'\n"+delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	if got := strings.Count(out, "auto_approve was not checked"); got != 1 {
		t.Fatalf("%d not-checked warnings, want exactly 1:\n%s", got, out)
	}
	for _, want := range []string{`".."`, written} {
		if !strings.Contains(out, want) {
			t.Errorf("not-checked warning lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "vetoed") {
		t.Errorf("a refused check must not probe:\n%s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the env file ran (marker exists): a \"..\" path must never be sourced")
	}
	if len(ov.calls) != 0 {
		t.Errorf("overlay installed %d time(s); a refused check derives nothing", len(ov.calls))
	}
}

// TestRunConfigCheck_TrustedEnvFileCapturedAndProbed pins the trusted path:
// the env file is sourced (marker), its captured values are installed as the
// operator population — before the probe runs, which is what makes the
// preview judge the PATH the shells will search — and the probe's findings
// are reported. The gate's allow-arm cannot run hermetically (its walk
// reaches /, and /tmp is world-writable on every test host), so this drives
// Capture directly; the gate itself is covered by the refusal tests above
// and TestEnvFileConcern.
func TestRunConfigCheck_TrustedEnvFileCapturedAndProbed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	rc := filepath.Join(dir, "rc")
	if err := os.WriteFile(rc, []byte("export FOCI_TEST_RC_VAR=from-rc\ntouch '"+marker+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ov := &envFileOverlays{}
	probedWith := -1
	d := checkDeps{
		euid: func() int { return 1000 },
		loadEnvFile: func(*string) (map[string]string, string, error) {
			env, err := shellenv.Capture(rc)
			return env, "", err
		},
		setOperatorEnv: func(m map[string]string) { ov.calls = append(ov.calls, m) },
		probe:          func() execguard.Env { probedWith = len(ov.calls); return mdsMdqEnv() },
	}

	code, out := runConfigCheckDeps(t, "shell_env_file = '"+rc+"'\n"+delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	if strings.Contains(out, "auto_approve was not checked") {
		t.Errorf("a trusted env file must not warn:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the trusted env file never ran: %v", err)
	}
	if len(ov.calls) != 1 {
		t.Fatalf("overlay installed %d time(s), want 1", len(ov.calls))
	}
	if got := ov.calls[0]["FOCI_TEST_RC_VAR"]; got != "from-rc" {
		t.Errorf("overlay carries FOCI_TEST_RC_VAR=%q, want the value the rc exported", got)
	}
	if probedWith != 1 {
		t.Errorf("probe saw %d overlay install(s), want 1: the overlay must be installed before the probe reads it", probedWith)
	}
	if !strings.Contains(out, `built-in read-only group: command "mds"`) {
		t.Errorf("the probe's findings must be reported:\n%s", out)
	}
}

// TestRunConfigCheck_MissingEnvFileWarnsUnsourcedStillProbes pins the mirror
// with the real loader: a configured env file that does not exist is never
// sourced and does not refuse the check — startup's capture of it yields no
// overlay either, so the preview probes the same PATH — but it is no longer
// SILENT: the operator configured an rc file, and the PATH judged (and the
// daemon's) has no overlay from it.
func TestRunConfigCheck_MissingEnvFileWarnsUnsourcedStillProbes(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "nope.rc")
	ov := &envFileOverlays{}
	d := checkDeps{
		euid:           func() int { return 1000 },
		loadEnvFile:    prodCheckDeps.loadEnvFile,
		setOperatorEnv: func(m map[string]string) { ov.calls = append(ov.calls, m) },
		probe:          func() execguard.Env { return mdsMdqEnv() },
	}

	code, out := runConfigCheckDeps(t, "shell_env_file = '"+missing+"'\n"+delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	if strings.Contains(out, "auto_approve was not checked") {
		t.Errorf("a missing env file is ordinary, not a refusal:\n%s", out)
	}
	if want := "config check warning: the shell env file " + missing + " does not exist — the preview judges the PATH without it"; !strings.Contains(out, want) {
		t.Errorf("stderr lacks the unsourced warning %q:\n%s", want, out)
	}
	if !strings.Contains(out, `built-in read-only group: command "mds"`) {
		t.Errorf("the preview must still probe:\n%s", out)
	}
	if len(ov.calls) != 1 || ov.calls[0] != nil {
		t.Errorf("overlay calls = %v, want one nil install (populations identical)", ov.calls)
	}
}

// TestClassifyEnvFile pins the one place LoadGated's returns become the
// plan's three states: refusal beats everything, a loaded overlay installs
// quietly, a configured-but-unsourced file is reported (missing or capture
// failure — startup gets no overlay from either), and only
// nothing-configured stays silent.
func TestClassifyEnvFile(t *testing.T) {
	t.Parallel()
	refusal := errors.New("gate refusal")

	env, unsourced, refused := classifyEnvFile(map[string]string{"PATH": "x"}, "/rc", true, nil)
	if env == nil || unsourced != "" || refused != nil {
		t.Errorf("loaded = (%v, %q, %v), want the overlay, silent, allowed", env != nil, unsourced, refused)
	}

	env, unsourced, refused = classifyEnvFile(nil, "/rc", false, fmt.Errorf("wrapped: %w", errEnvFileMissing))
	if env != nil || unsourced != "/rc does not exist" || refused != nil {
		t.Errorf("missing = (%v, %q, %v), want nil, reported, not a refusal", env != nil, unsourced, refused)
	}

	env, unsourced, refused = classifyEnvFile(nil, "/rc", false, nil)
	if env != nil || unsourced != "/rc could not be sourced (startup logs the capture failure; the same overlay is missing there)" || refused != nil {
		t.Errorf("capture failure = (%v, %q, %v), want nil, reported, not a refusal", env != nil, unsourced, refused)
	}

	env, unsourced, refused = classifyEnvFile(nil, "", false, refusal)
	if env != nil || unsourced != "" || refused == nil {
		t.Errorf("refusal = (%v, %q, %v), want nil, silent, the refusal passed through", env != nil, unsourced, refused)
	}

	env, unsourced, refused = classifyEnvFile(nil, "", false, nil)
	if env != nil || unsourced != "" || refused != nil {
		t.Errorf("nothing to load = (%v, %q, %v), want fully silent", env != nil, unsourced, refused)
	}
}

// TestRunConfigCheck_NothingLoadedProbesQuietly pins the other silent
// branch of the one derivation (no env file configured): to the plan it
// looks like "no overlay", and the preview quietly probes — exactly what
// startup does in that state.
func TestRunConfigCheck_NothingLoadedProbesQuietly(t *testing.T) {
	t.Parallel()
	ov := &envFileOverlays{}
	d := checkDeps{
		euid:           func() int { return 1000 },
		loadEnvFile:    func(*string) (map[string]string, string, error) { return nil, "", nil },
		setOperatorEnv: func(m map[string]string) { ov.calls = append(ov.calls, m) },
		probe:          func() execguard.Env { return mdsMdqEnv() },
	}

	code, out := runConfigCheckDeps(t, delegatedConfigTOML, d)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, out)
	}
	if strings.Contains(out, "auto_approve was not checked") {
		t.Errorf("nothing-to-load is ordinary, not a refusal:\n%s", out)
	}
	if strings.Contains(out, "shell env file") {
		t.Errorf("nothing-to-load must stay silent (no file was configured):\n%s", out)
	}
	if !strings.Contains(out, `built-in read-only group: command "mds"`) {
		t.Errorf("the preview must still probe:\n%s", out)
	}
	if len(ov.calls) != 1 || ov.calls[0] != nil {
		t.Errorf("overlay calls = %v, want one nil install (populations identical)", ov.calls)
	}
}

// TestRunConfigCheck_ReadonlyDisabledEverywhere_NoBuiltinLine is a
// characterisation test (listed in .factory/red-gate-exempt.txt): a group
// disabled everywhere prints nothing on either side of the change. It pins
// that disabling really silences the report, for a live (delegated) agent.
func TestRunConfigCheck_ReadonlyDisabledEverywhere_NoBuiltinLine(t *testing.T) {
	t.Parallel()
	d, _ := fakeCheckDeps(mdsMdqEnv())

	_, out := runConfigCheckDeps(t, delegatedConfigTOML+`
[permissions]
auto_approve_common_readonly = false
`, d)
	if strings.Contains(out, "built-in") {
		t.Errorf("a disabled group must not be reported:\n%s", out)
	}
}

// TestRunConfigCheck_SafeWriteDisabledByDefault_NoSafeWriteLine is a
// characterisation test (listed in .factory/red-gate-exempt.txt): safe-write
// defaults off, so a substitutable curl prints nothing on either side of the
// change.
func TestRunConfigCheck_SafeWriteDisabledByDefault_NoSafeWriteLine(t *testing.T) {
	t.Parallel()
	env := guardEnvFor(
		[]string{"/home/foci/bin"},
		[]string{"/home/foci/bin/curl"},
		[]string{"/home/foci/bin/curl"},
	)
	d, _ := fakeCheckDeps(env)

	_, out := runConfigCheckDeps(t, delegatedConfigTOML, d)
	if strings.Contains(out, "safe-write") {
		t.Errorf("safe-write off must not be reported:\n%s", out)
	}
}

// TestEnvFileConcern pins the trust walk on fully controlled chains — one
// per row, since rows mutate modes: every reason the walk can give, and the
// tight case that gives none. The walk stops at the chain's root so no row
// depends on the ancestors of the temp dir.
func TestEnvFileConcern(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	uid := os.Getuid()
	stranger := uid + 1234

	// chain builds dir/<name>/a/b/rc, tight by construction.
	chain := func(t *testing.T, name string) string {
		sub := filepath.Join(dir, name, "a", "b")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		rc := filepath.Join(sub, "rc")
		if err := os.WriteFile(rc, []byte("export X=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(rc, 0o600); err != nil { // umask-proof
			t.Fatal(err)
		}
		return rc
	}

	tests := []struct {
		name  string
		setup func(t *testing.T) (path string, uid int, stopAt string)
		want  string // substring of the concern; "" = trusted
	}{
		{"own tight chain is trusted", func(t *testing.T) (string, int, string) {
			rc := chain(t, "tight")
			return rc, uid, filepath.Dir(filepath.Dir(filepath.Dir(rc)))
		}, ""},
		{"group-writable file", func(t *testing.T) (string, int, string) {
			rc := chain(t, "groupfile")
			if err := os.Chmod(rc, 0o660); err != nil {
				t.Fatal(err)
			}
			return rc, uid, dir
		}, "is group-writable (mode 0660)"},
		{"world-writable file", func(t *testing.T) (string, int, string) {
			rc := chain(t, "worldfile")
			if err := os.Chmod(rc, 0o666); err != nil {
				t.Fatal(err)
			}
			return rc, uid, dir
		}, "is world-writable (mode 0666)"},
		{"another user's file", func(t *testing.T) (string, int, string) {
			rc := chain(t, "foreign")
			if os.Geteuid() == 0 {
				// As root every created file is root-owned, and root is
				// always allowed; make one genuinely foreign instead.
				if err := os.Chown(rc, 12345, -1); err != nil {
					t.Skipf("cannot chown as root: %v", err)
				}
				return rc, 12345, dir // judged as a non-root uid: the file's owner is neither it nor root
			}
			return rc, stranger, dir
		}, "is owned by uid"},
		{"symlinked file", func(t *testing.T) (string, int, string) {
			rc := chain(t, "symlink")
			link := rc + ".link"
			if err := os.Symlink(rc, link); err != nil {
				t.Fatal(err)
			}
			return link, uid, dir
		}, "is a symlink"},
		{"world-writable directory on the path", func(t *testing.T) (string, int, string) {
			rc := chain(t, "worlddir")
			if err := os.Chmod(filepath.Dir(filepath.Dir(rc)), 0o777); err != nil {
				t.Fatal(err)
			}
			return rc, uid, dir
		}, "is world-writable (mode 0777)"},
		{"group-writable directory on the path", func(t *testing.T) (string, int, string) {
			rc := chain(t, "groupdir")
			if err := os.Chmod(filepath.Dir(filepath.Dir(rc)), 0o770); err != nil {
				t.Fatal(err)
			}
			return rc, uid, dir
		}, "is group-writable (mode 0770)"},
		{"setgid directory keeps its full mode in the report", func(t *testing.T) (string, int, string) {
			rc := chain(t, "setgid")
			// A setgid group-writable home (mode 2770) must be reported as
			// 2770: Perm() alone would print 0770, and an operator fixing
			// exactly the reported mode would not have fixed the directory.
			home := filepath.Dir(filepath.Dir(rc))
			if err := os.Chmod(home, os.FileMode(0o770)|os.ModeSetgid); err != nil {
				t.Skipf("cannot set the setgid bit here (sandboxed or unprivileged): %v", err)
			}
			if fi, err := os.Stat(home); err != nil || fi.Mode()&os.ModeSetgid == 0 {
				t.Skip("this filesystem or sandbox drops the setgid bit; the 2770 case cannot be set up")
			}
			return rc, uid, dir
		}, "is group-writable (mode 2770)"},
		{"inspectable-error component", func(t *testing.T) (string, int, string) {
			rc := chain(t, "notdir")
			// A non-directory mid-path: Lstat fails with ENOTDIR, not
			// ENOENT, and the walk must refuse rather than guess.
			return filepath.Join(rc, "sub", "x"), uid, dir
		}, "cannot inspect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, judgeAs, stopAt := tt.setup(t)
			got := envFileConcern(path, judgeAs, stopAt)
			if tt.want == "" {
				if got != "" {
					t.Errorf("concern = %q, want trusted (\"\")", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("concern = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

// TestEnvFileGate_AllArmsHermetic drives the gate itself — the composition
// of the relative-path refusals, the missing-file sentinel and the trust
// walk — with injected euid and walkRoot, so its ALLOW arm is covered
// without depending on the host's real uid or the ancestors of the temp
// directory.
func TestEnvFileGate_AllArmsHermetic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	uid := os.Getuid()
	sub := filepath.Join(dir, "g", "a")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(sub, "rc")
	if err := os.WriteFile(rc, []byte("export X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gate := envFileGate{
		euid:     func() int { return uid },
		walkRoot: dir,
	}

	// ALLOW: a tight chain passes through unchanged, judged to its root.
	got, err := gate.check(rc)
	if err != nil || got != rc {
		t.Errorf("allow = (%q, %v), want (%q, nil)", got, err, rc)
	}

	// Relative-with-slash refuses too: bash's "." resolves it against the
	// cwd, and the daemon's shells run from its systemd WorkingDirectory,
	// not the check's — so no cwd the check could pick is known to be the
	// daemon's, and the file judged would not be the file startup runs.
	if _, err := gate.check(filepath.Join("g", "a", "rc")); err == nil || !strings.Contains(err.Error(), "relative path") {
		t.Errorf("relative with slash = %v, want the relative-path refusal", err)
	}

	// Bare relative names refuse: bash searches PATH for those.
	if _, err := gate.check("rc"); err == nil || !strings.Contains(err.Error(), "bare relative name") {
		t.Errorf("bare relative = %v, want the bare-relative refusal", err)
	}

	// A missing file returns the sentinel, not a trust refusal.
	if _, err := gate.check(filepath.Join(dir, "nope.rc")); !errors.Is(err, errEnvFileMissing) {
		t.Errorf("missing = %v, want errEnvFileMissing", err)
	}

	// A group-writable file on the chain refuses with the full rule.
	loose := filepath.Join(sub, "loose")
	if err := os.WriteFile(loose, []byte("export X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o660); err != nil { // umask-proof the mode
		t.Fatal(err)
	}
	if _, err := gate.check(loose); err == nil || !strings.Contains(err.Error(), envFileRefusalRule) {
		t.Errorf("loose file = %v, want the trust refusal with the full rule", err)
	}

	// ".." components refuse however they arise: the kernel resolves ".."
	// after symlinks, so the written path can name a directory the walk
	// never sees. The escape itself: dir/lnk/../rc2 is physically
	// dir/gwdir/rc2 (lnk -> dir/gwdir/sub, ".." -> gwdir), and the walk
	// would judge dir and dir/lnk only — never the group-writable gwdir.
	if err := os.MkdirAll(filepath.Join(dir, "gwdir", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	esc := filepath.Join(dir, "gwdir", "rc2")
	if err := os.WriteFile(esc, []byte("export X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "gwdir"), 0o770); err != nil { // umask-proof the mode
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gwdir", "sub"), filepath.Join(dir, "lnk")); err != nil {
		t.Fatal(err)
	}
	// Literal strings, not filepath.Join: Join cleans ".." away, which is
	// exactly the difference under test.
	for _, dd := range []string{
		filepath.Join(dir, "lnk") + "/../rc2", // the escape: real dir never walked
		"g/../rc",                             // relative form Join would clean away
		sub + "/../../rc",                     // plain ..: refused even with no symlink
	} {
		if _, err := gate.check(dd); err == nil || !strings.Contains(err.Error(), `".."`) {
			t.Errorf("check(%q) = %v, want the \"..\" refusal", dd, err)
		}
	}
}

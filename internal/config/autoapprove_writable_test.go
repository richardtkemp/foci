package config

import (
	"os"
	"path/filepath"
	"testing"
)

// testEnv builds an execEnv from explicit sets, so no test touches the host's
// real filesystem or PATH.
func testEnv(pathDirs []string, writable []string, executables []string) execEnv {
	w := make(map[string]bool, len(writable))
	for _, p := range writable {
		w[p] = true
	}
	x := make(map[string]bool, len(executables))
	for _, p := range executables {
		x[p] = true
	}
	return execEnv{
		canWrite:     func(p string) bool { return w[p] },
		pathDirs:     pathDirs,
		isExecutable: func(p string) bool { return x[p] },
		homeDir:      "/home/foci",
	}
}

// readOnlyPath mimics a hardened host: no writable directory anywhere on PATH.
func readOnlyPath() execEnv {
	return testEnv(
		[]string{"/usr/local/bin", "/usr/bin"},
		nil,
		[]string{"/usr/bin/git", "/usr/bin/sqlite3", "/usr/local/bin/gh"},
	)
}

// shimmedPath mimics Dick's host: a writable ~/.local/bin ahead of /usr/bin.
func shimmedPath() execEnv {
	return testEnv(
		[]string{"/home/foci/.local/bin", "/usr/bin"},
		[]string{"/home/foci/.local/bin"},
		[]string{"/home/foci/.local/bin/sqlite3", "/usr/bin/git", "/usr/bin/sqlite3"},
	)
}

func TestCommandTokens(t *testing.T) {
	tests := []struct {
		name string
		rule string
		want []string
	}{
		{"absolute path with glob arg", "Bash:/opt/bin/tool.py *", []string{"/opt/bin/tool.py"}},
		{"bare name", "Bash:sqlite3 *", []string{"sqlite3"}},
		{"foci shell function", "Bash:foci_*", []string{"foci_*"}},
		{"read rule names data not code", "Read:/home/rich/vault/*", nil},
		{"edit rule names data not code", "Edit:/tmp/*", nil},
		{"tool name with no pattern", "Bash", nil},
		{"empty pattern", "Bash:", nil},
		{"compound splits into segments", "Bash:cd /x && /opt/b.sh", []string{"cd", "/opt/b.sh"}},
		{"pipeline splits into segments", "Bash:cat /x | /opt/b.sh", []string{"cat", "/opt/b.sh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commandTokens(tt.rule)
			if len(got) != len(tt.want) {
				t.Fatalf("commandTokens(%q) = %v, want %v", tt.rule, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("commandTokens(%q) = %v, want %v", tt.rule, got, tt.want)
				}
			}
		})
	}
}

func TestCheckCommandToken_ExplicitPath(t *testing.T) {
	env := testEnv(nil, []string{"/home/foci/scripts/x.py", "/opt/rodir"}, nil)

	if sub, _, _ := checkCommandToken("/home/foci/scripts/x.py", env); !sub {
		t.Error("a writable file must be substitutable")
	}
	// Strict: read-only file inside a writable directory still fails, because
	// the directory permits unlink-and-replace.
	sub, path, reason := checkCommandToken("/opt/rodir/tool", env)
	if !sub {
		t.Fatal("a file in a writable directory must be substitutable")
	}
	if path != "/opt/rodir/tool" || reason == "" {
		t.Errorf("path=%q reason=%q, want the file path and a non-empty reason", path, reason)
	}
	if sub, _, _ := checkCommandToken("/usr/bin/sqlite3", env); sub {
		t.Error("a read-only file in a read-only directory must pass")
	}
	if sub, _, _ := checkCommandToken("./relative.sh", env); sub {
		t.Error("a relative path has no cwd to resolve against and must yield no verdict")
	}
	if sub, _, _ := checkCommandToken("~/bin/x.sh", testEnv(nil, []string{"/home/foci/bin/x.sh"}, nil)); !sub {
		t.Error("~ must expand against homeDir")
	}
}

func TestCheckBareCommand_WritablePathDirectoryShadows(t *testing.T) {
	env := shimmedPath()

	// The shim itself: resolved winner is writable.
	sub, _, _ := checkCommandToken("sqlite3", env)
	if !sub {
		t.Error("a bare name resolving into a writable directory must be substitutable")
	}

	// THE POINT OF THIS CHANGE: git resolves to a read-only /usr/bin/git, but a
	// writable directory sits earlier on PATH, so the name can be shadowed.
	sub, path, reason := checkCommandToken("git", env)
	if !sub {
		t.Fatal("a bare name shadowable via an earlier writable PATH dir must be substitutable")
	}
	if path != "/home/foci/.local/bin" {
		t.Errorf("path = %q, want the writable PATH directory", path)
	}
	if reason == "" {
		t.Error("want a non-empty reason")
	}
}

func TestCheckBareCommand_ReadOnlyPathPasses(t *testing.T) {
	env := readOnlyPath()
	for _, name := range []string{"git", "sqlite3", "gh"} {
		if sub, _, _ := checkCommandToken(name, env); sub {
			t.Errorf("%q on a fully read-only PATH must pass", name)
		}
	}
}

func TestCheckBareCommand_UnresolvableYieldsNoVerdict(t *testing.T) {
	env := readOnlyPath()
	// A shell builtin and a shell-function glob are not on PATH. The checker
	// must not invent a verdict it cannot support.
	for _, name := range []string{"cd", "foci_*", "not_installed_anywhere"} {
		if sub, _, _ := checkCommandToken(name, env); sub {
			t.Errorf("%q is unresolvable and must yield no verdict", name)
		}
	}
}

func TestCheckBareCommand_StopsAtFirstMatchNotLater(t *testing.T) {
	// A writable directory AFTER the winning binary cannot shadow it, so it
	// must not trigger a drop.
	env := testEnv(
		[]string{"/usr/bin", "/home/foci/.local/bin"},
		[]string{"/home/foci/.local/bin"},
		[]string{"/usr/bin/git", "/home/foci/.local/bin/git"},
	)
	if sub, _, _ := checkCommandToken("git", env); sub {
		t.Error("a writable PATH dir searched AFTER the winner cannot shadow it")
	}
}

func TestFilterWritableAutoApproveRules(t *testing.T) {
	rules := []string{
		"Bash:/usr/bin/sqlite3 -readonly", // read-only path: kept
		"Bash:/home/foci/scripts/x.py *",  // writable file: dropped
		"Bash:sqlite3 *",                  // resolves into writable dir: dropped
		"Bash:git *",                      // shadowable via earlier writable dir: dropped
		"Bash:cd *",                       // builtin, unresolvable: kept
		"Read:/home/foci/data/*",          // data path: kept
	}
	env := shimmedPath()
	env.canWrite = func(p string) bool {
		return p == "/home/foci/.local/bin" || p == "/home/foci/scripts/x.py"
	}

	kept, dropped := filterWritableAutoApproveRules(rules, env)

	wantKept := []string{"Bash:/usr/bin/sqlite3 -readonly", "Bash:cd *", "Read:/home/foci/data/*"}
	if len(kept) != len(wantKept) {
		t.Fatalf("kept = %v, want %v", kept, wantKept)
	}
	for i := range kept {
		if kept[i] != wantKept[i] {
			t.Fatalf("kept = %v, want %v", kept, wantKept)
		}
	}
	if len(dropped) != 3 {
		t.Fatalf("dropped %d entries, want 3: %+v", len(dropped), dropped)
	}
	for _, d := range dropped {
		if d.Rule == "" || d.Reason == "" || d.Path == "" {
			t.Errorf("dropped entry missing a field: %+v", d)
		}
	}
}

func TestFilterWritableAutoApproveRules_NoRules(t *testing.T) {
	kept, dropped := filterWritableAutoApproveRules(nil, readOnlyPath())
	if len(kept) != 0 || len(dropped) != 0 {
		t.Fatalf("kept=%v dropped=%v, want both empty", kept, dropped)
	}
}

func TestDropWritableAutoApproveRules_GlobalAndPerAgent(t *testing.T) {
	cfg := &Config{}
	cfg.Permissions.AutoApprove = []string{"Bash:/bad/g.sh *", "Bash:/good/g.sh *"}
	cfg.Agents = []AgentConfig{
		{ID: "clutch", Permissions: PermissionsConfig{AutoApprove: []string{"Bash:/bad/a.sh *", "Bash:cd *"}}},
		{ID: "scout", Permissions: PermissionsConfig{AutoApprove: []string{"Bash:/good/s.sh *"}}},
	}
	env := testEnv(nil, []string{"/bad/g.sh", "/bad/a.sh"}, nil)

	cfg.dropWritableAutoApproveRules(env)

	if got := cfg.Permissions.AutoApprove; len(got) != 1 || got[0] != "Bash:/good/g.sh *" {
		t.Errorf("global AutoApprove = %v, want [Bash:/good/g.sh *]", got)
	}
	if got := cfg.Agents[0].Permissions.AutoApprove; len(got) != 1 || got[0] != "Bash:cd *" {
		t.Errorf("clutch AutoApprove = %v, want [Bash:cd *]", got)
	}
	if got := cfg.Agents[1].Permissions.AutoApprove; len(got) != 1 || got[0] != "Bash:/good/s.sh *" {
		t.Errorf("scout AutoApprove = %v, want [Bash:/good/s.sh *]", got)
	}
}

// The injected-env tests above never touch the real filesystem, so exercise the
// live predicates directly too.
func TestLivePredicates_RealFilesystem(t *testing.T) {
	dir := t.TempDir()
	writablePath := filepath.Join(dir, "writable")
	if err := os.WriteFile(writablePath, []byte("x"), 0o700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !processCanWrite(writablePath) {
		t.Error("a 0700 file we own must report writable")
	}
	if !processCanWrite(dir) {
		t.Error("a temp dir we own must report writable")
	}
	if !processCanExecute(writablePath) {
		t.Error("a 0700 file must report executable")
	}
	if processCanExecute(dir) {
		t.Error("a directory must not report as an executable file")
	}
	if processCanExecute(filepath.Join(dir, "missing")) {
		t.Error("a missing path must not report executable")
	}

	nonExec := filepath.Join(dir, "plain")
	if err := os.WriteFile(nonExec, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root — access(2) ignores mode bits")
	}
	if processCanExecute(nonExec) {
		t.Error("a 0600 file must not report executable")
	}
	readonlyPath := filepath.Join(dir, "readonly")
	if err := os.WriteFile(readonlyPath, []byte("x"), 0o400); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if processCanWrite(readonlyPath) {
		t.Error("a 0400 file must report not writable")
	}
}

func TestLiveExecEnv_PopulatesFromProcess(t *testing.T) {
	t.Setenv("PATH", "/aaa:/bbb")
	env := liveExecEnv()
	if len(env.pathDirs) != 2 || env.pathDirs[0] != "/aaa" || env.pathDirs[1] != "/bbb" {
		t.Errorf("pathDirs = %v, want [/aaa /bbb]", env.pathDirs)
	}
	if env.canWrite == nil || env.isExecutable == nil {
		t.Error("live predicates must be populated")
	}
}

func TestCheckCommandToken_BuiltinsAndGlobsSurviveAWritablePath(t *testing.T) {
	// On a host with a writable PATH directory, every bare NAME is shadowable.
	// Builtins are not: the shell resolves them before searching PATH, so there
	// is no file to substitute. Glob tokens name no single file at all.
	env := shimmedPath()
	for _, token := range []string{"cd", "echo", "test", "[", "pwd", "printf", "true"} {
		if sub, _, _ := checkCommandToken(token, env); sub {
			t.Errorf("builtin %q must not be reported shadowable", token)
		}
	}
	for _, token := range []string{"foci_*", "gh?", "x[ab]"} {
		if sub, _, _ := checkCommandToken(token, env); sub {
			t.Errorf("glob token %q must not be reported shadowable", token)
		}
	}
	// Control: a NON-builtin bare name on the same env must still be caught,
	// or this test would pass for a checker that gave up on everything.
	if sub, _, _ := checkCommandToken("git", env); !sub {
		t.Error("control failed: a non-builtin bare name must still be shadowable")
	}
}

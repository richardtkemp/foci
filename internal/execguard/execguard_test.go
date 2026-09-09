package execguard

import (
	"os"
	"path/filepath"
	"testing"
)

// testEnv builds an Env from explicit sets, so no test touches the host's real
// filesystem or PATH.
func testEnv(pathDirs []string, writable []string, executables []string) Env {
	w := make(map[string]bool, len(writable))
	for _, p := range writable {
		w[p] = true
	}
	x := make(map[string]bool, len(executables))
	for _, p := range executables {
		x[p] = true
	}
	return Env{
		CanWrite:     func(p string) bool { return w[p] },
		PathDirs:     pathDirs,
		IsExecutable: func(p string) bool { return x[p] },
		HomeDir:      "/home/foci",
	}
}

// readOnlyPath mimics a hardened host: no writable directory anywhere on PATH.
func readOnlyPath() Env {
	return testEnv(
		[]string{"/usr/local/bin", "/usr/bin"},
		nil,
		[]string{"/usr/bin/git", "/usr/bin/sqlite3", "/usr/local/bin/gh"},
	)
}

// shimmedPath mimics Dick's host: a writable ~/.local/bin ahead of /usr/bin,
// holding sqlite3 but not git.
func shimmedPath() Env {
	return testEnv(
		[]string{"/home/foci/.local/bin", "/usr/bin"},
		[]string{"/home/foci/.local/bin"},
		[]string{"/home/foci/.local/bin/sqlite3", "/usr/bin/git", "/usr/bin/sqlite3"},
	)
}

func TestCheckCommandToken_ExplicitPath(t *testing.T) {
	env := testEnv(nil, []string{"/home/foci/scripts/x.py", "/opt/rodir"}, nil)

	if sub, _, _ := Substitutable("/home/foci/scripts/x.py", env); !sub {
		t.Error("a writable file must be substitutable")
	}
	// Strict: read-only file inside a writable directory still fails, because
	// the directory permits unlink-and-replace.
	sub, path, reason := Substitutable("/opt/rodir/tool", env)
	if !sub {
		t.Fatal("a file in a writable directory must be substitutable")
	}
	if path != "/opt/rodir/tool" || reason == "" {
		t.Errorf("path=%q reason=%q, want the file path and a non-empty reason", path, reason)
	}
	if sub, _, _ := Substitutable("/usr/bin/sqlite3", env); sub {
		t.Error("a read-only file in a read-only directory must pass")
	}
	if sub, _, _ := Substitutable("./relative.sh", env); sub {
		t.Error("a relative path has no cwd to resolve against and must yield no verdict")
	}
	if sub, _, _ := Substitutable("~/bin/x.sh", testEnv(nil, []string{"/home/foci/bin/x.sh"}, nil)); !sub {
		t.Error("~ must expand against homeDir")
	}
}

func TestCheckBareCommand_OnlyAnExistingShadowCounts(t *testing.T) {
	// shimmedPath has a WRITABLE /home/foci/.local/bin ahead of /usr/bin. It
	// contains sqlite3 but not git.
	env := shimmedPath()

	// sqlite3: the writable directory really does hold the winning executable.
	sub, path, reason := Substitutable("sqlite3", env)
	if !sub {
		t.Fatal("a bare name whose PATH winner is writable must be substitutable")
	}
	if path != "/home/foci/.local/bin/sqlite3" {
		t.Errorf("path = %q, want the winning executable", path)
	}
	if reason == "" {
		t.Error("want a non-empty reason")
	}

	// git: the same writable directory is on PATH but holds no git, so the
	// winner is the read-only /usr/bin/git. A shadow that COULD be created is
	// not a finding — only one that exists.
	if sub, _, _ := Substitutable("git", env); sub {
		t.Error("a writable PATH dir NOT containing the command must not be a finding")
	}
}

func TestCheckBareCommand_PlantedShadowIsCaught(t *testing.T) {
	// Same host, except the shadow now exists. It wins the PATH search and is
	// writable, so the entry must drop.
	env := testEnv(
		[]string{"/home/foci/.local/bin", "/usr/bin"},
		[]string{"/home/foci/.local/bin", "/home/foci/.local/bin/git"},
		[]string{"/home/foci/.local/bin/git", "/usr/bin/git"},
	)
	sub, path, _ := Substitutable("git", env)
	if !sub {
		t.Fatal("an existing writable shadow earlier on PATH must be caught")
	}
	if path != "/home/foci/.local/bin/git" {
		t.Errorf("path = %q, want the planted shadow", path)
	}
}

func TestCheckCommandToken_BuiltinBeatsAWritableFileOfTheSameName(t *testing.T) {
	// A writable /home/foci/.local/bin/echo EXISTS and would win a plain PATH
	// search — but bash resolves `echo` as a builtin and never searches PATH, so
	// the file is not what runs. Dropping "Bash:echo *" here would be a false
	// positive. This is the only case the builtin exemption is load-bearing for:
	// without it, this test fails.
	env := testEnv(
		[]string{"/home/foci/.local/bin", "/usr/bin"},
		[]string{"/home/foci/.local/bin", "/home/foci/.local/bin/echo", "/home/foci/.local/bin/test"},
		[]string{"/home/foci/.local/bin/echo", "/home/foci/.local/bin/test", "/usr/bin/echo"},
	)
	for _, name := range []string{"echo", "test"} {
		if sub, _, _ := Substitutable(name, env); sub {
			t.Errorf("builtin %q must not be dropped for a same-named PATH file", name)
		}
	}
	// Control: a NON-builtin writable file of the same shape IS caught, proving
	// the env really does report those files as writable winners.
	env2 := testEnv(
		[]string{"/home/foci/.local/bin"},
		[]string{"/home/foci/.local/bin", "/home/foci/.local/bin/ripgrep"},
		[]string{"/home/foci/.local/bin/ripgrep"},
	)
	if sub, _, _ := Substitutable("ripgrep", env2); !sub {
		t.Error("control failed: a writable non-builtin winner must be caught")
	}
}

func TestCheckBareCommand_ReadOnlyPathPasses(t *testing.T) {
	env := readOnlyPath()
	for _, name := range []string{"git", "sqlite3", "gh"} {
		if sub, _, _ := Substitutable(name, env); sub {
			t.Errorf("%q on a fully read-only PATH must pass", name)
		}
	}
}

func TestCheckBareCommand_UnresolvableYieldsNoVerdict(t *testing.T) {
	env := readOnlyPath()
	// A shell builtin and a shell-function glob are not on PATH. The checker
	// must not invent a verdict it cannot support.
	for _, name := range []string{"cd", "foci_*", "not_installed_anywhere"} {
		if sub, _, _ := Substitutable(name, env); sub {
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
	if sub, _, _ := Substitutable("git", env); sub {
		t.Error("a writable PATH dir searched AFTER the winner cannot shadow it")
	}
}

func TestCheckCommandToken_BuiltinsAndGlobsSurviveAWritablePath(t *testing.T) {
	// On a host with a writable PATH directory, every bare NAME is shadowable.
	// Builtins are not: the shell resolves them before searching PATH, so there
	// is no file to substitute. Glob tokens name no single file at all.
	env := shimmedPath()
	for _, token := range []string{"cd", "echo", "test", "[", "pwd", "printf", "true"} {
		if sub, _, _ := Substitutable(token, env); sub {
			t.Errorf("builtin %q must not be reported shadowable", token)
		}
	}
	for _, token := range []string{"foci_*", "gh?", "x[ab]"} {
		if sub, _, _ := Substitutable(token, env); sub {
			t.Errorf("glob token %q must not be reported shadowable", token)
		}
	}
	// Control: a NON-builtin bare name whose PATH winner IS writable must still
	// be caught, or this test would pass for a checker that gave up on
	// everything.
	if sub, _, _ := Substitutable("sqlite3", env); !sub {
		t.Error("control failed: a non-builtin bare name with a writable winner must be caught")
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

func TestLive_PopulatesFromProcess(t *testing.T) {
	t.Setenv("PATH", "/aaa:/bbb")
	env := Live()
	if len(env.PathDirs) != 2 || env.PathDirs[0] != "/aaa" || env.PathDirs[1] != "/bbb" {
		t.Errorf("pathDirs = %v, want [/aaa /bbb]", env.PathDirs)
	}
	if env.CanWrite == nil || env.IsExecutable == nil {
		t.Error("live predicates must be populated")
	}
}

package ccaccount

import (
	"os"
	"path/filepath"
	"testing"

	"foci/internal/fociroot"
	"foci/internal/procx"
)

// isolate gives a test a single-home install with no CC variables anywhere:
// HOME is a temp dir, FOCI_HOME and CLAUDE_CONFIG_DIR are empty and
// CLAUDE_SECURESTORAGE_CONFIG_DIR is unset in this process, and the operator
// overlay is cleared (and restored after).
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(fociroot.EnvVar, "")
	t.Setenv(fociroot.CCConfigDirEnv, "")
	t.Setenv(fociroot.CCSecureStorageEnv, "") // registers the restore
	_ = os.Unsetenv(fociroot.CCSecureStorageEnv)
	prev := procx.OperatorOverlay()
	procx.SetOperatorEnv(nil)
	t.Cleanup(func() { procx.SetOperatorEnv(envMap(prev)) })
	return home
}

func envMap(kv []string) map[string]string {
	m := map[string]string{}
	for _, e := range kv {
		for i := range len(e) {
			if e[i] == '=' {
				m[e[:i]] = e[i+1:]
				break
			}
		}
	}
	return m
}

// TestAccount_Linux (H8a): on Linux the account is the login file in the
// folder, and two spellings of one folder are one account.
func TestAccount_Linux(t *testing.T) {
	isolate(t)
	want := Account{Path: "/x/.credentials.json"}
	for _, dir := range []string{"/x", "/x/", "/x/./", "/y/../x"} {
		loc, err := FromEnv([]string{"CLAUDE_CONFIG_DIR=" + dir}, "linux")
		if err != nil {
			t.Fatal(err)
		}
		if loc.Dir != "/x" || loc.Account != want {
			t.Errorf("CLAUDE_CONFIG_DIR=%q: got %+v, want Dir /x, account %+v", dir, loc, want)
		}
	}
	a, _ := FromEnv([]string{"CLAUDE_CONFIG_DIR=/x"}, "linux")
	b, _ := FromEnv([]string{"CLAUDE_CONFIG_DIR=/z"}, "linux")
	if a.Account == b.Account {
		t.Errorf("two folders gave one account %+v", a.Account)
	}
}

// TestAccount_MacKeychain (H8b): on macOS the Keychain item is chosen by
// CLAUDE_SECURESTORAGE_CONFIG_DIR when set and non-empty, the default item
// when it is set and empty, else by CLAUDE_CONFIG_DIR, else the default item.
// The last value of a repeated key wins, as in execve.
func TestAccount_MacKeychain(t *testing.T) {
	isolate(t)
	cases := []struct {
		name string
		env  []string
		want Account
	}{
		{"secure storage set", []string{"CLAUDE_CONFIG_DIR=/x", "CLAUDE_SECURESTORAGE_CONFIG_DIR=/s/"}, Account{Keychain: true, Path: "/s"}},
		{"secure storage empty", []string{"CLAUDE_CONFIG_DIR=/x", "CLAUDE_SECURESTORAGE_CONFIG_DIR="}, Account{Keychain: true}},
		{"config dir only", []string{"CLAUDE_CONFIG_DIR=/x/./"}, Account{Keychain: true, Path: "/x"}},
		{"neither", nil, Account{Keychain: true}},
		{"later value wins", []string{"CLAUDE_SECURESTORAGE_CONFIG_DIR=", "CLAUDE_SECURESTORAGE_CONFIG_DIR=/s"}, Account{Keychain: true, Path: "/s"}},
	}
	for _, c := range cases {
		loc, err := FromEnv(c.env, "darwin")
		if err != nil {
			t.Fatal(err)
		}
		if loc.Account != c.want {
			t.Errorf("%s: account %+v, want %+v", c.name, loc.Account, c.want)
		}
	}
}

// TestForAgent_MacSeparateRootTodayDefaultItem (H8c) is a CHARACTERISATION
// test that phase 2 of #3104 changes (Q12): on the macOS separate-root install
// the gateway exports CLAUDE_SECURESTORAGE_CONFIG_DIR="" (EnsureCCConfigEnv),
// so an agent whose env sets only CLAUDE_CONFIG_DIR still gets the default
// Keychain item. It pins today's value.
func TestForAgent_MacSeparateRootTodayDefaultItem(t *testing.T) {
	isolate(t)
	t.Setenv(fociroot.CCSecureStorageEnv, "")
	t.Setenv(fociroot.CCConfigDirEnv, "/root/.claude")
	loc, err := FromEnv(Env(map[string]string{"CLAUDE_CONFIG_DIR": "/x"}), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if loc.Dir != "/x" || loc.Account != (Account{Keychain: true}) {
		t.Errorf("got %+v, want Dir /x and today's default Keychain item", loc)
	}
}

// TestForAgent_ShellOverlay (H9, F14): the operator's shell overlay can export
// CLAUDE_CONFIG_DIR, which the daemon's own environment lacks; CC is given the
// overlay's folder, so an agent with no override resolves to it, and an agent
// override still wins.
func TestForAgent_ShellOverlay(t *testing.T) {
	isolate(t)
	procx.SetOperatorEnv(map[string]string{"CLAUDE_CONFIG_DIR": "/o"})
	if loc, err := ForAgent(nil); err != nil || loc.Dir != "/o" {
		t.Errorf("no override: %+v, %v; want the overlay's /o", loc, err)
	}
	if loc, err := ForAgent(map[string]string{"CLAUDE_CONFIG_DIR": "/x"}); err != nil || loc.Dir != "/x" {
		t.Errorf("override /x: %+v, %v; want /x", loc, err)
	}
}

// TestForAgent_NoOverrideIsToday (H10, requirement 8): with no
// CLAUDE_CONFIG_DIR anywhere the folder is exactly the host folder
// fociroot.CCConfigDir gives, ~/.claude on a single-home install and
// <root>/.claude on a separate-root one (FOCI_HOME != HOME).
func TestForAgent_NoOverrideIsToday(t *testing.T) {
	home := isolate(t)
	check := func(name, want string) {
		t.Helper()
		host, err := fociroot.CCConfigDir()
		if err != nil {
			t.Fatal(err)
		}
		loc, err := ForAgent(nil)
		if err != nil {
			t.Fatal(err)
		}
		if loc.Dir != want || loc.Dir != host {
			t.Errorf("%s: Dir %q, host %q, want %q", name, loc.Dir, host, want)
		}
		if loc.ProjectsDir() != filepath.Join(want, "projects") {
			t.Errorf("%s: ProjectsDir %q", name, loc.ProjectsDir())
		}
	}
	check("single home", filepath.Join(home, ".claude"))
	root := filepath.Join(t.TempDir(), "foci")
	t.Setenv(fociroot.EnvVar, root)
	check("separate root", filepath.Join(root, ".claude"))
}

// TestEnv_AgentWinsOverOperator: the agent's backend_config.env comes after
// the operator population, so its value is the one FromEnv reads.
func TestEnv_AgentWinsOverOperator(t *testing.T) {
	isolate(t)
	t.Setenv(fociroot.CCConfigDirEnv, "/daemon")
	if loc, _ := FromEnv(Env(nil), "linux"); loc.Dir != "/daemon" {
		t.Errorf("no override: Dir %q, want the daemon's /daemon", loc.Dir)
	}
	if loc, _ := FromEnv(Env(map[string]string{"CLAUDE_CONFIG_DIR": "/agent"}), "linux"); loc.Dir != "/agent" {
		t.Errorf("override: Dir %q, want /agent", loc.Dir)
	}
}

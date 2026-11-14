package ccaccount

import (
	"os"
	"path/filepath"
	"slices"
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

// TestForAgent_MacSeparateRootOwnLogin (H8c, changed by phase 2 of #3104,
// Q12 "Its own login"): on the macOS separate-root install the gateway exports
// CLAUDE_SECURESTORAGE_CONFIG_DIR="" (EnsureCCConfigEnv). Phase 1 pinned that
// an agent whose env sets only CLAUDE_CONFIG_DIR still got the default
// Keychain item; it now gets the item of its own folder.
func TestForAgent_MacSeparateRootOwnLogin(t *testing.T) {
	isolate(t)
	t.Setenv(fociroot.CCSecureStorageEnv, "")
	t.Setenv(fociroot.CCConfigDirEnv, "/root/.claude")
	loc, err := FromEnv(Env(map[string]string{"CLAUDE_CONFIG_DIR": "/x"}), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if loc.Dir != "/x" || loc.Account != (Account{Keychain: true, Path: "/x"}) {
		t.Errorf("got %+v, want Dir /x and the Keychain item of /x", loc)
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

// separateRootMac simulates the work Mac's gateway environment: an install
// root apart from HOME, so EnsureCCConfigEnv exports CLAUDE_CONFIG_DIR and an
// empty CLAUDE_SECURESTORAGE_CONFIG_DIR.
func separateRootMac(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "foci")
	t.Setenv(fociroot.EnvVar, root)
	if _, err := fociroot.EnsureCCConfigEnv(); err != nil {
		t.Fatal(err)
	}
	if v, ok := os.LookupEnv(fociroot.CCSecureStorageEnv); !ok || v != "" {
		t.Fatalf("EnsureCCConfigEnv did not export the empty secure-storage var (%q, %v)", v, ok)
	}
	return root
}

// TestOwnLogin_Q12 (H1, requirement 1): on a separate-root install, alpha (no
// override) keeps the gateway's empty CLAUDE_SECURESTORAGE_CONFIG_DIR and the
// default Keychain item; beta (only CLAUDE_CONFIG_DIR) loses it and gets the
// item of its folder; gamma (sets the empty value itself) keeps it.
func TestOwnLogin_Q12(t *testing.T) {
	isolate(t)
	root := separateRootMac(t)
	cases := []struct {
		name    string
		agent   map[string]string
		present bool
		want    Account
	}{
		{"alpha", nil, true, Account{Keychain: true}},
		{"beta", map[string]string{"CLAUDE_CONFIG_DIR": "/x"}, false, Account{Keychain: true, Path: "/x"}},
		{"gamma", map[string]string{"CLAUDE_CONFIG_DIR": "/x", "CLAUDE_SECURESTORAGE_CONFIG_DIR": ""}, true, Account{Keychain: true}},
	}
	for _, c := range cases {
		env := Env(c.agent)
		_, present := lookup(env, fociroot.CCSecureStorageEnv)
		if present != c.present {
			t.Errorf("%s: CLAUDE_SECURESTORAGE_CONFIG_DIR present = %v, want %v", c.name, present, c.present)
		}
		loc, err := FromEnv(env, "darwin")
		if err != nil {
			t.Fatal(err)
		}
		if loc.Account != c.want {
			t.Errorf("%s: account %+v, want %+v", c.name, loc.Account, c.want)
		}
	}
	if loc, _ := FromEnv(Env(nil), "darwin"); loc.Dir != filepath.Join(root, ".claude") {
		t.Errorf("alpha folder %q, want the install root's", loc.Dir)
	}
}

// TestLoginEnv (H1, H2): the login pane's variables. alpha's equal
// fociroot.CCEnv(), on a single-home and a separate-root install; beta's name
// its folder and unset the secure-storage variable; gamma's keep its empty
// value.
func TestLoginEnv(t *testing.T) {
	isolate(t)
	if got, want := LoginEnv(nil), fociroot.CCEnv(); !slices.Equal(got, want) {
		t.Errorf("single home, alpha: %q, want fociroot.CCEnv() %q", got, want)
	}
	separateRootMac(t)
	if got, want := LoginEnv(nil), fociroot.CCEnv(); !slices.Equal(got, want) {
		t.Errorf("separate root, alpha: %q, want fociroot.CCEnv() %q", got, want)
	}
	home, _ := os.UserHomeDir()
	beta := LoginEnv(map[string]string{"CLAUDE_CONFIG_DIR": "/x"})
	if want := []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=/x", "CLAUDE_SECURESTORAGE_CONFIG_DIR"}; !slices.Equal(beta, want) {
		t.Errorf("beta: %q, want %q", beta, want)
	}
	gamma := LoginEnv(map[string]string{"CLAUDE_CONFIG_DIR": "/x", "CLAUDE_SECURESTORAGE_CONFIG_DIR": ""})
	if want := []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=/x", "CLAUDE_SECURESTORAGE_CONFIG_DIR="}; !slices.Equal(gamma, want) {
		t.Errorf("gamma: %q, want %q", gamma, want)
	}
}

// TestAccount_KeyAndString: equal accounts share a Key, different ones do
// not; String names the location (Q15's fallback).
func TestAccount_KeyAndString(t *testing.T) {
	accts := []Account{
		{Path: "/x/.credentials.json"},
		{Path: "/y/.credentials.json"},
		{Keychain: true},
		{Keychain: true, Path: "/x"},
	}
	seen := map[string]bool{}
	short := map[string]bool{}
	for _, a := range accts {
		if seen[a.Key()] || short[a.ShortKey()] {
			t.Errorf("%+v: key %q / %q not unique", a, a.Key(), a.ShortKey())
		}
		seen[a.Key()], short[a.ShortKey()] = true, true
		if a.Key() != (Account{Keychain: a.Keychain, Path: a.Path}).Key() {
			t.Errorf("%+v: key not stable", a)
		}
	}
	for a, want := range map[Account]string{
		{Path: "/x/.credentials.json"}: "/x",
		{Keychain: true}:               "the default Keychain sign-in",
		{Keychain: true, Path: "/x"}:   "/x",
	} {
		if a.String() != want {
			t.Errorf("%+v: String %q, want %q", a, a.String(), want)
		}
	}
}

package fociroot

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// isolate gives each test a known HOME and clears the two variables this
// package reads, so the host's environment cannot leak into a verdict.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvVar, "")
	t.Setenv(ccConfigEnv, "")
	t.Setenv(ccSecureStorageEnv, "") // registers the restore
	os.Unsetenv(ccSecureStorageEnv)
	return home
}

// With nothing set (every Linux install), the install root is HOME, CC's
// config folder is ~/.claude, nothing is exported and CCEnv adds nothing.
func TestSingleHomeIsUnchanged(t *testing.T) {
	home := isolate(t)
	if got, err := Dir(); err != nil || got != home {
		t.Fatalf("Dir() = %q, %v; want %q", got, err, home)
	}
	if Separate() {
		t.Fatal("Separate() = true with FOCI_HOME unset")
	}
	if got, _ := CCConfigDir(); got != filepath.Join(home, ".claude") {
		t.Fatalf("CCConfigDir() = %q; want ~/.claude", got)
	}
	if set, err := EnsureCCConfigEnv(); err != nil || set != "" || os.Getenv(ccConfigEnv) != "" {
		t.Fatalf("EnsureCCConfigEnv() set %q (%v), env %q; want nothing", set, err, os.Getenv(ccConfigEnv))
	}
	if _, ok := os.LookupEnv(ccSecureStorageEnv); ok {
		t.Fatalf("%s set on a single-home install", ccSecureStorageEnv)
	}
	if env := CCEnv(); env != nil {
		t.Fatalf("CCEnv() = %v; want nil", env)
	}
}

// FOCI_HOME equal to HOME is the single-home case too.
func TestFociHomeEqualToHomeIsSingleHome(t *testing.T) {
	home := isolate(t)
	t.Setenv(EnvVar, home+"/")
	if Separate() {
		t.Fatal("Separate() = true with FOCI_HOME == HOME")
	}
	if set, _ := EnsureCCConfigEnv(); set != "" {
		t.Fatalf("EnsureCCConfigEnv() set %q; want nothing", set)
	}
}

// The macOS shape: HOME is the user's, FOCI_HOME the install root. Install
// paths come from FOCI_HOME, CC gets its own folder under it, and a tmux-
// started CC is told both HOME and CLAUDE_CONFIG_DIR.
func TestSeparateInstallRoot(t *testing.T) {
	home := isolate(t)
	root := filepath.Join(t.TempDir(), "foci")
	t.Setenv(EnvVar, root)

	if got, _ := Dir(); got != root {
		t.Fatalf("Dir() = %q; want %q", got, root)
	}
	if !Separate() {
		t.Fatal("Separate() = false")
	}
	want := filepath.Join(root, ".claude")
	if got, _ := CCConfigDir(); got != want {
		t.Fatalf("CCConfigDir() = %q; want %q", got, want)
	}
	set, err := EnsureCCConfigEnv()
	if err != nil || set != want || os.Getenv(ccConfigEnv) != want {
		t.Fatalf("EnsureCCConfigEnv() = %q, %v; env %q; want %q", set, err, os.Getenv(ccConfigEnv), want)
	}
	if v, ok := os.LookupEnv(ccSecureStorageEnv); !ok || v != "" {
		t.Fatalf("%s = %q (set %v); want set and empty, so CC keeps the default keychain login", ccSecureStorageEnv, v, ok)
	}
	env := CCEnv()
	if !slices.Equal(env, []string{"HOME=" + home, ccConfigEnv + "=" + want, ccSecureStorageEnv + "="}) {
		t.Fatalf("CCEnv() = %v", env)
	}
}

// A CLAUDE_CONFIG_DIR the operator set wins everywhere: it is the folder CC
// itself will use, so foci must read transcripts from there and not
// overwrite it.
func TestExplicitCCConfigDirWins(t *testing.T) {
	isolate(t)
	t.Setenv(EnvVar, filepath.Join(t.TempDir(), "foci"))
	own := t.TempDir()
	t.Setenv(ccConfigEnv, own)

	if got, _ := CCConfigDir(); got != own {
		t.Fatalf("CCConfigDir() = %q; want %q", got, own)
	}
	if set, _ := EnsureCCConfigEnv(); set != "" || os.Getenv(ccConfigEnv) != own {
		t.Fatalf("EnsureCCConfigEnv() overwrote CLAUDE_CONFIG_DIR: set %q, env %q", set, os.Getenv(ccConfigEnv))
	}
}

// A relative FOCI_HOME is made absolute against the cwd, so a path derived
// from it does not change meaning when a child runs elsewhere.
func TestRelativeFociHomeIsAbsolute(t *testing.T) {
	isolate(t)
	t.Setenv(EnvVar, "rel-root")
	got, err := Dir()
	if err != nil || !filepath.IsAbs(got) || filepath.Base(got) != "rel-root" {
		t.Fatalf("Dir() = %q, %v; want an absolute .../rel-root", got, err)
	}
}

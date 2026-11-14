package config

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv("FOCI_CONFIG", "")
	got, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if want := filepath.Join(home, "config", "foci.toml"); got != want {
		t.Errorf("no FOCI_CONFIG: got %q, want %q", got, want)
	}

	t.Setenv("FOCI_CONFIG", "/etc/foci/alt.toml")
	got, err = DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if got != "/etc/foci/alt.toml" {
		t.Errorf("FOCI_CONFIG set: got %q, want /etc/foci/alt.toml", got)
	}
}

// foci-gw with no -config must load the live config, not ./foci.toml in
// whatever directory it was run from (#2117).
func TestParseFlagsInto_DefaultsToLiveConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FOCI_CONFIG", "")

	cases := []struct {
		args     []string
		wantPath string
	}{
		{nil, filepath.Join(home, "config", "foci.toml")},
		{[]string{"-config", "rel.toml"}, "rel.toml"},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("foci-gw", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		path, err := parseFlagsInto(fs, c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if path != c.wantPath {
			t.Errorf("%v: got %q, want %q", c.args, path, c.wantPath)
		}
	}
}

// foci-gw -check-config was dropped (#2788): every spelling is refused with
// a pointer to foci check config, not a bare "flag provided but not defined"
// and never a silent server start.
func TestParseFlagsInto_CheckConfigRemoved(t *testing.T) {
	t.Setenv("FOCI_CONFIG", "/x/foci.toml")
	for _, args := range [][]string{
		{"-check-config"},
		{"--check-config"},
		{"-check-config=true"},
		{"-config", "/x/foci.toml", "-check-config"},
	} {
		fs := flag.NewFlagSet("foci-gw", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		_, err := parseFlagsInto(fs, args)
		if err == nil || !strings.Contains(err.Error(), "foci check config") {
			t.Errorf("%v: err %v, want one naming foci check config", args, err)
		}
	}
}

// stubExecutable makes DefaultConfigPath see the binary at exe.
func stubExecutable(t *testing.T, exe string) {
	t.Helper()
	orig := executablePath
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executablePath = orig })
}

func writeConfig(t *testing.T, root string) string {
	t.Helper()
	p := filepath.Join(root, "config", "foci.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The macOS case (#3113): the install root is ~/foci (FOCI_HOME in the
// LaunchAgent), but a plain ssh shell sets neither FOCI_HOME nor HOME=~/foci.
// The CLI, installed as <root>/bin/foci, must still find
// <root>/config/foci.toml, as #2577 did for the gateway socket.
func TestDefaultConfigPath_BesideBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the user's real home: no config/
	t.Setenv("FOCI_HOME", "")
	t.Setenv("FOCI_CONFIG", "")
	root := t.TempDir()
	want := writeConfig(t, root)
	stubExecutable(t, filepath.Join(root, "bin", "foci"))

	got, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q (config beside the binary)", got, want)
	}
}

// The binary's root is found through a symlink (e.g. /usr/local/bin/foci ->
// ~/foci/bin/foci).
func TestDefaultConfigPath_BesideBinaryThroughSymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("FOCI_HOME", "")
	t.Setenv("FOCI_CONFIG", "")
	root := t.TempDir()
	want := writeConfig(t, root)
	target := filepath.Join(root, "bin", "foci")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "bin", "foci")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	stubExecutable(t, link)

	got, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	// EvalSymlinks may canonicalise the temp dir (macOS /var -> /private/var).
	wantReal, _ := filepath.EvalSymlinks(want)
	if got != want && got != wantReal {
		t.Errorf("got %q, want %q (config beside the symlink's target)", got, want)
	}
}

// The beside-the-binary lookup is only a fallback: when the install root has
// a config (the NUC: CLI at /usr/local/bin, root /home/foci), it wins.
func TestDefaultConfigPath_InstallRootBeatsBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FOCI_HOME", "")
	t.Setenv("FOCI_CONFIG", "")
	want := writeConfig(t, home)
	root := t.TempDir()
	writeConfig(t, root)
	stubExecutable(t, filepath.Join(root, "bin", "foci"))

	got, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q (install root first)", got, want)
	}
}

// Neither exists: the install root's path comes back, so the error a caller
// prints names the same file as before.
func TestDefaultConfigPath_NeitherExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FOCI_HOME", "")
	t.Setenv("FOCI_CONFIG", "")
	stubExecutable(t, filepath.Join(t.TempDir(), "bin", "foci"))

	got, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if want := filepath.Join(home, "config", "foci.toml"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

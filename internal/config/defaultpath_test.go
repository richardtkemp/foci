package config

import (
	"flag"
	"io"
	"path/filepath"
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

// foci-gw -check-config with no -config must validate the live config, not
// ./foci.toml in whatever directory it was run from (#2117).
func TestParseFlagsInto_DefaultsToLiveConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FOCI_CONFIG", "")

	cases := []struct {
		args      []string
		wantPath  string
		wantCheck bool
	}{
		{[]string{"-check-config"}, filepath.Join(home, "config", "foci.toml"), true},
		{nil, filepath.Join(home, "config", "foci.toml"), false},
		{[]string{"-check-config", "-config", "rel.toml"}, "rel.toml", true},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("foci-gw", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		path, check, err := parseFlagsInto(fs, c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if path != c.wantPath || check != c.wantCheck {
			t.Errorf("%v: got (%q, %v), want (%q, %v)", c.args, path, check, c.wantPath, c.wantCheck)
		}
	}
}

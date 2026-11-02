package config

import (
	"flag"
	"io"
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

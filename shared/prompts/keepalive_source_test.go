package prompts

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestResolveKeepaliveSource covers #1351 requirement 1.
func TestResolveKeepaliveSource(t *testing.T) {
	t.Run("script beats md in the same dir", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "keepalive.md"), "md", 0o600)
		writeFile(t, filepath.Join(dir, "keepalive.sh"), "#!/bin/sh\necho hi\n", 0o700)
		got := ResolveKeepaliveSource("", dir)
		if got.Script != filepath.Join(dir, "keepalive.sh") {
			t.Errorf("source = %+v, want the script", got)
		}
	})
	t.Run("non-executable sh ignored", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "keepalive.md"), "md", 0o600)
		writeFile(t, filepath.Join(dir, "keepalive.sh"), "#!/bin/sh\necho hi\n", 0o600)
		got := ResolveKeepaliveSource("", dir)
		if got.Script != "" || got.Text != "md" || got.Path != filepath.Join(dir, "keepalive.md") {
			t.Errorf("source = %+v, want the md", got)
		}
	})
	t.Run("non-executable sh alone falls to next dir then embedded", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "keepalive.sh"), "#!/bin/sh\necho hi\n", 0o600)
		got := ResolveKeepaliveSource("", dir)
		if got.Script != "" || got.Text != Keepalive() {
			t.Errorf("source = %+v, want embedded", got)
		}
	})
	t.Run("earlier dir md beats later dir script", func(t *testing.T) {
		d1, d2 := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(d1, "keepalive.md"), "md1", 0o600)
		writeFile(t, filepath.Join(d2, "keepalive.sh"), "#!/bin/sh\necho hi\n", 0o700)
		got := ResolveKeepaliveSource("", d1, d2)
		if got.Script != "" || got.Text != "md1" {
			t.Errorf("source = %+v, want dir1's md (search order unchanged)", got)
		}
	})
	t.Run("explicit script path", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "ka")
		writeFile(t, p, "#!/bin/sh\necho hi\n", 0o700)
		if got := ResolveKeepaliveSource(p); got.Script != p {
			t.Errorf("source = %+v, want script %s", got, p)
		}
	})
	t.Run("explicit executable-but-unreadable path is a script", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "ka")
		writeFile(t, p, "#!/bin/sh\necho hi\n", 0o711)
		if got := ResolveKeepaliveSource(p); got.Script != p {
			t.Errorf("source = %+v, want script %s", got, p)
		}
	})
	t.Run("directory named keepalive.sh is not a script", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "keepalive.sh"), 0o700); err != nil {
			t.Fatal(err)
		}
		if got := ResolveKeepaliveSource("", dir); got.Script != "" || got.Text != Keepalive() {
			t.Errorf("source = %+v, want embedded", got)
		}
	})
}

// TestResolveKeepaliveSource_Unchanged (characterisation): none, default,
// a missing file, a plain md path and the embedded fallback resolve as
// ResolvePrompt always did.
func TestResolveKeepaliveSource_Unchanged(t *testing.T) {
	md := filepath.Join(t.TempDir(), "ka.md")
	writeFile(t, md, "  custom  ", 0o600)
	for _, tc := range []struct {
		name, path string
		dirs       []string
		want       Source
	}{
		{"none", "none", nil, Source{Disabled: true}},
		{"default", "default", nil, Source{Text: Keepalive()}},
		{"empty", "", []string{t.TempDir()}, Source{Text: Keepalive()}},
		{"md path", md, nil, Source{Text: "custom", Path: md}},
		{"missing path", "/nonexistent/ka.md", nil, Source{Text: Keepalive()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveKeepaliveSource(tc.path, tc.dirs...)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if want := ResolvePrompt(tc.path, "keepalive.md", Keepalive(), tc.dirs...); got.Text != want {
				t.Errorf("text %q differs from ResolvePrompt %q", got.Text, want)
			}
		})
	}
}

// TestResolvePrompt_NeverRunsScripts (characterisation): every other prompt
// ignores scripts — an executable .sh beside it is not consulted, and an
// executable explicit path is read as text.
func TestResolvePrompt_NeverRunsScripts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "keepalive.sh"), "#!/bin/sh\necho hi\n", 0o700)
	writeFile(t, filepath.Join(dir, "keepalive.md"), "md", 0o600)
	if got := ResolvePrompt("", "keepalive.md", "emb", dir); got != "md" {
		t.Errorf("got %q, want md", got)
	}
	p := filepath.Join(dir, "exec.md")
	writeFile(t, p, "text of an executable", 0o700)
	if got := ResolvePrompt(p, "reflection.md", "emb"); got != "text of an executable" {
		t.Errorf("got %q, want the file's text", got)
	}
}

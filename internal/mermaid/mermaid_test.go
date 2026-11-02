package mermaid

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/mermaid/mermaidtest"
)

func TestMermaidPrune_KeepsMostRecent(t *testing.T) {
	m := New(t.TempDir(), "mmdc", "")
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxCached+3; i++ {
		p := m.cachePath(strings.Repeat("x", i), "default", Transparent)
		if err := os.WriteFile(p, []byte("png"), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := time.Unix(1_700_000_000+int64(i), 0)
		_ = os.Chtimes(p, ts, ts)
	}
	m.prune()
	entries, _ := os.ReadDir(m.dir)
	if len(entries) != MaxCached {
		t.Fatalf("after prune: %d files, want %d", len(entries), MaxCached)
	}
	for i := 0; i < 3; i++ {
		if _, err := os.Stat(m.cachePath(strings.Repeat("x", i), "default", Transparent)); !os.IsNotExist(err) {
			t.Errorf("oldest file %d survived the prune", i)
		}
	}
}

func TestShared_OneInstancePerConfig(t *testing.T) {
	dir := t.TempDir()
	a := Shared(dir, "", "")
	if b := Shared(dir, DefaultCmd, ""); b != a {
		t.Error(`Shared("") and Shared("mmdc") are different instances: the app and send_to_chat would not share a render`)
	}
	if c := Shared(dir, "other", ""); c == a {
		t.Error("a different mermaid_cmd returned the same instance")
	}
}

func TestRender_BackgroundIsPartOfTheCacheKey(t *testing.T) {
	cmd, runs := mermaidtest.FakeCmd(t)
	m := New(t.TempDir(), cmd, "/no/browser")
	src := "graph TD\n A-->B\n"
	clear, err := m.Render(context.Background(), src, "default", Transparent)
	if err != nil {
		t.Fatal(err)
	}
	white, err := m.Render(context.Background(), src, "default", White)
	if err != nil {
		t.Fatal(err)
	}
	if clear == white {
		t.Fatal("transparent and white renders share a cache file")
	}
	if b, _ := os.ReadFile(white); !strings.HasSuffix(string(b), "bg=white") {
		t.Errorf("white render body = %q", b)
	}
	if _, err := m.Render(context.Background(), src, "default", White); err != nil {
		t.Fatal(err)
	}
	if n := mermaidtest.RunCount(t, runs); n != 2 {
		t.Errorf("renders = %d, want 2 (third call is a cache hit)", n)
	}
	if parts, _ := filepath.Glob(filepath.Join(m.dir, "*.part")); len(parts) != 0 {
		t.Errorf("leftover part files: %v", parts)
	}
}

// Available is false for "off" and for a command that does not resolve, and
// true for an executable path — without running it.
func TestAvailable(t *testing.T) {
	t.Parallel()
	exe := filepath.Join(t.TempDir(), "fake-mmdc")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	notExec := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		cmd  string
		want bool
	}{
		{DisabledCmd, false},
		{"/no/such/mmdc", false},
		{"no-such-mermaid-cli-xyz", false},
		{notExec, false},
		{exe, true},
	} {
		if got := New(t.TempDir(), tt.cmd, "").Available(); got != tt.want {
			t.Errorf("cmd %q: Available()=%v, want %v", tt.cmd, got, tt.want)
		}
	}
	var nilR *Renderer
	if nilR.Available() {
		t.Error("nil renderer: Available()=true")
	}
}

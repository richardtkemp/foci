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

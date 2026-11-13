package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"foci/internal/platform"
)

// Tests for #3076: foci's non-isolated file tools may touch only the agent's
// workspace, the temp areas and tools.file_allowed_paths. Every writable test
// dir lies under a temp area, so these tests remove the built-in temp areas
// (withTempRoots()) to get an "outside" path they can create.

// canonPath is the canonical form resolveFileArg hands to the tool (on macOS
// /tmp is a symlink to /private/tmp).
func canonPath(t *testing.T, p string) string {
	t.Helper()
	r, err := canonicalFilePath(p)
	if err != nil {
		t.Fatalf("canonicalise %s: %v", p, err)
	}
	return r
}

// withTempRoots replaces the built-in temp areas (tests only).
func withTempRoots(roots ...string) FileOpt {
	return func(fs *fileScope) { fs.tempRoots = &roots }
}

// liveRoots is a tools.file_allowed_paths stand-in that a test edits between
// calls, as a config edit does.
type liveRoots struct {
	mu    sync.Mutex
	roots []string
}

func (l *liveRoots) set(r ...string) { l.mu.Lock(); l.roots = r; l.mu.Unlock() }
func (l *liveRoots) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.roots...)
}

type scopeFixture struct {
	ws, outside, extra string
	live               *liveRoots
	opts               []FileOpt
}

func newScopeFixture(t *testing.T) scopeFixture {
	t.Helper()
	base := t.TempDir()
	f := scopeFixture{
		ws:      filepath.Join(base, "ws"),
		outside: filepath.Join(base, "outside"),
		extra:   filepath.Join(base, "extra"),
		live:    &liveRoots{},
	}
	for _, d := range []string{f.ws, f.outside, f.extra} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "data.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.opts = []FileOpt{WithWorkspace(f.ws), WithExtraRoots(f.live.get), withTempRoots()}
	return f
}

type stubSummariser struct{}

func (stubSummariser) Summarise(_ context.Context, content []byte, _, _ string) (string, error) {
	return "summary of " + string(content), nil
}

func httpTool(opts ...FileOpt) *Tool {
	return NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 },
		func() int64 { return 0 }, func() int64 { return 0 }, nil, 0o640, opts...)
}

// scopeCase drives one tool with one caller-supplied path.
type scopeCase struct {
	name string
	run  func(t *testing.T, f scopeFixture, path string) error
}

func scopeCases(srvURL string) []scopeCase {
	exec := func(tool *Tool, params map[string]any) error {
		raw, _ := json.Marshal(params)
		_, err := tool.Execute(context.Background(), raw)
		return err
	}
	return []scopeCase{
		{"read", func(t *testing.T, f scopeFixture, p string) error {
			return exec(NewReadTool(nil, f.ws, nil, f.opts...), map[string]any{"path": p})
		}},
		{"write", func(t *testing.T, f scopeFixture, p string) error {
			return exec(NewWriteTool(nil, f.ws, nil, 0o640, f.opts...), map[string]any{"path": p, "content": "x"})
		}},
		{"edit", func(t *testing.T, f scopeFixture, p string) error {
			return exec(NewEditTool(nil, f.ws, nil, 0o640, nil, f.opts...),
				map[string]any{"path": p, "old_string": "hello", "new_string": "bye"})
		}},
		{"summary", func(t *testing.T, f scopeFixture, p string) error {
			return exec(NewSummaryTool(nil, stubSummariser{}, f.ws, f.opts...), map[string]any{"file": p, "prompt": "what"})
		}},
		{"http body_file", func(t *testing.T, f scopeFixture, p string) error {
			return exec(httpTool(f.opts...), map[string]any{"url": srvURL, "method": "POST", "body_file": p})
		}},
		{"http files", func(t *testing.T, f scopeFixture, p string) error {
			return exec(httpTool(f.opts...), map[string]any{"url": srvURL, "method": "POST",
				"files": []map[string]any{{"field_name": "f", "file_path": p}}})
		}},
		{"http save_to", func(t *testing.T, f scopeFixture, p string) error {
			return exec(httpTool(f.opts...), map[string]any{"url": srvURL, "save_to": p})
		}},
		{"send_to_chat file", func(t *testing.T, f scopeFixture, p string) error {
			mock := &mockSender{}
			tool := NewSendToChatTool(func(string) platform.Sender { return mock }, nil, nil, nil, f.opts...)
			return exec(tool, map[string]any{"file": p})
		}},
	}
}

func okServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func isScopeRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), FileAllowedPathsKey)
}

// TestFileScope_OutsideRefusedInsideAllowed: every non-isolated file tool
// refuses a path outside the allowed areas, naming the areas and the config
// key, and accepts the same file shape inside the workspace and inside a
// configured extra path. Fail-arm: make confine return canon unchecked and
// every "outside" subtest reddens.
func TestFileScope_OutsideRefusedInsideAllowed(t *testing.T) {
	t.Parallel()
	url := okServer(t)
	for _, c := range scopeCases(url) {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newScopeFixture(t)

			err := c.run(t, f, filepath.Join(f.outside, "data.txt"))
			if !isScopeRefusal(err) {
				t.Fatalf("outside path: want a refusal naming %s, got %v", FileAllowedPathsKey, err)
			}
			if !strings.Contains(err.Error(), canonPath(t, f.ws)) {
				t.Errorf("refusal should name the workspace %s: %v", f.ws, err)
			}

			if err := c.run(t, f, filepath.Join(f.ws, "data.txt")); isScopeRefusal(err) {
				t.Fatalf("workspace path refused: %v", err)
			}
			if err := c.run(t, f, "data.txt"); isScopeRefusal(err) {
				t.Fatalf("workspace-relative path refused: %v", err)
			}

			if err := c.run(t, f, filepath.Join(f.extra, "data.txt")); !isScopeRefusal(err) {
				t.Fatalf("extra path before it is configured: want refusal, got %v", err)
			}
			f.live.set(f.extra) // a live config edit: no tool rebuild
			if err := c.run(t, f, filepath.Join(f.extra, "data.txt")); isScopeRefusal(err) {
				t.Fatalf("configured extra path refused: %v", err)
			}
			f.live.set() // and removing it applies on the next call
			if err := c.run(t, f, filepath.Join(f.extra, "data.txt")); !isScopeRefusal(err) {
				t.Fatalf("extra path after removal: want refusal, got %v", err)
			}
		})
	}
}

// TestFileScope_SymlinkEscapeRefused: a symlink inside the workspace that
// points outside is refused, whether it names a file, a directory on the
// way, or a missing target (os.WriteFile would follow a dangling link and
// create the file outside).
func TestFileScope_SymlinkEscapeRefused(t *testing.T) {
	t.Parallel()
	url := okServer(t)
	for _, c := range scopeCases(url) {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newScopeFixture(t)
			fileLink := filepath.Join(f.ws, "file-link")
			dirLink := filepath.Join(f.ws, "dir-link")
			danglingLink := filepath.Join(f.ws, "dangling")
			if err := os.Symlink(filepath.Join(f.outside, "data.txt"), fileLink); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.outside, dirLink); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(f.outside, "new.txt"), danglingLink); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{fileLink, filepath.Join(dirLink, "data.txt"), filepath.Join(dirLink, "new.txt")} {
				if err := c.run(t, f, p); !isScopeRefusal(err) {
					t.Errorf("%s: want refusal, got %v", p, err)
				}
			}
			if err := c.run(t, f, danglingLink); err == nil || !strings.Contains(err.Error(), "access denied") {
				t.Errorf("dangling link: want access denied, got %v", err)
			}
			if _, err := os.Stat(filepath.Join(f.outside, "new.txt")); err == nil {
				t.Errorf("a file was created outside the workspace through a symlink")
			}
		})
	}
}

// TestFileScope_BlocklistStillAppliesInsideWorkspace: the secrets blocklist
// still refuses inside an allowed area.
func TestFileScope_BlocklistStillAppliesInsideWorkspace(t *testing.T) {
	t.Parallel()
	store, blocked := blockedSecretsFile(t) // a secrets.toml under a temp dir
	ws := filepath.Dir(blocked)
	tool := NewReadTool(store, ws, nil, WithWorkspace(ws), withTempRoots())
	raw, _ := json.Marshal(map[string]any{"path": blocked})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "restricted") {
		t.Fatalf("want the blocklist refusal inside the workspace, got %v", err)
	}
}

// TestFileScope_DefaultTempAreasAllowed: with no override, /tmp-style temp
// areas are allowed even with no workspace configured.
func TestFileScope_DefaultTempAreasAllowed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // under TMPDIR: the process temp dir
	p := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"path": p})
	if _, err := NewReadTool(nil, "", nil).Execute(context.Background(), raw); err != nil {
		t.Fatalf("temp-dir path refused: %v", err)
	}
	fs := newFileScope(fileScope{}, nil)
	if _, err := fs.resolveFileArg("/tmp/foci-3076-probe/new.txt"); err != nil {
		t.Fatalf("/tmp path refused: %v", err)
	}
}

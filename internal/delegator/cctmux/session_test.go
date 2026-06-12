package cctmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writePIDFile writes a CC ~/.claude/sessions/<pid>.json entry under the
// given home directory and returns its path.
func writePIDFile(t *testing.T, home string, pid int, entry pidEntry) string {
	t.Helper()
	dir := filepath.Join(home, ccSessionsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal pid entry: %v", err)
	}
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	return path
}

// TestProjectSlug proves workspace paths are converted to CC's project
// directory naming by replacing every slash with a dash.
func TestProjectSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/rich/git/foci", "-home-rich-git-foci"},
		{"/", "-"},
		{"", ""},
		{"relative/path", "relative-path"},
	}
	for _, tc := range cases {
		if got := projectSlug(tc.in); got != tc.want {
			t.Errorf("projectSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseContentBlocks proves block parsing: arrays decode into typed
// blocks, while plain-string content, empty content, and malformed arrays
// all return nil rather than erroring.
func TestParseContentBlocks(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int // number of blocks; 0 means nil expected
	}{
		{"empty", "", 0},
		{"plain string content", `"just user text"`, 0},
		{"malformed array", `[{"type":}`, 0},
		{"single text block", `[{"type":"text","text":"hi"}]`, 1},
		{"mixed blocks", `[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Read"}]`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseContentBlocks(json.RawMessage(tc.raw))
			if len(got) != tc.want {
				t.Fatalf("got %d blocks, want %d", len(got), tc.want)
			}
			if tc.want > 0 && got[0].Type != "text" {
				t.Errorf("block 0 type = %q, want text", got[0].Type)
			}
		})
	}
}

// TestDiscoverSessionFileFrom proves the testable core of session discovery:
// a valid PID file yields the session ID plus the JSONL path derived from the
// workdir slug, while missing files, bad JSON, and entries without a
// sessionId each produce a distinct error.
func TestDiscoverSessionFileFrom(t *testing.T) {
	home := t.TempDir()

	valid := filepath.Join(home, "valid.json")
	if err := os.WriteFile(valid, []byte(`{"pid":42,"sessionId":"sid-1","cwd":"/work/dir"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	badJSON := filepath.Join(home, "bad.json")
	if err := os.WriteFile(badJSON, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	noSID := filepath.Join(home, "nosid.json")
	if err := os.WriteFile(noSID, []byte(`{"pid":42}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		pidFile  string
		wantSID  string
		wantPath string
		wantErr  string
	}{
		{
			name:     "success",
			pidFile:  valid,
			wantSID:  "sid-1",
			wantPath: filepath.Join(home, ccProjectsDir, "-work-dir", "sid-1.jsonl"),
		},
		{name: "missing pid file", pidFile: filepath.Join(home, "absent.json"), wantErr: "read PID file"},
		{name: "malformed json", pidFile: badJSON, wantErr: "parse PID file"},
		{name: "missing sessionId", pidFile: noSID, wantErr: "no sessionId"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sid, path, err := discoverSessionFileFrom(tc.pidFile, home, "/work/dir")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("discoverSessionFileFrom: %v", err)
			}
			if sid != tc.wantSID {
				t.Errorf("sessionID = %q, want %q", sid, tc.wantSID)
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
		})
	}
}

// TestDiscoverSessionFile_UsesHomeDir proves the public wrapper resolves the
// PID file under $HOME/.claude/sessions/<pid>.json (verified by redirecting
// HOME to a temp dir) and errors when no entry exists for the PID.
func TestDiscoverSessionFile_UsesHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writePIDFile(t, home, 555, pidEntry{PID: 555, SessionID: "sid-9", CWD: "/proj"})

	sid, path, err := discoverSessionFile(555, "/proj")
	if err != nil {
		t.Fatalf("discoverSessionFile: %v", err)
	}
	if sid != "sid-9" {
		t.Errorf("sessionID = %q, want sid-9", sid)
	}
	want := filepath.Join(home, ccProjectsDir, "-proj", "sid-9.jsonl")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	if _, _, err := discoverSessionFile(556, "/proj"); err == nil {
		t.Error("expected error for PID without a sessions entry")
	}
}

// TestFindChildPID proves /proc scanning: the test binary's parent (the go
// test runner) must have at least one child, and a PID that cannot exist
// yields a "no child process" error.
func TestFindChildPID(t *testing.T) {
	child, err := findChildPID(os.Getppid())
	if err != nil {
		t.Fatalf("findChildPID(ppid): %v", err)
	}
	if child <= 0 {
		t.Errorf("child PID = %d, want > 0", child)
	}

	if _, err := findChildPID(-12345); err == nil || !strings.Contains(err.Error(), "no child process") {
		t.Errorf("err = %v, want no-child error", err)
	}
}

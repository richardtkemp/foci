package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestCloseAllServers_DrainsPool proves the shutdown backstop empties the pool
// (ignoring refcounts) and returns the number closed, and is idempotent. Uses
// bare Servers (cmd == nil) whose Close is a safe no-op — the real subprocess
// SIGTERM/SIGKILL ladder is covered by Server.Close's own tests; this verifies
// the drain itself (#948). Not parallel: it touches the package-level pool.
func TestCloseAllServers_DrainsPool(t *testing.T) {
	// Start from a clean pool regardless of any prior test's leftovers.
	serverPoolMu.Lock()
	for id := range serverPool {
		delete(serverPool, id)
	}
	serverPool["a"] = &Server{}
	serverPool["b"] = &Server{}
	serverPool["c"] = &Server{}
	serverPoolMu.Unlock()

	if n := CloseAllServers(); n != 3 {
		t.Fatalf("CloseAllServers closed %d, want 3", n)
	}

	serverPoolMu.Lock()
	remaining := len(serverPool)
	serverPoolMu.Unlock()
	if remaining != 0 {
		t.Fatalf("pool not drained: %d server(s) remain", remaining)
	}

	// Idempotent: a second call on the empty pool closes nothing.
	if n := CloseAllServers(); n != 0 {
		t.Fatalf("second CloseAllServers closed %d, want 0", n)
	}
}

// --- ReapOrphanedServers tests ---

// fakeProc describes one process to materialise in a fake /proc tree.
type fakeProc struct {
	pid     int
	ppid    int
	uid     string
	cmdline string // empty = no cmdline file (kernel thread)
}

func writeFakeProc(t *testing.T, dir string, procs []fakeProc) {
	t.Helper()
	for _, p := range procs {
		pdir := filepath.Join(dir, fmt.Sprintf("%d", p.pid))
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", pdir, err)
		}
		if p.cmdline != "" {
			cl := []byte{}
			for i, arg := range splitArgs(p.cmdline) {
				if i > 0 {
					cl = append(cl, 0)
				}
				cl = append(cl, []byte(arg)...)
			}
			cl = append(cl, 0)
			if err := os.WriteFile(filepath.Join(pdir, "cmdline"), cl, 0o644); err != nil {
				t.Fatalf("write cmdline: %v", err)
			}
		}
		status := fmt.Sprintf("Name:\topencode\nState:\tS (sleeping)\nPPid:\t%d\nUid:\t%s\t%s\t%s\t%s\nVmRSS:\t100000 kB\n",
			p.ppid, p.uid, p.uid, p.uid, p.uid)
		if err := os.WriteFile(filepath.Join(pdir, "status"), []byte(status), 0o644); err != nil {
			t.Fatalf("write status: %v", err)
		}
	}
}

func splitArgs(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestIsOpencodeProcess(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"serve", []string{"opencode", "serve"}, true},
		{"serve with flags", []string{"opencode", "serve", "--port", "44095", "--hostname", "127.0.0.1"}, true},
		{"serve full path", []string{"/usr/local/bin/opencode", "serve", "--port", "1234"}, true},
		{"run LSP", []string{"/home/foci/.opencode/bin/opencode", "run", "/home/foci/.local/share/opencode/bin/node_modules/bash-language-server/out/cli.js", "start"}, true},
		{"chat", []string{"opencode", "chat"}, true},
		{"wrong binary", []string{"python3", "serve"}, false},
		{"single arg", []string{"opencode"}, false},
		{"empty", []string{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := []byte{}
			for i, arg := range tt.args {
				if i > 0 {
					cl = append(cl, 0)
				}
				cl = append(cl, []byte(arg)...)
			}
			cl = append(cl, 0)
			if got := isOpencodeProcess(cl); got != tt.want {
				t.Errorf("isOpencodeServe(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestReadStatusPPID(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status")
	content := "Name:\topencode\nPPid:\t1\nUid:\t1000\t1000\t1000\t1000\nVmRSS:\t50000 kB\n"
	if err := os.WriteFile(statusPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ppid, owned := readStatusPPID(statusPath, "1000")
	if ppid != 1 {
		t.Errorf("ppid = %d, want 1", ppid)
	}
	if !owned {
		t.Error("owned = false, want true")
	}

	// Different UID → not owned.
	_, owned = readStatusPPID(statusPath, "9999")
	if owned {
		t.Error("owned = true for mismatched UID, want false")
	}
}

func TestFindOrphanedServers(t *testing.T) {
	dir := t.TempDir()
	writeFakeProc(t, dir, []fakeProc{
		{pid: 100, ppid: 1, uid: "1000", cmdline: "opencode serve --port 44095 --hostname 127.0.0.1"},
		{pid: 200, ppid: 1, uid: "1000", cmdline: "/usr/local/bin/opencode serve --port 46753"},
		{pid: 300, ppid: 55820, uid: "1000", cmdline: "opencode serve --port 12345"},
		{pid: 400, ppid: 1, uid: "0", cmdline: "opencode serve --port 9999"},
		{pid: 500, ppid: 1, uid: "1000", cmdline: "python3 -m http.server"},
		{pid: 600, ppid: 1, uid: "1000", cmdline: "opencode chat"},
		{pid: 700, ppid: 1, uid: "1000", cmdline: "opencode serve"},
		{pid: 800, ppid: 1, uid: "1000", cmdline: "/home/foci/.opencode/bin/opencode run /home/foci/.local/share/opencode/bin/node_modules/bash-language-server/out/cli.js start"},
	})

	orphans := findOrphanedServers(dir, 1000)
	if len(orphans) != 5 {
		t.Fatalf("got %d orphans %v, want 5", len(orphans), orphans)
	}

	seen := map[int]bool{}
	for _, pid := range orphans {
		seen[pid] = true
	}
	for _, want := range []int{100, 200, 600, 700, 800} {
		if !seen[want] {
			t.Errorf("expected pid %d in orphans, got %v", want, orphans)
		}
	}
}

func TestFindOrphanedServers_NoOrphans(t *testing.T) {
	dir := t.TempDir()
	writeFakeProc(t, dir, []fakeProc{
		{pid: 100, ppid: 55820, uid: "1000", cmdline: "opencode serve --port 44095"},
		{pid: 200, ppid: 1, uid: "1000", cmdline: "python3 server.py"},
	})
	if orphans := findOrphanedServers(dir, 1000); len(orphans) != 0 {
		t.Errorf("expected 0 orphans, got %d: %v", len(orphans), orphans)
	}
}

func TestFindOrphanedServers_MissingDir(t *testing.T) {
	if orphans := findOrphanedServers("/nonexistent/proc", 1000); len(orphans) != 0 {
		t.Errorf("expected 0 on missing dir, got %d", len(orphans))
	}
}

func TestReapOrphanedServers_NoOrphans(t *testing.T) {
	dir := t.TempDir()
	orig := procDir
	procDir = dir
	defer func() { procDir = orig }()

	if n := ReapOrphanedServers(); n != 0 {
		t.Errorf("on empty dir = %d, want 0", n)
	}
}

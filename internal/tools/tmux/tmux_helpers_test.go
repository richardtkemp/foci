package tmux

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/testtemp"
	"foci/internal/tools"
)

func TestMain(m *testing.M) {
	// Isolate exec/shell tests from a live foci agent's bridge. When `go test`
	// is run from inside a running agent's Bash session, the process inherits
	// FOCI_SOCK (the production exec-bridge socket) and BASH_ENV (which defines
	// the foci_* shell functions). ExecTool subprocesses inherit os.Environ(),
	// so tests that exec `foci_http_request ... https://example.com` would
	// connect to the PRODUCTION bridge with the real secret store — firing real
	// (host-check-blocked, but log-noisy) requests through the live session.
	// Tests that genuinely need a bridge set FOCI_SOCK explicitly themselves.
	for _, k := range []string{"FOCI_SOCK", "BASH_ENV", "FOCI_GW_SOCK", "FOCI_ADDR", "FOCI_SESSION_KEY"} {
		os.Unsetenv(k)
	}

	dir, _ := os.MkdirTemp(testtemp.Dir(), "foci-tmux-test-*")
	tmuxSocketPath = filepath.Join(dir, "tmux.sock")

	// Pre-start the tmux server so parallel tests don't race on startup.
	// "start-server" is a no-op if a server is already running.
	if _, err := exec.LookPath("tmux"); err == nil {
		exec.Command("tmux", "-S", tmuxSocketPath, "start-server").Run()
	}

	code := m.Run()
	exec.Command("tmux", "-S", tmuxSocketPath, "kill-server").Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func tmuxAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// tmuxIsolatedSocket creates a per-test tmux server on its own socket and
// registers cleanup to kill it. Returns the socket path to pass to NewTmuxTool
// (and to any direct tmux commands the test issues).
//
// Unlike the package-shared tmuxSocketPath, this fully isolates the test from
// sibling parallel tests: a kill-server (or production maybeKillTmuxServer) in
// another test can never destroy this test's sessions. Tests that depend on a
// session staying alive across a NewTmuxTool restore MUST use this — sharing the
// package socket lets a concurrent kill-server prune their watches mid-test.
// Skips the test if tmux is unavailable.
func tmuxIsolatedSocket(t *testing.T) string {
	t.Helper()
	tmuxAvailable(t)
	sock := filepath.Join(t.TempDir(), "tmux.sock")
	exec.Command("tmux", "-S", sock, "start-server").Run()
	t.Cleanup(func() {
		exec.Command("tmux", "-S", sock, "kill-server").Run()
	})
	return sock
}

// tmuxSetup pre-cleans named sessions (from prior crashed runs) and registers
// t.Cleanup to kill them when the test finishes. All operations use the
// shared package tmux socket.
//
// Prefer tmuxIsolatedSocket for any test that creates sessions and depends on
// them surviving: the shared socket is vulnerable to cross-test kill-server.
func tmuxSetup(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		exec.Command("tmux", "-S", tmuxSocketPath, "kill-session", "-t", name).Run()
		t.Cleanup(func() {
			exec.Command("tmux", "-S", tmuxSocketPath, "kill-session", "-t", name).Run()
		})
	}
}

// pollForReadMatch repeatedly issues a "read" operation against name via tool
// until the result text satisfies match (or the timeout elapses), then
// returns the last-seen result. Replaces a fixed time.Sleep before a single
// read: under CPU pressure (many parallel t.Parallel() tests sharing few
// cores) a fixed sleep is a genuine race — the shell inside the pane may not
// have executed/echoed its command yet — not a "the machine was busy"
// excuse. Polling for the expected content is the correct wait condition.
//
// extra merges additional read params (e.g. {"raw": true}) into the request;
// pass nil for a plain cleaned read.
func pollForReadMatch(t *testing.T, tool *tools.Tool, name string, match func(text string) bool, timeout time.Duration, extra ...map[string]interface{}) tools.ToolResult {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last tools.ToolResult
	for {
		req := map[string]interface{}{
			"operation": "read",
			"name":      name,
			"lines":     100,
		}
		for _, e := range extra {
			for k, v := range e {
				req[k] = v
			}
		}
		params, _ := json.Marshal(req)
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		last = result
		if match(result.Text) {
			return last
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// testTmuxInstance returns a minimal tmuxInstance using the global test socket.
// Used by tests that call methods like tmuxSessionPIDs or maybeKillTmuxServer
// directly (outside of a full NewTmuxTool).
func testTmuxInstance() *tmuxInstance {
	return &tmuxInstance{socketPath: tmuxSocketPath}
}

// pollUntil repeatedly calls cond (every 20ms) until it returns true or
// timeout elapses, returning whether cond became true in time. General
// sibling to pollForReadMatch for non-read wait conditions — a process
// spawning or dying, a watch/monitor goroutine reaching some state, a /proc
// field changing, etc. Any fixed time.Sleep that's really "wait for X to
// become true" should poll for X instead of guessing a duration: under CPU
// pressure (many parallel t.Parallel() tests sharing few cores) a fixed sleep
// is a genuine race, not a "the machine was busy" excuse.
func pollUntil(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// NOTE on a poll that was tried and reverted here: a `pollUntilSessionUp`
// helper (poll tmuxSessionPIDs until non-empty) previously replaced the
// package's many "start a session, sleep briefly, then send/read/watch it"
// sleeps. It was removed after reproducing NEW flakiness under full-suite
// load (go test -p=4 -parallel=16 ./...: 1/5 failures scoped to this package
// alone, vs. 0/8 on the fixed-sleep baseline) — the poll's own repeated
// `tmux list-panes` subprocess forks (one per site × ~25 call sites, each
// potentially looping) added exactly the kind of contention this audit is
// trying to remove. It also wasn't needed: `start`'s new-session call is
// synchronous (tmux forks the pane and assigns its PID before returning, and
// list-panes reflects that PID with zero measured delay in a 20-iteration
// no-wait test), and send/read/watch all tolerate an unready pane already —
// send-keys is TTY-buffered regardless of reader readiness, and read/watch's
// own capture-pane calls are best-effort (silently skipped on error, not
// required for the assertions these tests make). So these sites needed
// neither a sleep nor a poll; see the removed sleeps' git history.

package tmux

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// Every test that touches a tmux server takes its own via
	// tmuxIsolatedSocket, so no server is shared and autoReapEmptyServer is
	// left at its production value — the reap can only ever collect the
	// server belonging to the test that created it.
	//
	// tmuxSocketPath is still repointed at a temp path: it is the fallback
	// NewTmuxTool uses when socketPath is empty, so leaving it at the
	// production default would let a test that forgets to pass a socket
	// reach the live agent's tmux server. Nothing should create sessions
	// here; the redirect is a backstop, not a shared workspace.
	dir, _ := os.MkdirTemp(testtemp.Dir(), "foci-tmux-test-*")
	tmuxSocketPath = filepath.Join(dir, "tmux.sock")

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
// This is the ONLY way a test in this package should reach a tmux server. It
// fully isolates the test from its siblings: a kill-server (or production
// maybeKillTmuxServer, or a reap of the last session) in another test can never
// destroy this test's sessions, and no test needs to invent a unique session
// name to avoid colliding with one. Skips the test if tmux is unavailable.
func tmuxIsolatedSocket(t *testing.T) string {
	t.Helper()
	tmuxAvailable(t)
	sock := filepath.Join(t.TempDir(), "tmux.sock")

	// "start-server" on its own does NOT leave a server running: tmux's
	// exit-empty option (default on) makes a server with no sessions exit
	// immediately. Measured — start-server returns 0 and the very next
	// list-sessions reports "no server running on <sock>". So a bare
	// start-server is a no-op, and the first real command each test issues is
	// what actually cold-starts the server. Under parallel load that fork
	// races and fails, surfacing as "create session: exit status 1" or
	// "server exited unexpectedly" attributed to whichever test drew the short
	// straw — a setup failure wearing the costume of a product bug.
	//
	// exit-empty off keeps the empty server alive, so the server is warm
	// before the test's first command and there is no cold start to race.
	// Uses the production runTmuxRetryWithSocket for both steps: the server
	// start is subject to exactly the transient failure it exists to absorb,
	// so the fixture and the product should not have separate ideas about how
	// to survive it.
	ctx := context.Background()
	if out, err := runTmuxRetryWithSocket(ctx, sock, 3,
		"start-server", ";", "set", "-g", "exit-empty", "off"); err != nil {
		t.Fatalf("tmux start-server on %s: %v: %s", sock, err, strings.TrimSpace(out))
	}
	t.Cleanup(func() {
		runTmuxWithSocket(ctx, sock, "kill-server") //nolint:errcheck // best-effort teardown
	})

	// Guard the premise: don't hand back a socket until the server actually
	// answers. list-sessions exits 0 on a live empty server (and 1 with
	// "no server running" if it died), so it is the readiness signal.
	if _, err := runTmuxRetryWithSocket(ctx, sock, 20, "list-sessions"); err != nil {
		t.Fatalf("tmux server on %s never became ready: %v", sock, err)
	}
	return sock
}

// tmuxNoServerSocket returns a socket path with NO tmux server behind it, for
// the tests that specifically exercise the "no server running" branch (tmux
// commands fail, and the tool is expected to treat that as "zero sessions").
//
// Kept distinct from tmuxIsolatedSocket because that helper deliberately keeps
// an empty server alive (exit-empty off) to avoid cold-start races — which
// means a command against it succeeds-with-no-rows rather than failing. Both
// shapes are real; a test that cares which one it gets should say so.
func tmuxNoServerSocket(t *testing.T) string {
	t.Helper()
	tmuxAvailable(t)
	return filepath.Join(t.TempDir(), "tmux.sock")
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

// testTmuxInstance returns a minimal tmuxInstance bound to sock. Used by tests
// that call methods like tmuxSessionPIDs or maybeKillTmuxServer directly
// (outside of a full NewTmuxTool). Pass a socket from tmuxIsolatedSocket so the
// instance can never see — or kill — a sibling parallel test's server.
func testTmuxInstance(sock string) *tmuxInstance {
	return &tmuxInstance{socketPath: sock}
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

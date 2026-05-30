// testharness_control.go — env-gated control socket for L2 integration tests.
//
// When the FOCI_TESTHARNESS_CONTROL_SOCK environment variable points at a
// path, foci-gw opens a UNIX-domain stream socket there and listens for
// simple line-based commands from the harness. This lets integration
// tests force per-agent lifecycle transitions (mid-turn backend close,
// per-agent stop) that the production surface area doesn't expose.
//
// Protocol — one command per line; one reply per line:
//
//	close_backend <agentID>\n   -> ok\n   |   error: <msg>\n
//
// The socket is created with 0600 mode and lives at the path the harness
// supplies (already inside the test's TempDir, so file-perm scoping is
// sufficient — no peer-cred middleware needed). A blank env var disables
// the listener entirely; production foci-gw never has this set.
//
// This is a TEST-ONLY surface. Nothing in production calls it; it is
// guarded by env-gate at startup and removes the socket on shutdown.

package main

import (
	"bufio"
	"context"
	"net"
	"os"
	"strings"

	"foci/internal/log"
)

// setupTestharnessControl checks for the FOCI_TESTHARNESS_CONTROL_SOCK env
// var and, if set, opens a UNIX-stream listener at that path. Each accepted
// connection is handled in a goroutine that reads one command per line
// and writes one reply per line. The listener closes when ctx is done.
//
// Failure to bind the socket is logged at WARN level but does NOT abort
// foci-gw startup — the harness gates tests on observable backend state,
// not on the control socket being present, so a missing socket simply
// means lifecycle-control tests can't run against that gateway.
func setupTestharnessControl(ctx context.Context, agents map[string]*agentInstance) {
	sockPath := os.Getenv("FOCI_TESTHARNESS_CONTROL_SOCK")
	if sockPath == "" {
		return
	}

	// Best-effort cleanup of a stale socket file from a previous run.
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Warnf("testharness_control", "listen %s: %v (lifecycle-control tests will be unable to drive this gateway)", sockPath, err)
		return
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		log.Warnf("testharness_control", "chmod %s: %v", sockPath, err)
	}
	log.Infof("testharness_control", "listening on %s", sockPath)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				// Listener closed during shutdown — exit cleanly.
				return
			}
			go handleTestharnessControlConn(conn, agents)
		}
	}()
}

// handleTestharnessControlConn reads commands one line at a time and
// writes one reply line per command. The connection stays open until the
// remote side closes it.
func handleTestharnessControlConn(conn net.Conn, agents map[string]*agentInstance) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		reply := dispatchTestharnessControl(line, agents)
		if !strings.HasSuffix(reply, "\n") {
			reply += "\n"
		}
		if _, err := conn.Write([]byte(reply)); err != nil {
			return
		}
	}
}

// dispatchTestharnessControl parses one command line and dispatches it to
// the matching foci-gw operation. Returns the reply string (without
// trailing newline; the writer appends one).
func dispatchTestharnessControl(line string, agents map[string]*agentInstance) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "error: empty command"
	}
	op := fields[0]
	switch op {
	case "close_backend":
		// close_backend <agentID>: closes the agent's DelegatedManager,
		// killing every live backend the agent has spun up. Equivalent
		// to what shutdown.go does at graceful-stop, but scoped to one
		// agent. Subsequent inbound messages on that agent's bot will
		// fresh-spawn a new backend (via DelegatedManager.Get's
		// lazy-init path), so this is a "kick the running stub" hook
		// for tests that need to assert OnTurnComplete fires exactly
		// once on the in-flight turn before the respawn fires.
		if len(fields) != 2 {
			return "error: close_backend requires <agentID>"
		}
		agentID := fields[1]
		inst, ok := agents[agentID]
		if !ok {
			return "error: unknown agent " + agentID
		}
		if inst.ag == nil || inst.ag.DelegatedManager == nil {
			return "error: agent " + agentID + " has no delegated manager"
		}
		inst.ag.DelegatedManager.Close()
		log.Infof("testharness_control", "close_backend %s — closed all backends", agentID)
		return "ok"
	default:
		return "error: unknown op " + op
	}
}

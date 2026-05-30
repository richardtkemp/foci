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
	"strconv"
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
	case "set_active_work":
		// set_active_work <agentID> <count>: pin the agent's
		// HasActiveWorkFn return value to <count> for subsequent
		// periodic ticks. Use a negative value to clear the override.
		// Drives the background-scheduler gate that defers when async
		// work (tmux watches) is in flight; for delegated agents
		// (which have nil tmuxWatchCount in production) this is the
		// ONLY way to exercise the gate from a test.
		if len(fields) != 3 {
			return "error: set_active_work requires <agentID> <count>"
		}
		agentID := fields[1]
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			return "error: set_active_work count must be int: " + err.Error()
		}
		inst, ok := agents[agentID]
		if !ok {
			return "error: unknown agent " + agentID
		}
		inst.testActiveWorkOverride.Store(int64(count))
		log.Infof("testharness_control", "set_active_work %s = %d", agentID, count)
		return "ok"
	case "set_canfire":
		// set_canfire <agentID> <0|1> <reason...>: pin the agent's
		// CanFireFunc return value to (allowed, reason) for subsequent
		// periodic ticks. <0|1> is the boolean allowed value; the rest
		// of the line is the reason string passed through verbatim.
		// Drives the shared rate-limit / mana gate that all three
		// schedulers (background, reflection, consolidation) consult
		// before dispatching.
		if len(fields) < 3 {
			return "error: set_canfire requires <agentID> <0|1> [reason]"
		}
		agentID := fields[1]
		allowedStr := fields[2]
		var allowed bool
		switch allowedStr {
		case "0", "false":
			allowed = false
		case "1", "true":
			allowed = true
		default:
			return "error: set_canfire allowed must be 0|1|true|false, got " + allowedStr
		}
		reason := ""
		if len(fields) > 3 {
			// Reason is everything after fields[2]; rejoin to preserve
			// internal whitespace dropped by Fields().
			idx := strings.Index(line, fields[2])
			if idx >= 0 {
				rest := strings.TrimSpace(line[idx+len(fields[2]):])
				reason = rest
			}
		}
		inst, ok := agents[agentID]
		if !ok {
			return "error: unknown agent " + agentID
		}
		inst.testCanFireOverride.Store(&testCanFireState{allowed: allowed, reason: reason})
		log.Infof("testharness_control", "set_canfire %s = (%v, %q)", agentID, allowed, reason)
		return "ok"
	case "stop_agent":
		// stop_agent <agentID>: flag the agent as stopped so the
		// agentResolverFn returns nil for cross-agent dispatch lookups.
		// Exercises the session_notify drop-and-log path without
		// actually tearing down the agent's bot, session_router
		// registration, or in-flight backends — the agent record stays
		// in the map (so its own bot keeps serving messages), but
		// cross-agent target resolution treats it as unreachable.
		if len(fields) != 2 {
			return "error: stop_agent requires <agentID>"
		}
		agentID := fields[1]
		inst, ok := agents[agentID]
		if !ok {
			return "error: unknown agent " + agentID
		}
		inst.stopped.Store(true)
		log.Infof("testharness_control", "stop_agent %s — flagged as stopped", agentID)
		return "ok"
	default:
		return "error: unknown op " + op
	}
}

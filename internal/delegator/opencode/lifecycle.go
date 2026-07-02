// lifecycle.go — Server subprocess lifecycle. Mirrors ccstream's
// bounded-shutdown contract (graceful → SIGTERM → SIGKILL → bounded final
// wait → abandon) so a hung subprocess cannot wedge the pool.
//
// Scope: subprocess launch + health probe + kill-ladder Close. The SSE
// subscriber goroutine is launched from Start (runSubscriber in
// subscriber.go); OnSubscriberStopped is the entry point the subscriber
// calls when it exits.

package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"foci/internal/log"
	"foci/internal/procx"
)

// Close-ladder default waits. Copied into per-Server fields (Server.close*Wait)
// by newServer so a test can shorten them on its own Server without mutating a
// shared package global — the data race behind #975. Production keeps the ~9s
// worst-case in the bounded-shutdown contract; matches ccstream's budget.
const (
	defaultCloseGracefulWait = 5 * time.Second // wait for clean exit before SIGTERM
	defaultCloseSigtermWait  = 2 * time.Second // wait after SIGTERM before SIGKILL
	defaultCloseSigkillWait  = 2 * time.Second // wait after SIGKILL before abandoning the waiter goroutine
)

var (

	// healthProbeInterval is the polling cadence for GET /global/health
	// during Start. The probe times out via the caller's context.
	healthProbeInterval = 200 * time.Millisecond
)

// Start launches the opencode-server subprocess and blocks until the
// /global/health probe succeeds or ctx expires. The SSE subscriber
// goroutine is started from here (runSubscriber in subscriber.go);
// OnSubscriberStopped is the entry point the subscriber invokes on exit.
//
// Start is idempotent — calling it on an already-running Server is a
// no-op. acquireServer serialises construction per-agent so the only
// legitimate caller is the pool.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// 1. Pick a free TCP port (bind 0, read assigned, close — pass to --port).
	port, err := pickFreePort(s.hostname)
	if err != nil {
		return fmt.Errorf("opencode: pick free port: %w", err)
	}
	if s.port != 0 && s.port != port {
		// Caller asked for a specific port but it's taken. We
		// always pass 0 (free port); this branch is defensive.
		return fmt.Errorf("opencode: requested port %d unavailable", s.port)
	}
	s.port = port
	s.baseURL = fmt.Sprintf("http://%s:%d", s.hostname, port)

	// 2. Build subprocess: `opencode serve --port <port> --hostname <h>`.
	// Long-lived (survives across many Backend sessions), so its context
	// is detached from the caller's — closing happens via Close's kill
	// ladder, not ctx cancellation.
	binary := s.binaryPath
	if binary == "" {
		binary = "opencode"
	}
	args := []string{"serve", "--port", fmt.Sprintf("%d", port), "--hostname", s.hostname}
	if s.logLevel != "" {
		args = append(args, "--log-level", s.logLevel)
	}
	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	cmd := procx.Spawn(cmdCtx, binary, args...)
	cmd.Dir = s.workDir
	cmd.Env = s.buildCmdEnv()

	// Stdin/stdout aren't used (HTTP transport); just inherit /dev/null.
	// Stderr we capture for diagnostics + secondary auth-failure detection.
	cmd.Stdin = nil
	cmd.Stdout = nil
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cmdCancel()
		return fmt.Errorf("opencode: stderr pipe: %w", err)
	}

	component := s.logComponent()
	log.Infof(component, "launching: %s %s (workdir=%s, baseURL=%s)", binary, strings.Join(args, " "), s.workDir, s.baseURL)

	if err := cmd.Start(); err != nil {
		cmdCancel()
		return fmt.Errorf("opencode: start subprocess: %w", err)
	}

	s.cmd = cmd
	s.cancel = cmdCancel
	s.done = make(chan struct{})
	s.waitCh = make(chan error, 1)

	// Stderr capture goroutine. Mirrors ccstream's captureStderr — for
	// diagnostics; primary 401 detection lives in authfail.go's transport
	// wrapper.
	go s.captureStderr(stderrPipe)

	// SSE subscriber goroutine. Started BEFORE the health probe finishes
	// so we don't miss the server.connected event or any early per-session
	// events (sessions aren't created until POST /session anyway, but having the
	// subscriber ready avoids a race). The subscriber's own GET /event will
	// get "connection refused" until the subprocess binds its port (~1s);
	// runSubscriber retries the initial connect on a tick through that
	// startup window, bounded by ctx (Close cancels subscriberCancel when
	// the health probe fails) and s.done (subprocess death).
	subscriberCtx, subscriberCancel := context.WithCancel(context.Background())
	s.subscriberCancel = subscriberCancel
	go s.runSubscriber(subscriberCtx)

	// Process waiter goroutine — reaps the subprocess and is the AUTHORITATIVE
	// source of "process is dead" (cmd.Wait cannot lie). The SSE subscriber
	// (SSE subscriber) and stderr capture may exit silently; this goroutine still
	// fires finalizeExit regardless.
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.exitErr = err
		s.mu.Unlock()
		if err != nil {
			log.Warnf(component, "subprocess exited: %s", describeExitError(err))
		} else {
			log.Infof(component, "subprocess exited cleanly (status 0)")
		}
		s.finalizeExit(err)
		select {
		case s.waitCh <- err:
		default:
		}
		close(s.done)
	}()

	// Health probe — poll GET /global/health until it returns 200 or ctx
	// expires. On expiry we tear down what we just launched.
	if err := s.healthProbe(ctx); err != nil {
		_ = s.Close()
		return fmt.Errorf("opencode: health probe: %w", err)
	}

	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	log.Infof(component, "ready: %s", s.baseURL)
	return nil
}

// Close runs the shutdown kill-ladder exactly once (closeOnce-gated).
// Returns within ~9s worst-case (closeGracefulWait + closeSigtermWait +
// closeSigkillWait) regardless of subprocess behaviour — the liveness
// backstop is the final abandon-waiter case in the ladder.
//
// Safe to call on a never-Started Server (no-op) and idempotent.
func (s *Server) Close() error {
	s.closeOnce.Do(s.closeInner)
	return nil
}

func (s *Server) closeInner() {
	s.mu.Lock()
	started := s.cmd != nil
	s.running = false
	s.closing = true
	s.mu.Unlock()

	if !started {
		return
	}
	component := s.logComponent()
	pid := 0
	if s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	log.Infof(component, "closing opencode server (pid=%d)", pid)

	// Cancel the SSE subscriber ctx (nil only if Start never reached the
	// subscriber launch). Done early so it stops touching the subprocess —
	// and so runSubscriber's initial-connect retry loop exits promptly
	// rather than spinning against a server that will never come up.
	if s.subscriberCancel != nil {
		s.subscriberCancel()
	}

	// Graceful shutdown ladder, mirroring ccstream's contract:
	//   1. POST /instance/dispose (opencode's documented shutdown path)
	//   2. wait closeGracefulWait for clean exit
	//   3. SIGTERM
	//   4. wait closeSigtermWait
	//   5. SIGKILL
	//   6. wait closeSigkillWait
	//   7. abandon the waiter goroutine (liveness backstop)
	//
	// Each rung has a bounded timeout so worst-case Close returns in
	// ~9s regardless of subprocess behaviour — same invariant as
	// ccstream's Close.
	if s.baseURL != "" {
		// Best-effort; if the POST fails (network already gone, server
		// unresponsive), we fall through to SIGTERM. Short client timeout
		// so the POST itself doesn't eat into the graceful-wait budget.
		client := &http.Client{Timeout: 1 * time.Second}
		resp, err := client.Post(s.baseURL+"/instance/dispose", "application/json", nil)
		if err == nil {
			_ = resp.Body.Close()
		}
	}

	exitSeen := waitForExit(s.waitCh, s.closeGracefulWait)
	if !exitSeen {
		log.Warnf(component, "subprocess (pid=%d) did not exit after %s, sending SIGTERM", pid, s.closeGracefulWait)
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
		}
		exitSeen = waitForExit(s.waitCh, s.closeSigtermWait)
	}
	if !exitSeen {
		log.Warnf(component, "subprocess (pid=%d) did not exit after SIGTERM, sending SIGKILL", pid)
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		exitSeen = waitForExit(s.waitCh, s.closeSigkillWait)
	}
	if !exitSeen {
		log.Warnf(component, "waiter goroutine did not report after SIGKILL within %s — abandoning wait (possible zombie)", s.closeSigkillWait)
	} else {
		log.Infof(component, "opencode server (pid=%d) exited", pid)
	}

	// Cancel the cmd ctx (releases any outstanding cmd resources).
	if s.cancel != nil {
		s.cancel()
	}

	// Wait for the waiter goroutine to finish (finalizeExit + chan close).
	// Bounded: a stalled waiter shouldn't wedge Close.
	select {
	case <-s.done:
	case <-time.After(s.closeSigtermWait):
		log.Warnf(component, "waiter goroutine did not close done channel within %s — abandoning", s.closeSigtermWait)
	}
}

// waitForExit returns true if waitCh reported within timeout. Helper
// factored out of closeInner so each rung of the kill ladder reads the
// same. Reads from a buffered(1) channel, so the second+ read after the
// first already received is a no-op (returns true immediately) — that's
// the desired behaviour: once we know the subprocess has exited, every
// subsequent rung is a no-op.
func waitForExit(waitCh <-chan error, timeout time.Duration) bool {
	if waitCh == nil {
		return false
	}
	select {
	case <-waitCh:
		return true
	case <-time.After(timeout):
		return false
	}
}

// healthProbe polls GET /global/health until it returns 200 with
// healthy=true, ctx expires, or the subprocess dies (detected via done).
// The health endpoint returns `{"healthy": true, "version": "..."}`.
func (s *Server) healthProbe(ctx context.Context) error {
	client := &http.Client{Timeout: 2 * time.Second}
	url := s.baseURL + "/global/health"
	ticker := time.NewTicker(healthProbeInterval)
	defer ticker.Stop()

	for {
		// Subprocess died before/while probing — fail fast so caller can
		// surface the subprocess exit error rather than wait for ctx.
		select {
		case <-s.done:
			s.mu.Lock()
			err := s.exitErr
			s.mu.Unlock()
			if err != nil {
				return fmt.Errorf("subprocess exited during health probe: %s", describeExitError(err))
			}
			return errors.New("subprocess exited during health probe")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err != nil {
			continue // network not up yet; retry on next tick
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var health struct {
			Healthy bool   `json:"healthy"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &health); err != nil {
			continue
		}
		if health.Healthy {
			return nil
		}
	}
}

// OnSubscriberStopped is called by the SSE subscriber goroutine when it
// exits for any reason — clean EOF, transport error, or ctx cancel. Logs
// and calls finalizeExit so any in-flight cleanup runs.
func (s *Server) OnSubscriberStopped(err error) {
	component := s.logComponent()
	s.mu.Lock()
	expected := s.closing
	s.mu.Unlock()
	if expected {
		log.Infof(component, "SSE subscriber stopped (server closing)")
	} else {
		log.Warnf(component, "SSE subscriber stopped: %v", err)
	}
	s.finalizeExit(err)
}

// finalizeExit is the one-shot cleanup when the opencode-server
// subprocess has died (or the subscriber has dropped, treated as death
// for safety). sync.Once-gated: whichever of {waiter goroutine, SSE
// subscriber} notices first wins; the other is a no-op.
//
// For each registered Backend, synthesises a session.error event and
// pushes it onto the Backend's events channel — so its handlers (handlers.go)
// can complete any in-flight turn with an error rather than hanging
// forever waiting for events that will never arrive. Without this, the
// SSE stream going away would silently wedge every active session.
func (s *Server) finalizeExit(reason error) {
	s.finalizeOnce.Do(func() {
		component := s.logComponent()
		start := time.Now()
		log.Debugf(component, "finalizeExit: enter reason=%v", reason)
		defer func() {
			log.Debugf(component, "finalizeExit: exit elapsed=%s", time.Since(start))
		}()

		s.mu.Lock()
		s.running = false
		s.mu.Unlock()

		// Proactively evict the dead Server from the pool so the next
		// acquireServer spawns a fresh one instead of handing back this
		// corpse (whose port no longer answers). Guard that the pooled entry
		// is still us — a respawn may already have replaced it, and we must
		// not delete a healthy successor.
		serverPoolMu.Lock()
		if serverPool[s.agentID] == s {
			delete(serverPool, s.agentID)
		}
		serverPoolMu.Unlock()

		// Touch lastActivity so LastActivity() reports a fresh
		// "dead" timestamp rather than the last SSE frame time.
		s.lastActivity.Store(time.Now().UnixNano())

		// Synthesise a session.error event for each registered Backend.
		// The payload matches the wire shape opencode would emit on a
		// real auth/transient failure — OnSessionError (handlers.go)
		// treats synthesised and real events identically. The handler
		// fires OnTurnComplete with the error text and clears turn state.
		//
		// If reason was the expected Close path (s.closing), the message
		// reflects that so the user sees "session closed" rather than
		// "subprocess died unexpectedly".
		s.mu.Lock()
		expected := s.closing
		s.mu.Unlock()

		s.sessionsMu.RLock()
		backends := make([]*Backend, 0, len(s.sessions))
		for _, be := range s.sessions {
			backends = append(backends, be)
		}
		s.sessionsMu.RUnlock()

		if len(backends) > 0 {
			msg := "subprocess exited unexpectedly"
			if expected {
				msg = "session closed"
			} else if reason != nil {
				msg = reason.Error()
			}
			payload, _ := json.Marshal(struct {
				Error *MessageError `json:"error"`
			}{
				Error: &MessageError{
					Name: ErrUnknown,
					Data: mustMarshalJSON(map[string]string{"message": msg}),
				},
			})
			syntheticEvent := rawEvent{
				Type:       EventSessionError,
				Properties: payload,
			}
			for _, be := range backends {
				// Non-blocking push — if the dispatcher is wedged the
				// channel may be full; the WARN route already fires for
				// drops, so this is consistent with steady-state drops.
				select {
				case be.events <- syntheticEvent:
				default:
					log.Warnf(component, "finalizeExit: %s events channel full; cannot deliver session.error", be.sessionID)
				}
			}
			log.Warnf(component, "finalizeExit: dispatched session.error to %d session(s)", len(backends))
		}
	})
}

// mustMarshalJSON encodes v as JSON, panicking on failure. Used only
// for fixed-shape synthesised payloads where marshal failure would be a
// programmer error (not a runtime condition).
func mustMarshalJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("opencode: mustMarshalJSON: %v", err))
	}
	return b
}

// captureStderr reads the subprocess stderr line by line and logs it.
// Mirrors ccstream's captureStderr. Lines containing "error"/"fatal"/
// "panic" log at WARN; the rest at DEBUG. Primary 401 detection lives
// in authfail.go's transport wrapper; this is diagnostics-only.
func (s *Server) captureStderr(r io.Reader) {
	component := s.logComponent()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			line := strings.TrimSpace(string(buf[:n]))
			if line != "" {
				lower := strings.ToLower(line)
				if strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic") {
					log.Warnf(component, "stderr: %s", line)
				} else {
					log.Debugf(component, "stderr: %s", line)
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Warnf(component, "stderr capture stopped: %v", err)
			}
			return
		}
	}
}

// logComponent returns the log component string for this Server.
func (s *Server) logComponent() string {
	if s.agentID != "" {
		return "opencode:" + s.agentID
	}
	return "opencode"
}

// pickFreePort opens a listener on hostname:0, reads the assigned port,
// and closes the listener so the caller can pass the port to the
// subprocess. Race-free in practice (kernel allocates a free port); the
// tiny window between close and bind-by-subprocess is acceptable because
// opencode-server is the only consumer and starts quickly.
func pickFreePort(hostname string) (int, error) {
	addr := hostname + ":0"
	if hostname == "" {
		addr = "127.0.0.1:0"
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// buildCmdEnv assembles the environment for the opencode subprocess.
// Starts with the parent process's environment, adds OPENCODE_SERVER_
// PASSWORD (if set), then applies extraEnv (BASH_ENV, FOCI_SOCK from
// the exec bridge). Extracted from Start so tests can verify the env
// composition without spawning a subprocess.
func (s *Server) buildCmdEnv() []string {
	env := os.Environ()
	if s.serverPassword != "" {
		env = append(env, "OPENCODE_SERVER_PASSWORD="+s.serverPassword)
	}
	for k, v := range s.extraEnv {
		env = append(env, k+"="+v)
	}
	return env
}

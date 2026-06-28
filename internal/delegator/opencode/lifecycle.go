// lifecycle.go — Server subprocess lifecycle. Mirrors ccstream's
// bounded-shutdown contract (graceful → SIGTERM → SIGKILL → bounded final
// wait → abandon) so a hung subprocess cannot wedge the pool.
//
// Step 3 scope: subprocess launch + health probe + kill-ladder Close.
// The SSE subscriber goroutine (Step 4) is stubbed to a no-op ctx-cancel
// hook here; OnSubscriberStopped is the entry point Step 4 will call.

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

// Close timeouts. Vars (not consts) so tests can shrink them; production
// keeps the ~9s worst-case documented in the bounded-shutdown contract.
// Matches ccstream's budget so behaviour is consistent across backends.
var (
	closeGracefulWait = 5 * time.Second // wait for clean exit before SIGTERM
	closeSigtermWait  = 2 * time.Second // wait after SIGTERM before SIGKILL
	closeSigkillWait  = 2 * time.Second // wait after SIGKILL before abandoning the waiter goroutine

	// healthProbeInterval is the polling cadence for GET /global/health
	// during Start. The probe times out via the caller's context.
	healthProbeInterval = 200 * time.Millisecond
)

// Start launches the opencode-server subprocess and blocks until the
// /global/health probe succeeds or ctx expires. The SSE subscriber
// goroutine is started in Step 4; here we leave subscriberCancel nil
// and OnSubscriberStopped is the entry point Step 4 invokes.
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
		// Caller asked for a specific port but it's taken. For Step 3 we
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
	cmdCtx, cmdCancel := context.WithCancel(context.Background())
	cmd := procx.Spawn(cmdCtx, binary, args...)
	cmd.Dir = s.workDir
	cmd.Env = os.Environ()
	if s.serverPassword != "" {
		cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD="+s.serverPassword)
	}

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

	// Stderr capture goroutine. Mirrors ccstream's captureStderr — secondary
	// auth-failure detection happens in Step 11; for Step 3 we just log.
	go s.captureStderr(stderrPipe)

	// Process waiter goroutine — reaps the subprocess and is the AUTHORITATIVE
	// source of "process is dead" (cmd.Wait cannot lie). The SSE subscriber
	// (Step 4) and stderr capture may exit silently; this goroutine still
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

	// Cancel the SSE subscriber ctx (Step 4 stub: subscriberCancel is nil
	// for Step 3 — no subscriber started yet). Done early so it stops
	// touching the subprocess.
	if s.subscriberCancel != nil {
		s.subscriberCancel()
	}

	// Graceful: SIGTERM by default for opencode (no "interrupt" stdin
	// channel like ccstream has). Then SIGKILL if it doesn't exit.
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-s.waitCh:
		// Clean exit.
	case <-time.After(closeGracefulWait):
		log.Warnf(component, "subprocess did not exit after %s, sending SIGKILL", closeGracefulWait)
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		select {
		case <-s.waitCh:
		case <-time.After(closeSigkillWait):
			log.Warnf(component, "waiter goroutine did not report after SIGKILL within %s — abandoning wait (possible zombie)", closeSigkillWait)
		}
	}

	// Cancel the cmd ctx (releases any outstanding cmd resources).
	if s.cancel != nil {
		s.cancel()
	}

	// Wait for the waiter goroutine to finish (finalizeExit + chan close).
	// Bounded: a stalled waiter shouldn't wedge Close.
	select {
	case <-s.done:
	case <-time.After(closeSigtermWait):
		log.Warnf(component, "waiter goroutine did not close done channel within %s — abandoning", closeSigtermWait)
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

// OnSubscriberStopped is called by the SSE subscriber goroutine (Step 4)
// when it exits for any reason — clean EOF, transport error, or ctx
// cancel. Step 3 stub: logs and calls finalizeExit so any in-flight
// cleanup runs. Step 4 replaces this with real subscriber-driven dispatch.
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
// Step 3 scope: mark running=false, surface to each registered Backend
// via a synthesised session.error event (Step 4 will route real events).
// Step 7 will hook OnSessionError on each Backend to fire its
// OnTurnComplete with the error.
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

		// Touch lastActivity so Step 12's LastActivity() reports a fresh
		// "dead" timestamp rather than the last SSE frame time.
		s.lastActivity.Store(time.Now().UnixNano())

		// Step 4 will replace this with real event dispatch. For Step 3
		// we just log the count so a race between waiter and subscriber
		// is observable in tests.
		s.sessionsMu.RLock()
		n := len(s.sessions)
		s.sessionsMu.RUnlock()
		if n > 0 {
			log.Warnf(component, "finalizeExit: %d session(s) registered; Step 4 will dispatch session.error to each", n)
		}
	})
}

// captureStderr reads the subprocess stderr line by line and logs it.
// Mirrors ccstream's captureStderr. Lines containing "error"/"fatal"/
// "panic" log at WARN; the rest at DEBUG. Secondary 401 detection
// happens in Step 11 (fireAuthFailure); for Step 3 we just log.
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

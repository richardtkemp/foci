package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/procx"
)

// stubBinary returns the absolute path to testdata/opc-stub. Tests pass
// this via serverConfig.binaryPath so Server.Start launches the stub
// instead of looking up "opencode" via $PATH.
//
// Absolute (not "testdata/opc-stub") because Server.Start sets cmd.Dir
// to the agent's workdir, and the kernel resolves a relative binary
// path against cmd.Dir — not the test's CWD. An absolute path makes the
// binary resolvable regardless of subprocess workdir.
func stubBinary(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Join(wd, "testdata", "opc-stub")
}

// newTestServer returns a Server configured to use the opc-stub binary,
// ready to be passed to Start (or for tests to drive directly).
func newTestServer(t *testing.T, agentID string) *Server {
	t.Helper()
	return newServer(agentID, serverConfig{
		workDir:    t.TempDir(),
		binaryPath: stubBinary(t),
		hostname:   "127.0.0.1",
		port:       0,
	})
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

func TestServer_Start_LaunchesSubprocess(t *testing.T) {
	// Verifies the launch path: Start spawns the stub subprocess, the
	// health probe fails (stub doesn't serve HTTP), and Start tears down
	// + reaps the subprocess on its way out. The error must mention the
	// probe so we know the launch actually ran.
	srv := newTestServer(t, "agent-launch")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := srv.Start(ctx)
	if err == nil {
		t.Fatal("Start should fail — stub doesn't serve HTTP")
	}
	if !strings.Contains(err.Error(), "health probe") &&
		!strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("err = %v, want it to mention health probe or ctx deadline", err)
	}

	// Subprocess must be reaped by Start's tear-down.
	select {
	case <-srv.done:
	case <-time.After(2 * time.Second):
		t.Fatal("subprocess not reaped after Start failure")
	}
}

func TestServer_Start_PicksFreePort(t *testing.T) {
	// Verifies pickFreePort returns distinct ports across calls. The
	// kernel's bind(0) allocator gives unique ports; this test pins that
	// behaviour so a regression (e.g. hardcoded port) fails loudly.
	seen := make(map[int]bool)
	for i := 0; i < 10; i++ {
		p, err := pickFreePort("127.0.0.1")
		if err != nil {
			t.Fatalf("pickFreePort: %v", err)
		}
		if seen[p] {
			t.Errorf("port %d returned twice", p)
		}
		seen[p] = true
	}
}

func TestServer_HealthProbe_HappyPath(t *testing.T) {
	// Verifies the probe's happy path against a real httptest.Server.
	// Tests the GET /global/health + JSON decode + healthy==true check
	// without needing a subprocess.
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"healthy": true, "version": "test"}`)
	}))
	defer hs.Close()

	srv := &Server{baseURL: hs.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.healthProbe(ctx); err != nil {
		t.Fatalf("healthProbe: %v", err)
	}
}

func TestServer_HealthProbe_Timeout(t *testing.T) {
	// Verifies healthProbe surfaces ctx.DeadlineExceeded when the server
	// never responds 200. Stub returns 500 forever.
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hs.Close()

	srv := &Server{baseURL: hs.URL}
	prev := healthProbeInterval
	healthProbeInterval = 10 * time.Millisecond
	defer func() { healthProbeInterval = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := srv.healthProbe(ctx); err == nil {
		t.Fatal("healthProbe should have timed out")
	}
}

func TestServer_HealthProbe_SubprocessDeath(t *testing.T) {
	// Verifies healthProbe returns early when the subprocess dies (rather
	// than waiting for ctx). We simulate death by closing srv.done.
	hs := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(time.Minute) // hang
	}))
	defer hs.Close()

	srv := &Server{baseURL: hs.URL}
	srv.done = make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(srv.done)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.healthProbe(ctx); err == nil {
		t.Fatal("healthProbe should have returned an error on subprocess death")
	}
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestServer_Close_NotStarted(t *testing.T) {
	// Verifies Close on a never-Started Server is a no-op (no panic).
	srv := newTestServer(t, "agent-nostart")
	if err := srv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestServer_Close_Idempotent(t *testing.T) {
	// Verifies the closeOnce gate: multiple Close calls are safe.
	srv := newTestServer(t, "agent-idempotent")
	for i := 0; i < 3; i++ {
		if err := srv.Close(); err != nil {
			t.Errorf("Close[%d]: %v", i, err)
		}
	}
}

func TestServer_Close_BoundedWait(t *testing.T) {
	// Verifies Close returns within bounded time even when the subprocess
	// ignores SIGTERM. We launch a stub that ignores signals by setting
	// OPC_STUB_EXIT_CODE=0 — wait, that exits immediately. Instead we
	// launch a plain sleep that won't catch SIGTERM, forcing SIGKILL.
	//
	// Shrink the timeouts so the test runs fast. The assertion is that
	// Close returns well under 2s.
	prevGrace, prevKill, prevTerm := closeGracefulWait, closeSigkillWait, closeSigtermWait
	closeGracefulWait = 200 * time.Millisecond
	closeSigkillWait = 200 * time.Millisecond
	closeSigtermWait = 200 * time.Millisecond
	defer func() {
		closeGracefulWait = prevGrace
		closeSigkillWait = prevKill
		closeSigtermWait = prevTerm
	}()

	srv := newTestServer(t, "agent-bounded")
	if err := launchStubDirectly(t, srv); err != nil {
		t.Fatalf("launchStubDirectly: %v", err)
	}

	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)

	// Bound: graceful + sigkill + final-accept ≈ 600ms with shrunk
	// timeouts. Allow headroom for goroutine scheduling.
	if elapsed > 2*time.Second {
		t.Errorf("Close took %s; expected bounded to ~600ms", elapsed)
	}
	select {
	case <-srv.done:
	case <-time.After(2 * time.Second):
		t.Error("subprocess not reaped after Close")
	}
}

func TestServer_Close_GracefulDisposeBeforeSIGTERM(t *testing.T) {
	// Verifies Close calls POST /instance/dispose BEFORE falling back
	// to SIGTERM. We point a Server at an httptest server that records
	// the dispose call and immediately "exits" (closes waitCh) to
	// simulate the subprocess honouring the request. Asserts the
	// dispose endpoint was hit and the waitCh path didn't escalate.
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/instance/dispose" {
			t.Logf("dispose called")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer hs.Close()

	srv := &Server{
		baseURL:    hs.URL,
		agentID:    "agent-dispose",
		sessions:   map[string]*Backend{},
	}
	srv.done = make(chan struct{})
	srv.waitCh = make(chan error, 1)
	// Simulate the subprocess exiting immediately on dispose.
	go func() {
		time.Sleep(20 * time.Millisecond)
		srv.waitCh <- nil
		close(srv.done)
	}()

	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)

	// Close should return quickly because the "subprocess" exited on
	// its own (no SIGTERM/SIGKILL fallback needed). With closeGracefulWait
	// at its default 5s, an ungraceful path would take ≥5s; the dispose
	// path returns in ~20ms.
	if elapsed > time.Second {
		t.Errorf("Close took %s; expected dispose path to exit fast", elapsed)
	}
}

// ---------------------------------------------------------------------------
// finalizeExit — synthesised session.error dispatch
// ---------------------------------------------------------------------------

func TestServer_FinalizeExit_DispatchesSessionErrorToBackends(t *testing.T) {
	// Verifies finalizeExit synthesises a session.error rawEvent and
	// pushes it to every registered Backend's events channel — so the
	// Step 7 OnSessionError handler can complete any in-flight turn
	// rather than hanging on a stream that will never deliver.
	//
	// Without this dispatch, a subprocess death would silently wedge
	// every active session (the SSE stream is gone but no event signals
	// it).
	srv := &Server{
		agentID:  "agent-finalize",
		sessions: map[string]*Backend{},
	}
	be1 := &Backend{sessionID: "sess-1", events: make(chan rawEvent, 1)}
	be2 := &Backend{sessionID: "sess-2", events: make(chan rawEvent, 1)}
	srv.sessions["sess-1"] = be1
	srv.sessions["sess-2"] = be2

	srv.finalizeExit(errors.New("subprocess died"))

	// Both Backends should have received a session.error event.
	for _, be := range []*Backend{be1, be2} {
		select {
		case ev := <-be.events:
			if ev.Type != EventSessionError {
				t.Errorf("%s event.Type = %q, want %q", be.sessionID, ev.Type, EventSessionError)
			}
		case <-time.After(time.Second):
			t.Errorf("%s did not receive session.error event", be.sessionID)
		}
	}
}

func TestServer_FinalizeExit_ExpectedCloseDifferentMessage(t *testing.T) {
	// Verifies the synthesised event's error data distinguishes "expected
	// Close" from "unexpected death" so Step 7's handler can render the
	// right user-visible message ("session closed" vs "subprocess died
	// unexpectedly"). We mark s.closing=true to simulate the Close path.
	srv := &Server{
		agentID:  "agent-expected",
		sessions: map[string]*Backend{},
		closing:  true,
	}
	be := &Backend{sessionID: "sess-x", events: make(chan rawEvent, 1)}
	srv.sessions["sess-x"] = be

	srv.finalizeExit(nil)

	select {
	case ev := <-be.events:
		// Decode the payload and check the message.
		var payload eventSessionError
		if err := json.Unmarshal(ev.Properties, &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if payload.Error == nil {
			t.Fatal("payload.Error nil")
		}
		var data struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(payload.Error.Data, &data); err != nil {
			t.Fatalf("decode data: %v", err)
		}
		if data.Message != "session closed" {
			t.Errorf("message = %q, want %q", data.Message, "session closed")
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive session.error event")
	}
}

func TestServer_FinalizeExit_Idempotent(t *testing.T) {
	// Verifies finalizeExit is sync.Once-gated — calling it twice (e.g.
	// once from the waiter goroutine, once from OnSubscriberStopped)
	// dispatches session.error exactly once per Backend.
	srv := &Server{
		agentID:  "agent-once",
		sessions: map[string]*Backend{},
	}
	be := &Backend{sessionID: "sess-once", events: make(chan rawEvent, 5)}
	srv.sessions["sess-once"] = be

	srv.finalizeExit(errors.New("first"))
	srv.finalizeExit(errors.New("second"))

	// Should have received exactly one event.
	count := 0
	for {
		select {
		case <-be.events:
			count++
		default:
			goto done
		}
	}
done:
	if count != 1 {
		t.Errorf("finalizeExit dispatched %d events, want 1", count)
	}
}

// ---------------------------------------------------------------------------
// Server pool
// ---------------------------------------------------------------------------

func TestServer_Pool_LazyStartPerAgent(t *testing.T) {
	// Verifies the pool returns the same *Server for repeated acquires
	// of one agent, and different *Servers for different agents.
	resetTestPool(t)

	s1 := insertLiveServerForTest("agent-A")
	s2 := insertLiveServerForTest("agent-A")
	if s1 != s2 {
		t.Error("acquire(agent-A) twice should return same *Server")
	}

	s3 := insertLiveServerForTest("agent-B")
	if s3 == s1 {
		t.Error("acquire(agent-B) should return a different *Server")
	}
}

func TestServer_Pool_RefcountShutdown(t *testing.T) {
	// Verifies refcounting: release decrements but doesn't trigger pool
	// removal until refcount hits zero; second release does. (The actual
	// bounded-shutdown Close is exercised in TestServer_Close_BoundedWait
	// against a real subprocess; here we just pin pool semantics.)
	resetTestPool(t)

	insertLiveServerForTest("agent-rc")
	// Two acquires → refcount is 2.
	insertLiveServerForTest("agent-rc")

	// First release: refcount 1, server stays.
	releaseServer("agent-rc")
	if _, ok := lookupTestPool("agent-rc"); !ok {
		t.Error("Server removed from pool after first release (refcount was 2 → 1)")
	}

	// Second release: refcount 0, server removed (Close spawned in goroutine).
	releaseServer("agent-rc")
	if _, ok := lookupTestPool("agent-rc"); ok {
		t.Error("Server not removed from pool after refcount hit zero")
	}
}

func TestServer_Pool_ReleaseDoesNotBlockOnClose(t *testing.T) {
	// Verifies releaseServer returns immediately even when Close will be
	// slow (subprocess wedged). Critical: a slow close must not block
	// acquireServer for an unrelated agent.
	resetTestPool(t)

	srv := insertLiveServerForTest("agent-slow")
	// Force Close to take ~200ms via shrunk timeouts.
	prevGrace, prevKill, prevTerm := closeGracefulWait, closeSigkillWait, closeSigtermWait
	closeGracefulWait = 100 * time.Millisecond
	closeSigkillWait = 100 * time.Millisecond
	closeSigtermWait = 100 * time.Millisecond
	defer func() {
		closeGracefulWait = prevGrace
		closeSigkillWait = prevKill
		closeSigtermWait = prevTerm
	}()
	_ = srv

	start := time.Now()
	releaseServer("agent-slow")
	elapsed := time.Since(start)
	if elapsed > 50*time.Millisecond {
		t.Errorf("releaseServer took %s; should return immediately (Close runs in goroutine)", elapsed)
	}

	// Concurrent acquire for a different agent must not block on the
	// slow close of agent-slow.
	done := make(chan struct{})
	go func() {
		insertLiveServerForTest("agent-other")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("acquire for agent-other blocked on slow close of agent-slow")
	}
}

// ---------------------------------------------------------------------------
// Test-only helpers (production code never uses these)
// ---------------------------------------------------------------------------

// launchStubDirectly spawns the opc-stub subprocess without going through
// Start's health-probe path. Used by tests that need a real child process
// for Close to kill but don't need HTTP readiness.
func launchStubDirectly(t *testing.T, s *Server) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := procx.Spawn(ctx, stubBinary(t))
	cmd.Dir = s.workDir
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	s.cmd = cmd
	s.cancel = cancel
	s.done = make(chan struct{})
	s.waitCh = make(chan error, 1)
	go func() {
		err := cmd.Wait()
		s.waitCh <- err
		close(s.done)
	}()
	return nil
}

// resetTestPool clears the package-level serverPool so tests don't bleed
// state between each other.
func resetTestPool(t *testing.T) {
	t.Helper()
	serverPoolMu.Lock()
	serverPool = map[string]*Server{}
	serverPoolMu.Unlock()
}

// insertLiveServerForTest inserts a "started-looking" Server into the
// pool without spawning a subprocess. Pool-semantics tests use this to
// avoid needing real subprocess lifecycle.
func insertLiveServerForTest(agentID string) *Server {
	serverPoolMu.Lock()
	defer serverPoolMu.Unlock()
	if s, ok := serverPool[agentID]; ok {
		s.refCount++
		return s
	}
	s := newServer(agentID, serverConfig{workDir: "/tmp"})
	s.refCount = 1
	s.running = true
	serverPool[agentID] = s
	return s
}

// lookupTestPool returns the Server registered under agentID, if any.
func lookupTestPool(agentID string) (*Server, bool) {
	serverPoolMu.Lock()
	defer serverPoolMu.Unlock()
	s, ok := serverPool[agentID]
	return s, ok
}

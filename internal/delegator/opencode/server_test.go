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

func TestServer_buildCmdEnv_IncludesExtraEnv(t *testing.T) {
	// Verifies the subprocess environment includes the exec bridge's
	// BASH_ENV + FOCI_SOCK so the LLM can call foci_todo etc. via the
	// bash tool. Also verifies OPENCODE_SERVER_PASSWORD is present when
	// set. Tests the env-building logic without spawning a subprocess.
	srv := &Server{
		serverPassword: "s3cret",
		extraEnv: map[string]string{
			"BASH_ENV":  "/tmp/foci_funcs.sh",
			"FOCI_SOCK": "/tmp/foci_sock",
		},
	}
	env := srv.buildCmdEnv()

	has := func(prefix string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}

	if !has("BASH_ENV=/tmp/foci_funcs.sh") {
		t.Error("BASH_ENV missing from subprocess env — exec bridge won't work")
	}
	if !has("FOCI_SOCK=/tmp/foci_sock") {
		t.Error("FOCI_SOCK missing from subprocess env — exec bridge won't work")
	}
	if !has("OPENCODE_SERVER_PASSWORD=s3cret") {
		t.Error("OPENCODE_SERVER_PASSWORD missing from subprocess env")
	}
}

func TestServer_buildCmdEnv_NoExtraEnv(t *testing.T) {
	// Verifies buildCmdEnv works with no extraEnv (the default — no exec
	// bridge configured, or pre-Step-17 behaviour).
	srv := &Server{}
	env := srv.buildCmdEnv()
	// Should have at least the parent process's env (PATH etc.).
	if len(env) == 0 {
		t.Error("buildCmdEnv returned empty env")
	}
	// Should NOT have BASH_ENV or FOCI_SOCK.
	for _, e := range env {
		if strings.HasPrefix(e, "BASH_ENV=") || strings.HasPrefix(e, "FOCI_SOCK=") {
			t.Errorf("unexpected exec bridge env var in default buildCmdEnv: %s", e)
		}
	}
}

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
	srv := newTestServer(t, "agent-bounded")
	srv.closeGracefulWait = 200 * time.Millisecond
	srv.closeSigtermWait = 200 * time.Millisecond
	srv.closeSigkillWait = 200 * time.Millisecond
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
	// at its default 500ms, an ungraceful path would take ≥500ms; the dispose
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

	srv := insertLiveServerForTest("agent-rc")
	// Two acquires → refcount is 2.
	insertLiveServerForTest("agent-rc")

	// First release: refcount 1, server stays.
	releaseServer("agent-rc", srv)
	if _, ok := lookupTestPool("agent-rc"); !ok {
		t.Error("Server removed from pool after first release (refcount was 2 → 1)")
	}

	// Second release: refcount 0, server removed (Close spawned in goroutine).
	releaseServer("agent-rc", srv)
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
	// Force Close to take ~100ms via shrunk timeouts.
	srv.closeGracefulWait = 100 * time.Millisecond
	srv.closeSigtermWait = 100 * time.Millisecond
	srv.closeSigkillWait = 100 * time.Millisecond
	start := time.Now()
	releaseServer("agent-slow", srv)
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

func TestServer_Pool_AcquireEvictsDeadAndRespawns(t *testing.T) {
	// Verifies the core fix: a DEAD pooled Server is evicted and acquireServer
	// spawns a fresh one rather than handing the corpse back. Uses the
	// opc-stub binary so Start runs through its real launch path; the health
	// probe fails (stub serves no HTTP), so acquireServer returns an error —
	// but the assertion is about the POOL, not the spawn outcome: the dead
	// entry must be gone, and a fresh *Server (≠ the dead one) must have been
	// constructed and attempted.
	resetTestPool(t)

	// Pool a dead Server (running=false) under agent-dead.
	dead := newServer("agent-dead", serverConfig{workDir: "/tmp"})
	dead.refCount = 1
	dead.running = false // dead
	serverPoolMu.Lock()
	serverPool["agent-dead"] = dead
	serverPoolMu.Unlock()

	if dead.isAlive() {
		t.Fatal("precondition: dead server should report not alive")
	}

	cfg := serverConfig{
		workDir:    t.TempDir(),
		binaryPath: stubBinary(t),
		hostname:   "127.0.0.1",
		port:       0,
	}
	// OPC_STUB_EXIT_CODE makes the fresh subprocess exit immediately, so
	// Start's health probe returns the subprocess-death error promptly
	// (acquireServer passes context.Background(), so without an early death
	// the probe would loop forever). We only need the spawn to be ATTEMPTED
	// — the assertion is about the pool, not Start's success.
	got, err := acquireServer("agent-dead", cfg, map[string]string{"OPC_STUB_EXIT_CODE": "1"})
	if err == nil {
		t.Logf("acquireServer unexpectedly succeeded (got=%p) — fine for this test", got)
	}
	if got == dead {
		t.Fatal("acquireServer returned the DEAD pooled server — the bug is not fixed")
	}
	// The dead entry must no longer be the pooled server: it was either
	// deleted (spawn failed before re-insert) or replaced by the fresh one.
	if cur, ok := lookupTestPool("agent-dead"); ok && cur == dead {
		t.Fatal("dead server still pooled after acquireServer — not evicted")
	}
}

func TestServer_Pool_AcquireWritesPluginsBeforeSpawn(t *testing.T) {
	// Invariant: acquireServer materialises the foci workspace plugins
	// (session-env routing + blank-system) BEFORE it spawns the subprocess, at
	// the single spawn chokepoint — so EVERY spawner (interactive Start, batch
	// RunBatch, any future caller) gets a fully-wired server by construction.
	// Regression for the batch-spawn gap: a batch that spawns the shared server
	// first must not strand later interactive sessions on a plugin-less server
	// (no per-session FOCI_SOCK/BASH_ENV override → misrouted session tools).
	// Uses the opc-stub with an immediate exit so Start fails fast; the plugin
	// files must exist regardless of the spawn outcome (they're written first).
	resetTestPool(t)

	workDir := t.TempDir()
	cfg := serverConfig{
		workDir:    workDir,
		binaryPath: stubBinary(t),
		hostname:   "127.0.0.1",
		port:       0,
	}
	_, _ = acquireServer("agent-plugin-invariant", cfg, map[string]string{"OPC_STUB_EXIT_CODE": "1"})

	for _, fn := range []string{sessionEnvPluginFn, blankSystemPluginFn} {
		p := filepath.Join(workDir, ".opencode", "plugin", fn)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("plugin %q not written before spawn: %v (acquireServer must ensure plugins on every spawn path)", fn, err)
		}
	}
}

func TestServer_Pool_AcquireReusesLiveServer(t *testing.T) {
	// Sanity counterpart: a LIVE pooled Server is reused (refcount bumped),
	// never respawned. Pins the happy path so the eviction fix doesn't
	// over-evict.
	resetTestPool(t)

	live := insertLiveServerForTest("agent-live") // running=true, refCount=1
	got, err := acquireServer("agent-live", serverConfig{}, nil)
	if err != nil {
		t.Fatalf("acquireServer on live server: %v", err)
	}
	if got != live {
		t.Error("acquireServer did not reuse the live pooled server")
	}
	if live.refCount != 2 {
		t.Errorf("refCount = %d, want 2 after second acquire", live.refCount)
	}
}

func TestServer_Pool_ReleaseStaleServerIsNoOp(t *testing.T) {
	// THE CORRUPTION CASE. Server A pooled with refCount=2 (sessions S1,S2).
	// A dies → finalizeExit evicts A. S3 acquires → fresh Server B pooled,
	// refCount=1. Then S1 releases its stale handle on A: releaseServer must
	// be a NO-OP — B's refCount stays 1, B is NOT closed/evicted.
	resetTestPool(t)

	// Server A, two sessions.
	a := insertLiveServerForTest("agent-corrupt") // refCount=1
	insertLiveServerForTest("agent-corrupt")      // refCount=2
	if a.refCount != 2 {
		t.Fatalf("precondition: A.refCount = %d, want 2", a.refCount)
	}

	// A dies → finalizeExit runs, clears running and evicts A from the pool.
	a.finalizeExit(errors.New("subprocess died"))
	if a.isAlive() {
		t.Error("A should report not alive after finalizeExit")
	}
	if cur, ok := lookupTestPool("agent-corrupt"); ok && cur == a {
		t.Fatal("finalizeExit did not evict dead server A from the pool")
	}

	// S3 acquires → a fresh Server B takes A's slot.
	b := insertLiveServerForTest("agent-corrupt") // fresh: refCount=1
	if b == a {
		t.Fatal("expected a fresh Server B after A's eviction")
	}
	if b.refCount != 1 {
		t.Fatalf("B.refCount = %d, want 1", b.refCount)
	}

	// S1 releases its STALE handle on A. Pool holds B≠A, so this must be a
	// no-op: B's refcount unchanged, B still pooled (not closed).
	releaseServer("agent-corrupt", a)

	if b.refCount != 1 {
		t.Errorf("B.refCount = %d after stale release of A; want 1 (corruption!)", b.refCount)
	}
	if cur, ok := lookupTestPool("agent-corrupt"); !ok || cur != b {
		t.Error("releasing stale A evicted/replaced the live Server B from the pool (corruption!)")
	}
}

func TestServer_FinalizeExit_EvictsFromPool(t *testing.T) {
	// Verifies finalizeExit proactively removes the dead Server from the
	// pool, so it's gone before the next acquireServer even runs.
	resetTestPool(t)

	s := insertLiveServerForTest("agent-evict") // pooled, running=true
	if _, ok := lookupTestPool("agent-evict"); !ok {
		t.Fatal("precondition: server should be pooled")
	}

	s.finalizeExit(errors.New("subprocess died"))

	if _, ok := lookupTestPool("agent-evict"); ok {
		t.Error("finalizeExit did not evict the dead server from the pool")
	}
}

func TestServer_FinalizeExit_DoesNotEvictSuccessor(t *testing.T) {
	// Verifies the guard in finalizeExit: if a respawn already replaced the
	// dead server in the pool, finalizeExit must NOT delete the successor.
	resetTestPool(t)

	dead := insertLiveServerForTest("agent-succ") // pooled
	// Simulate a respawn having already taken the slot: replace the pooled
	// entry with a different Server sharing the same agentID.
	successor := newServer("agent-succ", serverConfig{workDir: "/tmp"})
	successor.running = true
	successor.refCount = 1
	serverPoolMu.Lock()
	serverPool["agent-succ"] = successor
	serverPoolMu.Unlock()

	dead.finalizeExit(errors.New("subprocess died"))

	cur, ok := lookupTestPool("agent-succ")
	if !ok {
		t.Fatal("finalizeExit evicted the successor (pool empty) — guard failed")
	}
	if cur != successor {
		t.Error("pooled server is not the successor after dead.finalizeExit")
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

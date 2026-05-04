package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/session"
)

// ---------------------------------------------------------------------------
// mockBackendDM implements delegator.Delegator for DelegatedManager tests.
// Every method is a no-op or records calls unless overridden via function
// fields. This keeps tests focused on the manager logic, not the backend.
// ---------------------------------------------------------------------------
type mockBackendDM struct {
	mu sync.Mutex

	started     bool
	closed      bool
	interrupted bool
	startErr    error
	startOpts   delegator.StartOptions

	permPromptFunc      delegator.PermissionPromptFunc
	onPermCleared       func()
	onPermCancelled     func(requestID, toolName, reason string)
	onSessionReady      func(string)
	typingFunc          func(bool)
	sessionID           string
	sessionFilePath     string
	waitReadyErr        error
	waitReadyDelay      time.Duration
	turnInFlight        bool
	running             bool
	waitForTurnErr      error
	waitForTurnBlock    chan struct{} // if non-nil, WaitForTurn blocks until closed
	sendToPaneFn        func(context.Context, string, *delegator.EventHandler) (*delegator.TurnResult, error)
	sendCommandFn       func(context.Context, string) error
	closeFn             func() error
}

func (m *mockBackendDM) Start(_ context.Context, opts delegator.StartOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startErr != nil {
		return m.startErr
	}
	m.started = true
	m.startOpts = opts
	// Fire the session-ready callback if set, simulating CC discovering its UUID.
	if m.onSessionReady != nil && m.sessionID != "" {
		go m.onSessionReady(m.sessionID)
	}
	return nil
}

func (m *mockBackendDM) SendToPane(ctx context.Context, text string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
	if m.sendToPaneFn != nil {
		return m.sendToPaneFn(ctx, text, handler)
	}
	// Default: immediately complete the turn via handler.
	result := &delegator.TurnResult{Text: "ok"}
	if handler != nil && handler.OnTurnComplete != nil {
		handler.OnTurnComplete(result)
	}
	return result, nil
}

func (m *mockBackendDM) WaitForTurn(ctx context.Context) error {
	if m.waitForTurnBlock != nil {
		select {
		case <-m.waitForTurnBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.waitForTurnErr
}

func (m *mockBackendDM) IsTurnInFlight() bool { return m.turnInFlight }

func (m *mockBackendDM) SendCommand(ctx context.Context, cmd string) error {
	if m.sendCommandFn != nil {
		return m.sendCommandFn(ctx, cmd)
	}
	return nil
}

func (m *mockBackendDM) IsRunning() bool { return m.running }

func (m *mockBackendDM) Restart(_ context.Context) error { return nil }

func (m *mockBackendDM) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.permPromptFunc = fn
}

func (m *mockBackendDM) SetOnPermissionCleared(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onPermCleared = fn
}

func (m *mockBackendDM) SetOnPermissionCancelled(fn func(requestID, toolName, reason string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onPermCancelled = fn
}

func (m *mockBackendDM) SetOnSessionReady(fn func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onSessionReady = fn
}

func (m *mockBackendDM) SetTypingFunc(fn func(bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.typingFunc = fn
}

func (m *mockBackendDM) SendKeystroke(_ context.Context, _ string) error { return nil }
func (m *mockBackendDM) SendSpecialKey(_ context.Context, _ string) error { return nil }

func (m *mockBackendDM) Interrupt(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interrupted = true
	return nil
}

func (m *mockBackendDM) SessionID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionID
}

func (m *mockBackendDM) SessionFilePath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionFilePath
}

func (m *mockBackendDM) WaitReady(ctx context.Context) error {
	if m.waitReadyDelay > 0 {
		select {
		case <-time.After(m.waitReadyDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.waitReadyErr
}

func (m *mockBackendDM) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

// wasClosed returns true if Close was called.
func (m *mockBackendDM) wasClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// wasInterrupted returns true if Interrupt was called.
func (m *mockBackendDM) wasInterrupted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.interrupted
}

// ---------------------------------------------------------------------------
// newTestManager creates a DelegatedManager wired up with mock factories.
// Each call to NewBackend returns a fresh mockBackendDM and appends it to
// the returned slice for inspection. The optional idx enables resume-ID
// persistence (pass nil to disable).
// ---------------------------------------------------------------------------
func newTestManager(t *testing.T, idx *session.SessionIndex) (*DelegatedManager, *[]*mockBackendDM) {
	t.Helper()
	var mocks []*mockBackendDM
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			be := &mockBackendDM{running: true}
			mocks = append(mocks, be)
			return be, nil
		},
		StartOpts:    delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:      "test-agent",
		SessionIndex: idx,
		IdleTimeout:  time.Hour, // don't idle-reap during tests
	}
	t.Cleanup(func() { mgr.Close() })
	return mgr, &mocks
}

// newTestSessionIndex creates a SessionIndex backed by a temp SQLite file.
func newTestSessionIndex(t *testing.T) *session.SessionIndex {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	idx, err := session.NewSessionIndex(dbPath)
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestGet_LazyCreation(t *testing.T) {
	// Proves that Get creates a new backend on first call and returns the same
	// backend on subsequent calls with the same session key base.
	mgr, mocks := newTestManager(t, nil)

	be1, err := mgr.Get(context.Background(), "test-agent/c123/1000")
	if err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	if be1 == nil {
		t.Fatal("Get #1 returned nil backend")
	}
	if len(*mocks) != 1 {
		t.Fatalf("expected 1 mock created, got %d", len(*mocks))
	}

	// Same session key returns the same backend (no new mock created).
	be2, err := mgr.Get(context.Background(), "test-agent/c123/1000")
	if err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if be1 != be2 {
		t.Error("expected same backend for same session key")
	}
	if len(*mocks) != 1 {
		t.Errorf("expected still 1 mock, got %d", len(*mocks))
	}

	// Different session key creates a separate backend.
	// In production, use RotateBackendKey to migrate after compaction.
	be3, err := mgr.Get(context.Background(), "test-agent/c123/2000")
	if err != nil {
		t.Fatalf("Get #3 (different key): %v", err)
	}
	if be1 == be3 {
		t.Error("expected different backend for different session key")
	}
	if len(*mocks) != 2 {
		t.Errorf("expected 2 mocks after different key, got %d", len(*mocks))
	}
}

func TestGet_DifferentKeys(t *testing.T) {
	// Proves that different session key bases produce different backends.
	mgr, mocks := newTestManager(t, nil)

	be1, err := mgr.Get(context.Background(), "test-agent/c123")
	if err != nil {
		t.Fatalf("Get key1: %v", err)
	}
	be2, err := mgr.Get(context.Background(), "test-agent/c456")
	if err != nil {
		t.Fatalf("Get key2: %v", err)
	}

	if be1 == be2 {
		t.Error("different session keys should produce different backends")
	}
	if len(*mocks) != 2 {
		t.Errorf("expected 2 mocks, got %d", len(*mocks))
	}
}

func TestGet_ResumePath(t *testing.T) {
	// Proves that a saved session UUID is loaded and passed as ResumeSessionID
	// on the next Get() call for that session key.
	idx := newTestSessionIndex(t)
	mgr, mocks := newTestManager(t, idx)

	// Simulate a previously saved session UUID.
	base := "test-agent/c789"
	stateKey := "cc_session:" + base
	if err := idx.SetAgentMetadata("test-agent", stateKey, "saved-uuid-123"); err != nil {
		t.Fatalf("SetAgentMetadata: %v", err)
	}

	_, err := mgr.Get(context.Background(), base)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mock := (*mocks)[0]
	if mock.startOpts.ResumeSessionID != "saved-uuid-123" {
		t.Errorf("ResumeSessionID = %q, want %q", mock.startOpts.ResumeSessionID, "saved-uuid-123")
	}
}

func TestGet_ResumeFailsFallsBackToFresh(t *testing.T) {
	// Proves that when Start with a resume ID fails, the manager retries
	// without the resume ID and succeeds.
	idx := newTestSessionIndex(t)

	var callCount int
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			callCount++
			be := &mockBackendDM{running: true}
			if callCount == 1 {
				// First backend: fail on Start with resume.
				be.startErr = errors.New("stale session")
			}
			return be, nil
		},
		StartOpts:    delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:      "test-agent",
		SessionIndex: idx,
		IdleTimeout:  time.Hour,
	}
	t.Cleanup(func() { mgr.Close() })

	base := "test-agent/c111"
	stateKey := "cc_session:" + base
	if err := idx.SetAgentMetadata("test-agent", stateKey, "stale-uuid"); err != nil {
		t.Fatalf("SetAgentMetadata: %v", err)
	}

	be, err := mgr.Get(context.Background(), base)
	if err != nil {
		t.Fatalf("Get should succeed after retry: %v", err)
	}
	if be == nil {
		t.Fatal("Get returned nil backend")
	}
	if callCount != 2 {
		t.Errorf("expected 2 NewBackend calls (first fail + retry), got %d", callCount)
	}
}

func TestGet_NewBackendError(t *testing.T) {
	// Proves that Get returns an error when NewBackend fails.
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			return nil, errors.New("factory broken")
		},
		StartOpts: delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:   "test-agent",
	}

	_, err := mgr.Get(context.Background(), "test-agent/c999")
	if err == nil {
		t.Fatal("expected error from Get when NewBackend fails")
	}
	if got := err.Error(); !contains(got, "factory broken") {
		t.Errorf("error should contain cause, got: %s", got)
	}
}

func TestGet_StartError_NoResume(t *testing.T) {
	// Proves that Start errors without a resume ID propagate directly.
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			return &mockBackendDM{startErr: errors.New("start failed"), running: true}, nil
		},
		StartOpts: delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:   "test-agent",
	}

	_, err := mgr.Get(context.Background(), "test-agent/c555")
	if err == nil {
		t.Fatal("expected error from Get when Start fails")
	}
	if got := err.Error(); !contains(got, "start failed") {
		t.Errorf("error should contain 'start failed', got: %s", got)
	}
}

func TestGet_SetsLabelFromBase(t *testing.T) {
	// Proves that the StartOptions.Label is derived from the session key base
	// with slashes replaced by dashes.
	mgr, mocks := newTestManager(t, nil)

	_, err := mgr.Get(context.Background(), "myagent/c42/v1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mock := (*mocks)[0]
	want := "myagent-c42-v1"
	if mock.startOpts.Label != want {
		t.Errorf("Label = %q, want %q", mock.startOpts.Label, want)
	}
}

func TestStopSession_InterruptsBackend(t *testing.T) {
	// Proves that StopSession calls Interrupt on the correct backend.
	mgr, mocks := newTestManager(t, nil)

	_, err := mgr.Get(context.Background(), "test-agent/c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if err := mgr.StopSession(context.Background(), "test-agent/c1"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	if !(*mocks)[0].wasInterrupted() {
		t.Error("expected Interrupt to be called")
	}
}

func TestStopSession_NoBackend(t *testing.T) {
	// Proves that StopSession returns an error when no backend exists.
	mgr, _ := newTestManager(t, nil)

	err := mgr.StopSession(context.Background(), "test-agent/nonexistent")
	if err == nil {
		t.Fatal("expected error for missing backend")
	}
}

func TestSetPermissionPending_And_IsPermissionPending(t *testing.T) {
	// Proves that SetPermissionPending(true) makes IsPermissionPending return
	// true, and SetPermissionPending(false) clears it.
	mgr, _ := newTestManager(t, nil)
	sk := "test-agent/c1"

	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if mgr.IsPermissionPending(sk) {
		t.Error("should not be pending initially")
	}

	mgr.SetPermissionPending(sk, true)
	if !mgr.IsPermissionPending(sk) {
		t.Error("should be pending after SetPermissionPending(true)")
	}

	mgr.SetPermissionPending(sk, false)
	if mgr.IsPermissionPending(sk) {
		t.Error("should not be pending after SetPermissionPending(false)")
	}
}

func TestSetPermissionPending_NoBackend(t *testing.T) {
	// Proves that SetPermissionPending is a no-op when no backend exists.
	mgr, _ := newTestManager(t, nil)
	mgr.SetPermissionPending("test-agent/nonexistent", true) // should not panic
}

func TestIsPermissionPending_NoBackend(t *testing.T) {
	// Proves that IsPermissionPending returns false when no backend exists.
	mgr, _ := newTestManager(t, nil)
	if mgr.IsPermissionPending("test-agent/nonexistent") {
		t.Error("should return false for missing backend")
	}
}

func TestWaitForPermission_ImmediateReturn(t *testing.T) {
	// Proves that WaitForPermission returns immediately when no permission
	// prompt is pending.
	mgr, _ := newTestManager(t, nil)
	sk := "test-agent/c1"

	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Should return immediately since not pending.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mgr.WaitForPermission(ctx, sk); err != nil {
		t.Fatalf("WaitForPermission: %v", err)
	}
}

func TestWaitForPermission_NoBackend(t *testing.T) {
	// Proves that WaitForPermission returns nil when no backend exists.
	mgr, _ := newTestManager(t, nil)
	if err := mgr.WaitForPermission(context.Background(), "test-agent/none"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestWaitForPermission_BlocksAndUnblocks(t *testing.T) {
	// Proves that WaitForPermission blocks when a permission prompt is pending
	// and unblocks when SetPermissionPending(false) is called.
	mgr, _ := newTestManager(t, nil)
	sk := "test-agent/c1"

	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mgr.SetPermissionPending(sk, true)

	var done atomic.Bool
	waitErr := make(chan error, 1)
	go func() {
		err := mgr.WaitForPermission(context.Background(), sk)
		done.Store(true)
		waitErr <- err
	}()

	// Give the goroutine time to block.
	time.Sleep(50 * time.Millisecond)
	if done.Load() {
		t.Fatal("WaitForPermission should be blocking")
	}

	// Clear permission — should unblock.
	mgr.SetPermissionPending(sk, false)

	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("WaitForPermission returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForPermission did not unblock after clearing permission")
	}
}

func TestWaitForPermission_ContextCancellation(t *testing.T) {
	// Proves that WaitForPermission returns ctx.Err() when the context is
	// cancelled while waiting for a permission prompt to resolve.
	mgr, _ := newTestManager(t, nil)
	sk := "test-agent/c1"

	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mgr.SetPermissionPending(sk, true)

	ctx, cancel := context.WithCancel(context.Background())
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- mgr.WaitForPermission(ctx, sk)
	}()

	// Give the goroutine time to enter the wait.
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case err := <-waitErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForPermission did not return on context cancellation")
	}
}

func TestResetSession_ClosesAndClears(t *testing.T) {
	// Proves that ResetSession closes the backend, removes it from the map,
	// and clears the saved resume ID so the next Get creates a fresh session.
	idx := newTestSessionIndex(t)
	mgr, mocks := newTestManager(t, idx)

	sk := "test-agent/c1"

	// Pre-populate a resume ID.
	stateKey := "cc_session:" + sk
	if err := idx.SetAgentMetadata("test-agent", stateKey, "some-uuid"); err != nil {
		t.Fatalf("SetAgentMetadata: %v", err)
	}

	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if mgr.Count() != 1 {
		t.Fatalf("Count = %d, want 1", mgr.Count())
	}

	firstMock := (*mocks)[0]
	mgr.ResetSession(sk)

	if !firstMock.wasClosed() {
		t.Error("expected backend to be closed")
	}
	if mgr.Count() != 0 {
		t.Errorf("Count = %d, want 0 after reset", mgr.Count())
	}

	// Resume ID should be cleared.
	val, _ := idx.GetAgentMetadata("test-agent", stateKey)
	if val != "" {
		t.Errorf("resume ID should be cleared, got %q", val)
	}
}

func TestResetSession_NoBackend(t *testing.T) {
	// Proves that ResetSession is a no-op when no backend exists (no panic).
	mgr, _ := newTestManager(t, nil)
	mgr.ResetSession("test-agent/nonexistent") // should not panic
}

func TestClose_ShutsDownAll(t *testing.T) {
	// Proves that Close shuts down all managed backends and the idle reaper.
	mgr, mocks := newTestManager(t, nil)

	_, err := mgr.Get(context.Background(), "test-agent/c1")
	if err != nil {
		t.Fatalf("Get c1: %v", err)
	}
	_, err = mgr.Get(context.Background(), "test-agent/c2")
	if err != nil {
		t.Fatalf("Get c2: %v", err)
	}

	if mgr.Count() != 2 {
		t.Fatalf("Count = %d, want 2", mgr.Count())
	}

	mgr.Close()

	for i, m := range *mocks {
		if !m.wasClosed() {
			t.Errorf("mock[%d] was not closed", i)
		}
	}
	if mgr.Count() != 0 {
		t.Errorf("Count = %d after Close, want 0", mgr.Count())
	}
}

func TestClose_SavesResumeIDs(t *testing.T) {
	// Proves that Close persists session UUIDs so backends can be resumed
	// after restart.
	idx := newTestSessionIndex(t)
	mgr, mocks := newTestManager(t, idx)

	_, err := mgr.Get(context.Background(), "test-agent/c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Set a session ID on the mock so Close can save it.
	(*mocks)[0].mu.Lock()
	(*mocks)[0].sessionID = "uuid-to-save"
	(*mocks)[0].mu.Unlock()

	mgr.Close()

	// Verify the resume ID was persisted.
	val, err := idx.GetAgentMetadata("test-agent", "cc_session:test-agent/c1")
	if err != nil {
		t.Fatalf("GetAgentMetadata: %v", err)
	}
	if val != "uuid-to-save" {
		t.Errorf("saved resume ID = %q, want %q", val, "uuid-to-save")
	}
}

func TestCount(t *testing.T) {
	// Proves Count returns the correct number of active backends.
	mgr, _ := newTestManager(t, nil)

	if mgr.Count() != 0 {
		t.Errorf("Count = %d, want 0", mgr.Count())
	}

	_, _ = mgr.Get(context.Background(), "test-agent/c1")
	if mgr.Count() != 1 {
		t.Errorf("Count = %d, want 1", mgr.Count())
	}

	_, _ = mgr.Get(context.Background(), "test-agent/c2")
	if mgr.Count() != 2 {
		t.Errorf("Count = %d, want 2", mgr.Count())
	}

	// Getting the same key again should not increase the count.
	_, _ = mgr.Get(context.Background(), "test-agent/c1")
	if mgr.Count() != 2 {
		t.Errorf("Count = %d after duplicate get, want 2", mgr.Count())
	}
}

func TestSessionFilePath(t *testing.T) {
	// Proves that SessionFilePath returns the backend's path when present
	// and empty string when no backend exists.
	mgr, mocks := newTestManager(t, nil)

	// No backend yet.
	if got := mgr.SessionFilePath("test-agent/c1"); got != "" {
		t.Errorf("SessionFilePath = %q, want empty", got)
	}

	_, err := mgr.Get(context.Background(), "test-agent/c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Set a path on the mock.
	(*mocks)[0].mu.Lock()
	(*mocks)[0].sessionFilePath = "/tmp/sessions/abc.jsonl"
	(*mocks)[0].mu.Unlock()

	if got := mgr.SessionFilePath("test-agent/c1"); got != "/tmp/sessions/abc.jsonl" {
		t.Errorf("SessionFilePath = %q, want %q", got, "/tmp/sessions/abc.jsonl")
	}
}

func TestWaitForTurn_Success(t *testing.T) {
	// Proves that WaitForTurn delegates to the backend's WaitForTurn.
	mgr, _ := newTestManager(t, nil)
	sk := "test-agent/c1"

	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if err := mgr.WaitForTurn(context.Background(), sk); err != nil {
		t.Fatalf("WaitForTurn: %v", err)
	}
}

func TestWaitForTurn_NoBackend(t *testing.T) {
	// Proves that WaitForTurn returns an error when no backend exists.
	mgr, _ := newTestManager(t, nil)

	err := mgr.WaitForTurn(context.Background(), "test-agent/none")
	if err == nil {
		t.Fatal("expected error for missing backend")
	}
}

func TestCloseIdle(t *testing.T) {
	// Proves that closeIdle removes backends that have been idle longer than
	// the timeout and saves their resume IDs before closing.
	idx := newTestSessionIndex(t)
	mgr, mocks := newTestManager(t, idx)

	// Create two backends.
	_, err := mgr.Get(context.Background(), "test-agent/c1")
	if err != nil {
		t.Fatalf("Get c1: %v", err)
	}
	_, err = mgr.Get(context.Background(), "test-agent/c2")
	if err != nil {
		t.Fatalf("Get c2: %v", err)
	}

	// Set session IDs for resume persistence.
	(*mocks)[0].mu.Lock()
	(*mocks)[0].sessionID = "uuid-idle-1"
	(*mocks)[0].mu.Unlock()
	(*mocks)[1].mu.Lock()
	(*mocks)[1].sessionID = "uuid-idle-2"
	(*mocks)[1].mu.Unlock()

	// Make c1 idle by backdating its lastActive.
	mgr.mu.Lock()
	if mb, ok := mgr.backends["test-agent/c1"]; ok {
		mb.lastActive = time.Now().Add(-2 * time.Hour)
	}
	mgr.mu.Unlock()

	// closeIdle with 1-hour timeout should reap c1 but keep c2.
	mgr.closeIdle(time.Hour)

	if mgr.Count() != 1 {
		t.Errorf("Count = %d, want 1 after idle reap", mgr.Count())
	}
	if !(*mocks)[0].wasClosed() {
		t.Error("idle backend c1 should have been closed")
	}
	if (*mocks)[1].wasClosed() {
		t.Error("active backend c2 should NOT have been closed")
	}

	// Verify resume ID was saved for the idle backend.
	val, _ := idx.GetAgentMetadata("test-agent", "cc_session:test-agent/c1")
	if val != "uuid-idle-1" {
		t.Errorf("saved resume ID = %q, want %q", val, "uuid-idle-1")
	}
}

func TestGet_TypingFuncRouting(t *testing.T) {
	// Proves that SetTypingFunc is called on the backend with a function that
	// routes typing state through TypingFunc with the correct session key.
	var gotKey string
	var gotTyping bool
	mgr, mocks := newTestManager(t, nil)
	mgr.TypingFunc = func(sk string, typing bool) {
		gotKey = sk
		gotTyping = typing
	}

	_, err := mgr.Get(context.Background(), "test-agent/c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mock := (*mocks)[0]
	mock.mu.Lock()
	tf := mock.typingFunc
	mock.mu.Unlock()
	if tf == nil {
		t.Fatal("typingFunc not set on backend")
	}

	tf(true)
	if gotKey != "test-agent/c1" {
		t.Errorf("TypingFunc sessionKey = %q, want %q", gotKey, "test-agent/c1")
	}
	if !gotTyping {
		t.Error("TypingFunc typing = false, want true")
	}
}

func TestGet_PermissionPromptFuncRouting(t *testing.T) {
	// Proves that SetPermissionPromptFunc is called on the backend, and when
	// invoked it both sets permission pending and calls the manager's func.
	var gotKey, gotReqID, gotText, gotSummary string
	mgr, mocks := newTestManager(t, nil)
	mgr.PermissionPromptFunc = func(sk, reqID, text, summary string, choices []delegator.PromptChoice) {
		gotKey = sk
		gotReqID = reqID
		gotText = text
		gotSummary = summary
	}

	sk := "test-agent/c1"
	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mock := (*mocks)[0]
	mock.mu.Lock()
	ppf := mock.permPromptFunc
	mock.mu.Unlock()
	if ppf == nil {
		t.Fatal("permissionPromptFunc not set on backend")
	}

	ppf("req-1", "Allow edit?", "Edit foo.go", nil)

	if gotKey != sk {
		t.Errorf("sessionKey = %q, want %q", gotKey, sk)
	}
	if gotReqID != "req-1" {
		t.Errorf("requestID = %q, want %q", gotReqID, "req-1")
	}
	if gotText != "Allow edit?" {
		t.Errorf("text = %q, want %q", gotText, "Allow edit?")
	}
	if gotSummary != "Edit foo.go" {
		t.Errorf("summary = %q, want %q", gotSummary, "Edit foo.go")
	}

	// Permission should now be pending.
	if !mgr.IsPermissionPending(sk) {
		t.Error("permission should be pending after prompt")
	}
}

func TestGet_OnPermissionClearedCallback(t *testing.T) {
	// Proves that the OnPermissionCleared callback on the backend correctly
	// clears the permission pending state in the manager.
	mgr, mocks := newTestManager(t, nil)
	sk := "test-agent/c1"

	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Set pending manually.
	mgr.SetPermissionPending(sk, true)
	if !mgr.IsPermissionPending(sk) {
		t.Fatal("should be pending")
	}

	// Fire the callback the backend would call.
	mock := (*mocks)[0]
	mock.mu.Lock()
	opc := mock.onPermCleared
	mock.mu.Unlock()
	if opc == nil {
		t.Fatal("onPermCleared not set on backend")
	}
	opc()

	if mgr.IsPermissionPending(sk) {
		t.Error("permission should be cleared after callback")
	}
}

func TestGet_OnSessionReadyPersistsUUID(t *testing.T) {
	// Proves that the OnSessionReady callback persists the session UUID via
	// the SessionIndex.
	idx := newTestSessionIndex(t)
	mgr, mocks := newTestManager(t, idx)

	_, err := mgr.Get(context.Background(), "test-agent/c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mock := (*mocks)[0]
	mock.mu.Lock()
	osr := mock.onSessionReady
	mock.mu.Unlock()
	if osr == nil {
		t.Fatal("onSessionReady not set on backend")
	}

	osr("new-session-uuid-42")

	val, err := idx.GetAgentMetadata("test-agent", "cc_session:test-agent/c1")
	if err != nil {
		t.Fatalf("GetAgentMetadata: %v", err)
	}
	if val != "new-session-uuid-42" {
		t.Errorf("persisted UUID = %q, want %q", val, "new-session-uuid-42")
	}
}

func TestResetSession_ClearsPermission(t *testing.T) {
	// Proves that ResetSession unblocks any WaitForPermission waiters by
	// clearing the permission state before closing.
	mgr, _ := newTestManager(t, nil)
	sk := "test-agent/c1"

	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mgr.SetPermissionPending(sk, true)

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- mgr.WaitForPermission(context.Background(), sk)
	}()

	// Give the waiter time to block.
	time.Sleep(50 * time.Millisecond)

	mgr.ResetSession(sk)

	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("WaitForPermission after reset: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForPermission did not unblock after ResetSession")
	}
}

func TestClose_ClearsPermissions(t *testing.T) {
	// Proves that Close unblocks any WaitForPermission waiters on all backends.
	mgr, _ := newTestManager(t, nil)
	sk := "test-agent/c1"

	_, err := mgr.Get(context.Background(), sk)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mgr.SetPermissionPending(sk, true)

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- mgr.WaitForPermission(context.Background(), sk)
	}()

	time.Sleep(50 * time.Millisecond)

	mgr.Close()

	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("WaitForPermission after Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForPermission did not unblock after Close")
	}
}

func TestGet_StartOptionsPassthrough(t *testing.T) {
	// Proves that global StartOpts fields (WorkDir, SystemPrompt, Model) are
	// passed through to the backend, while Label and SessionKey are overridden.
	mgr, mocks := newTestManager(t, nil)
	mgr.StartOpts = delegator.StartOptions{
		WorkDir:      "/workspace",
		SystemPrompt: "You are helpful.",
		Model:        "opus",
	}

	_, err := mgr.Get(context.Background(), "agent/c1/v1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	opts := (*mocks)[0].startOpts
	if opts.WorkDir != "/workspace" {
		t.Errorf("WorkDir = %q, want %q", opts.WorkDir, "/workspace")
	}
	if opts.SystemPrompt != "You are helpful." {
		t.Errorf("SystemPrompt = %q, want %q", opts.SystemPrompt, "You are helpful.")
	}
	if opts.Model != "opus" {
		t.Errorf("Model = %q, want %q", opts.Model, "opus")
	}
	if opts.Label != "agent-c1-v1" {
		t.Errorf("Label = %q, want %q", opts.Label, "agent-c1-v1")
	}
	if opts.SessionKey != "agent/c1/v1" {
		t.Errorf("SessionKey = %q, want %q", opts.SessionKey, "agent/c1/v1")
	}
}

func TestLoadResumeID_NilIndex(t *testing.T) {
	// Proves that loadResumeID returns empty string when SessionIndex is nil.
	mgr := &DelegatedManager{}
	if got := mgr.loadResumeID("any/key"); got != "" {
		t.Errorf("loadResumeID = %q, want empty", got)
	}
}

func TestSaveResumeID_NilIndex(t *testing.T) {
	// Proves that saveResumeID is a no-op when SessionIndex is nil (no panic).
	mgr := &DelegatedManager{}
	mgr.saveResumeID("any/key", "some-uuid") // should not panic
}

func TestSaveResumeID_EmptySessionID(t *testing.T) {
	// Proves that saveResumeID is a no-op when sessionID is empty.
	idx := newTestSessionIndex(t)
	mgr := &DelegatedManager{
		SessionIndex: idx,
		AgentID:      "test-agent",
	}
	mgr.saveResumeID("any/key", "") // should not panic or write

	val, _ := idx.GetAgentMetadata("test-agent", "cc_session:any/key")
	if val != "" {
		t.Errorf("expected no value saved, got %q", val)
	}
}

func TestStateKey(t *testing.T) {
	// Proves that stateKey returns the correct prefix format.
	mgr := &DelegatedManager{}
	if got := mgr.stateKey("agent/c1"); got != "cc_session:agent/c1" {
		t.Errorf("stateKey = %q, want %q", got, "cc_session:agent/c1")
	}
}

func TestClearResumeID(t *testing.T) {
	// Proves that clearResumeID removes the stored session UUID so subsequent
	// loads return empty.
	idx := newTestSessionIndex(t)
	mgr := &DelegatedManager{AgentID: "test-agent", SessionIndex: idx}

	base := "test-agent/c1"
	mgr.saveResumeID(base, "some-uuid")
	if got := mgr.loadResumeID(base); got != "some-uuid" {
		t.Fatalf("loadResumeID before clear = %q, want %q", got, "some-uuid")
	}

	mgr.clearResumeID(base)
	if got := mgr.loadResumeID(base); got != "" {
		t.Errorf("loadResumeID after clear = %q, want empty", got)
	}
}

func TestClearResumeID_NilIndex(t *testing.T) {
	// Proves that clearResumeID is a no-op when SessionIndex is nil.
	mgr := &DelegatedManager{AgentID: "test-agent"}
	mgr.clearResumeID("test-agent/c1") // should not panic
}

func TestGet_RetryAfterInitDeath(t *testing.T) {
	// Proves that when a backend starts successfully but dies during init
	// (WaitReady fails + IsRunning returns false) and a resume ID was used,
	// the manager clears the stale resume ID and retries without --resume.
	idx := newTestSessionIndex(t)

	var callCount atomic.Int32
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			n := int(callCount.Add(1))
			be := &mockBackendDM{running: true}
			if n == 1 {
				// First backend: simulate dying during init.
				// WaitReady returns error, IsRunning returns false.
				be.waitReadyErr = context.DeadlineExceeded
				be.running = false // dead after Start returns
			}
			// Second backend: healthy.
			return be, nil
		},
		StartOpts:    delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:      "test-agent",
		SessionIndex: idx,
		IdleTimeout:  time.Hour,
	}
	t.Cleanup(func() { mgr.Close() })

	// Seed a resume ID.
	base := "test-agent/c222"
	stateKey := "cc_session:" + base
	if err := idx.SetAgentMetadata("test-agent", stateKey, "stale-uuid"); err != nil {
		t.Fatalf("SetAgentMetadata: %v", err)
	}

	be, err := mgr.Get(context.Background(), base)
	if err != nil {
		t.Fatalf("Get should succeed after retry: %v", err)
	}
	if be == nil {
		t.Fatal("Get returned nil backend")
	}

	// Should have created 2 backends: first died, second succeeded.
	if got := int(callCount.Load()); got != 2 {
		t.Errorf("expected 2 NewBackend calls, got %d", got)
	}

	// The stale resume ID should have been cleared.
	if got := mgr.loadResumeID(base); got != "" {
		t.Errorf("resume ID should be cleared after init death retry, got %q", got)
	}
}

func TestGet_NoRetryAfterInitDeath_WithoutResumeID(t *testing.T) {
	// Proves that when a backend dies during init but no resume ID was used,
	// no retry is attempted (there's nothing to clear).
	var callCount atomic.Int32
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			callCount.Add(1)
			be := &mockBackendDM{
				running:      false, // dead
				waitReadyErr: context.DeadlineExceeded,
			}
			return be, nil
		},
		StartOpts:   delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:     "test-agent",
		IdleTimeout: time.Hour,
	}
	t.Cleanup(func() { mgr.Close() })

	// No resume ID stored — no retry should happen.
	_, err := mgr.Get(context.Background(), "test-agent/c333")
	if err != nil {
		t.Fatalf("Get should not error (proceeds anyway): %v", err)
	}

	if got := int(callCount.Load()); got != 1 {
		t.Errorf("expected 1 NewBackend call (no retry), got %d", got)
	}
}

func TestSetBackendCallbacks(t *testing.T) {
	// Proves that setBackendCallbacks wires up callback types on a backend.
	var typingKey string
	var typingState bool

	mgr := &DelegatedManager{
		AgentID: "test-agent",
		TypingFunc: func(sk string, typing bool) {
			typingKey = sk
			typingState = typing
		},
	}

	be := &mockBackendDM{running: true}
	mb := &managedBackend{be: be, sessionKey: "test-agent/c1"}
	mgr.setBackendCallbacks(mb)

	be.mu.Lock()
	tf := be.typingFunc
	osr := be.onSessionReady
	opc := be.onPermCleared
	be.mu.Unlock()

	if tf == nil {
		t.Fatal("typingFunc not set")
	}
	tf(true)
	if typingKey != "test-agent/c1" || !typingState {
		t.Errorf("typingFunc routed to key=%q state=%v", typingKey, typingState)
	}

	if osr == nil {
		t.Fatal("onSessionReady not set")
	}

	if opc == nil {
		t.Fatal("onPermCleared not set")
	}
}

func TestSetBackendCallbacks_PermissionCancel(t *testing.T) {
	// Proves that setBackendCallbacks wires the per-perm cancel callback when
	// PermissionCancelFunc is configured, and that it routes the session key
	// + reqID + tool + reason through to the platform-level handler.
	var (
		gotSK, gotReq, gotTool, gotReason string
		called                            int
	)
	mgr := &DelegatedManager{
		AgentID: "test-agent",
		PermissionCancelFunc: func(sk, reqID, tool, reason string) {
			called++
			gotSK = sk
			gotReq = reqID
			gotTool = tool
			gotReason = reason
		},
	}

	be := &mockBackendDM{running: true}
	mb := &managedBackend{be: be, sessionKey: "test-agent/c-cancel"}
	mgr.setBackendCallbacks(mb)

	be.mu.Lock()
	hook := be.onPermCancelled
	be.mu.Unlock()
	if hook == nil {
		t.Fatal("onPermCancelled not wired on backend")
	}

	hook("req-9", "Bash", "tool request cancelled by follow-up message")

	if called != 1 {
		t.Fatalf("PermissionCancelFunc called %d times, want 1", called)
	}
	if gotSK != "test-agent/c-cancel" || gotReq != "req-9" || gotTool != "Bash" || gotReason == "" {
		t.Errorf("got sk=%q reqID=%q tool=%q reason=%q", gotSK, gotReq, gotTool, gotReason)
	}
}

func TestSetBackendCallbacks_NoCancelWhenUnset(t *testing.T) {
	// Proves that when PermissionCancelFunc is nil, no cancel hook is wired.
	// (Symmetric with how PermissionPromptFunc is gated above it.)
	mgr := &DelegatedManager{AgentID: "test-agent"}

	be := &mockBackendDM{running: true}
	mb := &managedBackend{be: be, sessionKey: "test-agent/c"}
	mgr.setBackendCallbacks(mb)

	be.mu.Lock()
	hook := be.onPermCancelled
	be.mu.Unlock()
	if hook != nil {
		t.Error("onPermCancelled should not be wired when PermissionCancelFunc is nil")
	}
}

func TestRotateBackendKey(t *testing.T) {
	idx := newTestSessionIndex(t)
	mgr, mocks := newTestManager(t, idx)

	// Create a backend under the old key.
	be, err := mgr.Get(context.Background(), "old-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if be == nil {
		t.Fatal("Get returned nil")
	}

	// Simulate session-ready to persist a resume ID.
	(*mocks)[0].mu.Lock()
	osr := (*mocks)[0].onSessionReady
	(*mocks)[0].mu.Unlock()
	if osr != nil {
		osr("resume-uuid-123")
	}

	// Rotate the key.
	mgr.RotateBackendKey("old-key", "new-key")

	// Old key should be gone — Get would create a new backend.
	if mgr.Count() != 1 {
		t.Fatalf("expected 1 backend after rotate, got %d", mgr.Count())
	}

	// New key should return the same backend without creating a new one.
	be2, err := mgr.Get(context.Background(), "new-key")
	if err != nil {
		t.Fatalf("Get new-key: %v", err)
	}
	if len(*mocks) != 1 {
		t.Fatalf("expected no new backend creation, got %d mocks", len(*mocks))
	}
	_ = be2

	// Resume ID should be migrated: new key has it, old key doesn't.
	newID := mgr.loadResumeID("new-key")
	oldID := mgr.loadResumeID("old-key")
	if newID != "resume-uuid-123" {
		t.Errorf("resume ID not migrated to new key: got %q", newID)
	}
	if oldID != "" {
		t.Errorf("resume ID not cleared from old key: got %q", oldID)
	}
}

func TestRotateBackendKey_NoOp(t *testing.T) {
	mgr, _ := newTestManager(t, nil)

	// Rotating a key that doesn't exist should be a no-op, not panic.
	mgr.RotateBackendKey("nonexistent", "new-key")

	if mgr.Count() != 0 {
		t.Fatalf("expected 0 backends, got %d", mgr.Count())
	}
}

func TestRotateBackendKey_NilSessionIndex(t *testing.T) {
	mgr, mocks := newTestManager(t, nil) // no session index

	_, err := mgr.Get(context.Background(), "old-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = mocks

	// Should not panic even without a session index.
	mgr.RotateBackendKey("old-key", "new-key")

	if mgr.Count() != 1 {
		t.Fatalf("expected 1 backend after rotate, got %d", mgr.Count())
	}

	// New key should find the existing backend.
	_, err = mgr.Get(context.Background(), "new-key")
	if err != nil {
		t.Fatalf("Get new-key: %v", err)
	}
	if len(*mocks) != 1 {
		t.Fatal("should not have created a new backend")
	}
}

// contains is a test helper that checks if a string contains a substring.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

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

	permPromptFunc   delegator.PermissionPromptFunc
	onPermCleared    func()
	cancelListeners  map[string][]func(reason string)
	onSessionReady   func(string)
	typingFunc       func(bool)
	sessionID        string
	sessionFilePath  string
	waitReadyErr     error
	waitReadyDelay   time.Duration
	turnInFlight     bool
	running          bool
	waitForTurnErr   error
	waitForTurnBlock chan struct{} // if non-nil, WaitForTurn blocks until closed
	sendToPaneFn     func(context.Context, string, *mockHandler) (*delegator.TurnResult, error)
	sessionEvents    *delegator.SessionEvents
	sendCommandFn    func(context.Context, string) error
	closeFn          func() error
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

func (m *mockBackendDM) SendToPane(ctx context.Context, text string, handler *mockHandler) (*delegator.TurnResult, error) {
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

func (m *mockBackendDM) CheckReady(_ context.Context) (bool, error) { return true, nil }

func (m *mockBackendDM) SendCommand(ctx context.Context, cmd string) error {
	if m.sendCommandFn != nil {
		return m.sendCommandFn(ctx, cmd)
	}
	return nil
}

// Inject delegates to the existing SendToPane / SendCommand mocks based on
// inj.Source so existing tests don't need rewriting. Routing mirrors
// production's Inject: SourceUser routes to SendToPane at idle (begin
// turn) or SendCommand mid-turn (follow-up); slash commands route to
// SendCommand directly.
//
// Production callers (turn_delegated.go) pass inj.Turn (TurnEvents) for
// bookkeeping and install delivery via AttachSessionEvents; we recombine the
// two into a mockHandler for the SendToPane test seam so the existing test
// surface (which invokes handler.OnTurnComplete) keeps working without churn.
func (m *mockBackendDM) Inject(ctx context.Context, inj delegator.Inject) error {
	m.mu.Lock()
	se := m.sessionEvents
	m.mu.Unlock()
	handler := &mockHandler{}
	if inj.Turn != nil {
		if handler.OnTurnComplete == nil {
			handler.OnTurnComplete = inj.Turn.OnTurnComplete
		}
		if handler.PostToolNudgeFunc == nil {
			handler.PostToolNudgeFunc = inj.Turn.PostToolNudgeFunc
		}
		if handler.PreAnswerNudgeFunc == nil {
			handler.PreAnswerNudgeFunc = inj.Turn.PreAnswerNudgeFunc
		}
	}
	if se != nil {
		if handler.OnText == nil {
			handler.OnText = se.OnText
		}
		if handler.OnTextDelta == nil {
			handler.OnTextDelta = se.OnTextDelta
		}
		if handler.OnThinkingDelta == nil {
			handler.OnThinkingDelta = se.OnThinkingDelta
		}
		if handler.OnToolStart == nil {
			handler.OnToolStart = se.OnToolStart
		}
		if handler.OnToolEnd == nil {
			handler.OnToolEnd = se.OnToolEnd
		}
	}
	switch inj.Source {
	case delegator.SourceUser, delegator.SourceSteer:
		if !m.IsTurnInFlight() {
			_, err := m.SendToPane(ctx, inj.Text, handler)
			return err
		}
		return m.SendCommand(ctx, inj.Text)
	case delegator.SourceCompact, delegator.SourcePass:
		return m.SendCommand(ctx, inj.Text)
	}
	return nil
}

func (m *mockBackendDM) IsRunning() bool { return m.running }

func (m *mockBackendDM) SetPermissionPromptFunc(fn delegator.PermissionPromptFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.permPromptFunc = fn
}

func (m *mockBackendDM) SetOnPromptsCleared(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onPermCleared = fn
}

func (m *mockBackendDM) RegisterPromptCancelListener(requestID string, fn func(reason string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancelListeners == nil {
		m.cancelListeners = make(map[string][]func(reason string))
	}
	m.cancelListeners[requestID] = append(m.cancelListeners[requestID], fn)
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

func (m *mockBackendDM) AttachSessionEvents(events *delegator.SessionEvents) {
	m.mu.Lock()
	m.sessionEvents = events
	m.mu.Unlock()
}

func (m *mockBackendDM) SendKeystroke(_ context.Context, _ string) error  { return nil }
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

func TestGet_ConcurrentSameKeySpawnsOne(t *testing.T) {
	// Proves that many concurrent Get calls for the same session key spawn
	// exactly one backend (not one per caller) and all callers receive the same
	// instance — the singleflight serialization of creation. Run with -race to
	// also catch the unsynchronized creation the old code allowed. (P2-3.)
	var created int32
	var mu sync.Mutex
	var mocks []*mockBackendDM
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			atomic.AddInt32(&created, 1)
			// Widen the creation window so concurrent callers reliably overlap
			// between the lookup-miss and the insert (the racy gap).
			time.Sleep(20 * time.Millisecond)
			be := &mockBackendDM{running: true}
			mu.Lock()
			mocks = append(mocks, be)
			mu.Unlock()
			return be, nil
		},
		StartOpts:   delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:     "test-agent",
		IdleTimeout: time.Hour,
	}
	t.Cleanup(func() { mgr.Close() })

	const N = 24
	var wg sync.WaitGroup
	backends := make([]delegator.Delegator, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			backends[i], errs[i] = mgr.Get(context.Background(), "test-agent/c1")
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("Get[%d]: %v", i, e)
		}
	}
	if got := atomic.LoadInt32(&created); got != 1 {
		t.Errorf("NewBackend called %d times, want exactly 1", got)
	}
	for i := 1; i < N; i++ {
		if backends[i] != backends[0] {
			t.Errorf("Get[%d] returned a different backend instance", i)
		}
	}
}

func TestGet_LazyCreation(t *testing.T) {
	// Proves that Get creates a new backend on first call and returns the same
	// backend on subsequent calls with the same session key base.
	mgr, mocks := newTestManager(t, nil)

	be1, err := mgr.Get(context.Background(), "test-agent/c123")
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
	be2, err := mgr.Get(context.Background(), "test-agent/c123")
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
	be3, err := mgr.Get(context.Background(), "test-agent/c999")
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
	if err := idx.SetSessionMetadata(base, "cc_resume_id", "saved-uuid-123"); err != nil {
		t.Fatalf("SetSessionMetadata: %v", err)
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

func TestGet_EffortFuncResolvedPerSession(t *testing.T) {
	// Proves the cold-launch effort plumbing (#840): EffortFunc is invoked with
	// the session key at each Start and its result populates opts.Effort, so a
	// relaunch re-establishes the level (apply_flag_settings is runtime-only).
	// Per-session — each key resolves its own effort.
	mgr, mocks := newTestManager(t, nil)
	mgr.StartOpts.EffortFunc = func(sk string) string {
		switch sk {
		case "test-agent/c1":
			return "max"
		case "test-agent/c2":
			return "" // no override → empty, backend omits --effort
		default:
			return "high"
		}
	}

	if _, err := mgr.Get(context.Background(), "test-agent/c1"); err != nil {
		t.Fatalf("Get c1: %v", err)
	}
	if _, err := mgr.Get(context.Background(), "test-agent/c2"); err != nil {
		t.Fatalf("Get c2: %v", err)
	}

	if got := (*mocks)[0].startOpts.Effort; got != "max" {
		t.Errorf("c1 effort = %q, want %q", got, "max")
	}
	if got := (*mocks)[1].startOpts.Effort; got != "" {
		t.Errorf("c2 effort = %q, want empty", got)
	}
}

func TestGet_ResumeFailsFallsBackToFresh(t *testing.T) {
	// Proves that when Start with a resume ID fails, the manager retries
	// without the resume ID and succeeds.
	idx := newTestSessionIndex(t)

	var callCount int
	var noticeKey, noticeText string
	var noticeCalls int
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
		SystemNoticeFunc: func(sessionKey, text string) {
			noticeCalls++
			noticeKey, noticeText = sessionKey, text
		},
	}
	t.Cleanup(func() { mgr.Close() })

	base := "test-agent/c111"
	if err := idx.SetSessionMetadata(base, "cc_resume_id", "stale-uuid"); err != nil {
		t.Fatalf("SetSessionMetadata: %v", err)
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
	// The user must be told their old session couldn't be resumed.
	if noticeCalls != 1 {
		t.Errorf("expected exactly 1 resume-missed notice, got %d", noticeCalls)
	}
	if noticeKey != base {
		t.Errorf("notice sessionKey = %q, want %q", noticeKey, base)
	}
	if !contains(noticeText, "stale-uuid") {
		t.Errorf("notice should mention the missing resume id, got: %q", noticeText)
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

	_, err := mgr.Get(context.Background(), "myagent/c42")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mock := (*mocks)[0]
	want := "myagent-c42"
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
	if err := idx.SetSessionMetadata(sk, "cc_resume_id", "some-uuid"); err != nil {
		t.Fatalf("SetSessionMetadata: %v", err)
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
	val, _ := idx.GetSessionMetadata(sk, "cc_resume_id")
	if val != "" {
		t.Errorf("resume ID should be cleared, got %q", val)
	}
}

func TestResetSession_NoBackend(t *testing.T) {
	// Proves that ResetSession is a no-op when no backend exists (no panic).
	mgr, _ := newTestManager(t, nil)
	mgr.ResetSession("test-agent/nonexistent") // should not panic
}

func TestBounceSession_ClosesButKeepsResumeID(t *testing.T) {
	// Proves that BounceSession (#828 Part B) closes the backend and unmaps it
	// BUT keeps the saved resume ID — so the next Get respawns CC with --resume
	// <same session>, picking up the now-compacted conversation while Part A's
	// SystemPromptFunc rebuilds the prompt from disk. The opposite of
	// ResetSession, which clears the resume ID for a genuinely fresh session.
	idx := newTestSessionIndex(t)
	mgr, mocks := newTestManager(t, idx)

	sk := "test-agent/c1"
	if err := idx.SetSessionMetadata(sk, "cc_resume_id", "keep-this-uuid"); err != nil {
		t.Fatalf("SetSessionMetadata: %v", err)
	}

	if _, err := mgr.Get(context.Background(), sk); err != nil {
		t.Fatalf("Get: %v", err)
	}
	firstMock := (*mocks)[0]

	mgr.BounceSession(sk)

	if !firstMock.wasClosed() {
		t.Error("expected backend to be closed by bounce")
	}
	if mgr.Count() != 0 {
		t.Errorf("Count = %d, want 0 after bounce", mgr.Count())
	}
	// Resume ID must be PRESERVED — the key difference from ResetSession.
	if val, _ := idx.GetSessionMetadata(sk, "cc_resume_id"); val != "keep-this-uuid" {
		t.Errorf("resume ID = %q, want %q preserved across bounce", val, "keep-this-uuid")
	}
	// And the next Get must respawn resuming that same session.
	if _, err := mgr.Get(context.Background(), sk); err != nil {
		t.Fatalf("Get after bounce: %v", err)
	}
	if got := (*mocks)[1].startOpts.ResumeSessionID; got != "keep-this-uuid" {
		t.Errorf("post-bounce ResumeSessionID = %q, want %q (respawn must resume the kept session)", got, "keep-this-uuid")
	}
}

func TestBounceSession_NoBackend(t *testing.T) {
	// Proves that BounceSession is a no-op when no backend exists (no panic).
	mgr, _ := newTestManager(t, nil)
	mgr.BounceSession("test-agent/nonexistent") // should not panic
}

func TestResetSession_DoesNotHoldManagerLockDuringClose(t *testing.T) {
	// Regression for the 2026-05-06 deadlock. ResetSession used to hold m.mu
	// across be.Close(), so a single stuck backend froze the entire agent —
	// every subsequent inbound message blocked on m.mu trying to look up its
	// session. This test installs a backend whose Close blocks, kicks off
	// ResetSession in a goroutine, and verifies that an unrelated session's
	// Get still completes promptly.
	t.Parallel()

	mgr, _ := newTestManager(t, nil)

	stuck := "test-agent/stuck"
	other := "test-agent/other"

	// Spawn the stuck backend.
	if _, err := mgr.Get(context.Background(), stuck); err != nil {
		t.Fatalf("Get(stuck): %v", err)
	}

	// Wire its Close to block until we let it through.
	release := make(chan struct{})
	mgr.mu.Lock()
	stuckMB := mgr.backends[stuck]
	mgr.mu.Unlock()
	stuckMock := stuckMB.be.(*mockBackendDM)
	stuckMock.mu.Lock()
	stuckMock.closeFn = func() error { <-release; return nil }
	stuckMock.mu.Unlock()

	// Reset in the background — Close will block.
	resetDone := make(chan struct{})
	go func() {
		mgr.ResetSession(stuck)
		close(resetDone)
	}()

	// Give ResetSession time to enter Close.
	time.Sleep(20 * time.Millisecond)

	// An unrelated session should still be reachable. If m.mu was held
	// during Close, this would block.
	getDone := make(chan error, 1)
	go func() {
		_, err := mgr.Get(context.Background(), other)
		getDone <- err
	}()

	select {
	case err := <-getDone:
		if err != nil {
			t.Fatalf("Get(other) while stuck close in progress: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Get(other) blocked while ResetSession's Close was in progress — m.mu is being held across Close")
	}

	// Let the stuck Close finish so the test goroutine cleans up.
	close(release)
	<-resetDone
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
	val, err := idx.GetSessionMetadata("test-agent/c1", "cc_resume_id")
	if err != nil {
		t.Fatalf("GetSessionMetadata: %v", err)
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
	val, _ := idx.GetSessionMetadata("test-agent/c1", "cc_resume_id")
	if val != "uuid-idle-1" {
		t.Errorf("saved resume ID = %q, want %q", val, "uuid-idle-1")
	}
}

func TestCloseIdle_DoesNotHoldManagerLockDuringClose(t *testing.T) {
	// Regression for TODO #749 root cause. closeIdle used to hold m.mu across
	// be.Close(). The ccstream waiter goroutine, on its way to send waitCh,
	// passes through the bounded typingFunc wrapper at SetTypingFunc — which
	// calls sk() to read mb.sessionKey. sk() takes m.mu. With m.mu held by
	// closeIdle, the waiter blocked, the 2s typingFunc timer never armed
	// (sk() ran synchronously before the timer), and Close took the full
	// 5s+2s bounded-shutdown fallback. Mirrors
	// TestResetSession_DoesNotHoldManagerLockDuringClose.
	t.Parallel()

	mgr, _ := newTestManager(t, nil)

	stuck := "test-agent/stuck"
	other := "test-agent/other"

	// Spawn the stuck backend.
	if _, err := mgr.Get(context.Background(), stuck); err != nil {
		t.Fatalf("Get(stuck): %v", err)
	}

	// Wire its Close to block until we let it through, and backdate
	// lastActive so closeIdle picks it up.
	release := make(chan struct{})
	mgr.mu.Lock()
	stuckMB := mgr.backends[stuck]
	stuckMB.lastActive = time.Now().Add(-2 * time.Hour)
	mgr.mu.Unlock()
	stuckMock := stuckMB.be.(*mockBackendDM)
	stuckMock.mu.Lock()
	stuckMock.closeFn = func() error { <-release; return nil }
	stuckMock.mu.Unlock()

	// Run closeIdle in the background — Close on the stuck backend will block.
	closeDone := make(chan struct{})
	go func() {
		mgr.closeIdle(time.Hour)
		close(closeDone)
	}()

	// Give closeIdle time to enter the stuck Close.
	time.Sleep(20 * time.Millisecond)

	// An unrelated session should still be reachable. If m.mu was held
	// during Close, this would block.
	getDone := make(chan error, 1)
	go func() {
		_, err := mgr.Get(context.Background(), other)
		getDone <- err
	}()

	select {
	case err := <-getDone:
		if err != nil {
			t.Fatalf("Get(other) while stuck close in progress: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Get(other) blocked while closeIdle's Close was in progress — m.mu is being held across Close")
	}

	// Let the stuck Close finish so the test goroutine cleans up.
	close(release)
	<-closeDone
}

func TestClose_DoesNotHoldManagerLockDuringBackendClose(t *testing.T) {
	// Regression for TODO #749 root cause. Manager Close used to hold m.mu
	// across be.Close(). Same deadlock as closeIdle — at foci shutdown the
	// waiter goroutine would block on m.mu via the bounded typingFunc
	// wrapper's sk() call. This test installs a backend whose Close blocks,
	// fires manager Close in a goroutine, and proves that taking m.mu
	// from another goroutine succeeds promptly (the lock is no longer held
	// during the slow Close).
	t.Parallel()

	mgr, _ := newTestManager(t, nil)

	stuck := "test-agent/stuck"
	if _, err := mgr.Get(context.Background(), stuck); err != nil {
		t.Fatalf("Get(stuck): %v", err)
	}

	release := make(chan struct{})
	mgr.mu.Lock()
	stuckMB := mgr.backends[stuck]
	mgr.mu.Unlock()
	stuckMock := stuckMB.be.(*mockBackendDM)
	stuckMock.mu.Lock()
	stuckMock.closeFn = func() error { <-release; return nil }
	stuckMock.mu.Unlock()

	closeDone := make(chan struct{})
	go func() {
		mgr.Close()
		close(closeDone)
	}()

	// Give Close time to enter the stuck backend's Close.
	time.Sleep(20 * time.Millisecond)

	// m.mu must be acquirable while the slow Close runs.
	lockAcquired := make(chan struct{})
	go func() {
		mgr.mu.Lock()
		_ = len(mgr.backends) // touch mu-guarded state to prove the lock is free
		mgr.mu.Unlock()
		close(lockAcquired)
	}()

	select {
	case <-lockAcquired:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("m.mu still held while manager Close's slow be.Close was in progress")
	}

	close(release)
	<-closeDone
}

func TestGet_TypingFuncBoundedWhenManagerLockHeld(t *testing.T) {
	// Defense-in-depth regression for TODO #749. Even if a future caller
	// regresses and holds m.mu across be.Close (the original bug), the
	// typingFunc wrapper must still return within typingFuncTimeout —
	// because sk() now runs inside the inner goroutine, so the outer
	// select's 2s timer fires regardless of what the inner goroutine
	// is blocked on.
	mgr, mocks := newTestManager(t, nil)
	mgr.TypingFunc = func(sk string, typing bool) {}

	if _, err := mgr.Get(context.Background(), "test-agent/c1"); err != nil {
		t.Fatalf("Get: %v", err)
	}

	mock := (*mocks)[0]
	mock.mu.Lock()
	tf := mock.typingFunc
	mock.mu.Unlock()
	if tf == nil {
		t.Fatal("typingFunc not set on backend")
	}

	// Hold m.mu — sk() inside the wrapper's inner goroutine will block on this.
	mgr.mu.Lock()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		tf(false) // must return within typingFuncTimeout despite m.mu being held
		close(done)
	}()

	select {
	case <-done:
		elapsed := time.Since(start)
		if elapsed > typingFuncTimeout+500*time.Millisecond {
			t.Errorf("tf returned in %s — slower than typingFuncTimeout (%s) + slack", elapsed, typingFuncTimeout)
		}
		if elapsed < typingFuncTimeout-200*time.Millisecond {
			t.Errorf("tf returned in %s — faster than typingFuncTimeout (%s); did the 2s bound actually fire?", elapsed, typingFuncTimeout)
		}
	case <-time.After(typingFuncTimeout + 2*time.Second):
		mgr.mu.Unlock()
		t.Fatalf("tf did not return within %s — sk() is still running outside the inner goroutine and blocking the wrapper", typingFuncTimeout+2*time.Second)
	}

	mgr.mu.Unlock()
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

func TestGet_TypingFuncBoundedWhenDownstreamHangs(t *testing.T) {
	// Proves that when the manager's TypingFunc hangs (e.g. Telegram
	// SetChatTyping waiting on a stale connection), the wrapper returned by
	// SetTypingFunc returns within typingFuncTimeout instead of blocking
	// indefinitely. This is the defense-in-depth fix for TODO #749 — a
	// fire-and-forget call should never wedge its caller.
	mgr, mocks := newTestManager(t, nil)
	hangCh := make(chan struct{})
	defer close(hangCh)
	mgr.TypingFunc = func(sk string, typing bool) {
		<-hangCh // block until test cleanup
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

	// Override the timeout so the test doesn't take 2 seconds. We can't change
	// the const, but we can verify the wrapper returns within a generous bound
	// shorter than the no-timeout case (which would block forever).
	start := time.Now()
	done := make(chan struct{})
	go func() {
		tf(false) // should return after ~typingFuncTimeout, not block forever
		close(done)
	}()
	select {
	case <-done:
		elapsed := time.Since(start)
		// Allow some slack: must complete close to the configured timeout, not
		// hang forever and not return instantly (which would mean we bypassed
		// the downstream call entirely).
		if elapsed < 100*time.Millisecond {
			t.Errorf("tf returned in %s — too fast, downstream should have been invoked", elapsed)
		}
		if elapsed > typingFuncTimeout+500*time.Millisecond {
			t.Errorf("tf returned in %s — slower than typingFuncTimeout (%s) + slack", elapsed, typingFuncTimeout)
		}
	case <-time.After(typingFuncTimeout + 2*time.Second):
		t.Fatalf("tf did not return within %s — bounded-timeout fix not working", typingFuncTimeout+2*time.Second)
	}
}

func TestGet_TypingFuncReturnsImmediatelyWhenFast(t *testing.T) {
	// Proves that when the manager's TypingFunc returns promptly, the wrapper
	// returns shortly after — no unnecessary delay introduced by the
	// bounded-timeout machinery.
	mgr, mocks := newTestManager(t, nil)
	mgr.TypingFunc = func(sk string, typing bool) {
		// Return immediately.
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

	start := time.Now()
	tf(true)
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("tf with fast downstream took %s — wrapper should be near-zero overhead", elapsed)
	}
}

func TestGet_PermissionPromptFuncRouting(t *testing.T) {
	// Proves that SetPermissionPromptFunc is called on the backend, and when
	// invoked it both sets permission pending and calls the manager's func.
	var gotKey, gotReqID, gotText, gotSummary, gotAttachment string
	mgr, mocks := newTestManager(t, nil)
	mgr.PermissionPromptFunc = func(sk, reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		gotKey = sk
		gotReqID = reqID
		gotText = text
		gotSummary = summary
		gotAttachment = attachmentPath
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

	ppf("req-1", "Allow edit?", "Edit foo.go", "plan.md", nil)

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
	if gotAttachment != "plan.md" {
		t.Errorf("attachmentPath = %q, want %q", gotAttachment, "plan.md")
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

	val, err := idx.GetSessionMetadata("test-agent/c1", "cc_resume_id")
	if err != nil {
		t.Fatalf("GetSessionMetadata: %v", err)
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

	_, err := mgr.Get(context.Background(), "agent/c1")
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
	if opts.Label != "agent-c1" {
		t.Errorf("Label = %q, want %q", opts.Label, "agent-c1")
	}
	if opts.SessionKey != "agent/c1" {
		t.Errorf("SessionKey = %q, want %q", opts.SessionKey, "agent/c1")
	}
}

func TestGet_SystemPromptFuncOverridesStatic(t *testing.T) {
	// Part A (#828/#706): a non-nil SystemPromptFunc returning a non-empty
	// string takes precedence over the static SystemPrompt at session-start,
	// so a fresh session picks up character-file edits made after agent setup.
	mgr, mocks := newTestManager(t, nil)
	calls := 0
	mgr.StartOpts = delegator.StartOptions{
		WorkDir:      "/workspace",
		SystemPrompt: "STALE prompt frozen at setup",
		SystemPromptFunc: func() string {
			calls++
			return "FRESH prompt rebuilt from disk"
		},
	}

	_, err := mgr.Get(context.Background(), "agent/c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if calls != 1 {
		t.Errorf("SystemPromptFunc called %d times, want 1 (once per session-start)", calls)
	}
	if got := (*mocks)[0].startOpts.SystemPrompt; got != "FRESH prompt rebuilt from disk" {
		t.Errorf("SystemPrompt = %q, want the rebuilt prompt (func should win over static)", got)
	}
}

func TestGet_SystemPromptFuncEmptyFallsBackToStatic(t *testing.T) {
	// If SystemPromptFunc yields empty (e.g. a transient disk read failure),
	// the static SystemPrompt is preserved rather than shipping an empty prompt.
	mgr, mocks := newTestManager(t, nil)
	mgr.StartOpts = delegator.StartOptions{
		WorkDir:          "/workspace",
		SystemPrompt:     "STATIC fallback prompt",
		SystemPromptFunc: func() string { return "" },
	}

	_, err := mgr.Get(context.Background(), "agent/c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := (*mocks)[0].startOpts.SystemPrompt; got != "STATIC fallback prompt" {
		t.Errorf("SystemPrompt = %q, want the static fallback (empty func result must not clobber)", got)
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

func TestRemapSession_MovesBackendAndResumeID(t *testing.T) {
	// Proves that RemapSession moves both the live backend map entry and the
	// persisted cc_resume_id row from oldKey to newKey, leaving the old key
	// clean — the primitive the reflection-branch handoff is built on.
	idx := newTestSessionIndex(t)
	mgr, _ := newTestManager(t, idx)

	oldKey := "test-agent/c1"
	newKey := "test-agent/c1/b1700000000"

	be, err := mgr.Get(context.Background(), oldKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	mgr.saveResumeID(oldKey, "resume-uuid-123")

	mgr.RemapSession(oldKey, newKey)

	if _, ok := mgr.getManaged(oldKey); ok {
		t.Error("backend still mapped under old key after remap")
	}
	mb, ok := mgr.getManaged(newKey)
	if !ok {
		t.Fatal("backend not mapped under new key after remap")
	}
	if mb.be != be {
		t.Error("remapped entry is not the same backend instance")
	}
	if mb.sessionKey != newKey {
		t.Errorf("managed sessionKey = %q, want %q (reply routing must follow the new key)", mb.sessionKey, newKey)
	}

	if got := mgr.loadResumeID(newKey); got != "resume-uuid-123" {
		t.Errorf("resume ID under new key = %q, want resume-uuid-123", got)
	}
	if got := mgr.loadResumeID(oldKey); got != "" {
		t.Errorf("resume ID under old key = %q, want empty after remap", got)
	}
}

func TestRemapSession_NoOpGuards(t *testing.T) {
	// Proves that RemapSession does nothing when oldKey == newKey or newKey is
	// empty — the backend stays where it is and the resume ID row is untouched.
	idx := newTestSessionIndex(t)
	mgr, _ := newTestManager(t, idx)

	key := "test-agent/c1"
	if _, err := mgr.Get(context.Background(), key); err != nil {
		t.Fatalf("Get: %v", err)
	}
	mgr.saveResumeID(key, "resume-uuid-123")

	mgr.RemapSession(key, key)
	mgr.RemapSession(key, "")

	if _, ok := mgr.getManaged(key); !ok {
		t.Error("backend lost after no-op remaps")
	}
	if got := mgr.loadResumeID(key); got != "resume-uuid-123" {
		t.Errorf("resume ID = %q, want resume-uuid-123 after no-op remaps", got)
	}
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
	if err := idx.SetSessionMetadata(base, "cc_resume_id", "stale-uuid"); err != nil {
		t.Fatalf("SetSessionMetadata: %v", err)
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

// TestRegisterPromptCancelListener_Routing proves that
// DelegatedManager.RegisterPromptCancelListener routes through to the
// session's backend so per-prompt cancel listeners installed by the platform
// layer reach the right ccstream backend instance. This replaces the
// pre-Phase-2 PermissionCancelFunc global-callback chain.
func TestRegisterPromptCancelListener_Routing(t *testing.T) {
	idx := newTestSessionIndex(t)
	mgr, mocks := newTestManager(t, idx)

	if _, err := mgr.Get(context.Background(), "sk-cancel"); err != nil {
		t.Fatalf("Get: %v", err)
	}

	called := 0
	var gotReason string
	mgr.RegisterPromptCancelListener("sk-cancel", "req-9", func(reason string) {
		called++
		gotReason = reason
	})

	// Inspect the mock backend's recorded listener and fire it.
	be := (*mocks)[0]
	be.mu.Lock()
	listeners := be.cancelListeners["req-9"]
	be.mu.Unlock()
	if len(listeners) != 1 {
		t.Fatalf("listener count for req-9 = %d, want 1", len(listeners))
	}
	listeners[0]("tool request cancelled by follow-up message")

	if called != 1 {
		t.Errorf("listener called %d times, want 1", called)
	}
	if gotReason == "" {
		t.Error("reason not propagated to listener")
	}
}

// TestRegisterPromptCancelListener_UnknownSession proves that registering a
// listener for a session that has no managed backend is a silent no-op.
func TestRegisterPromptCancelListener_UnknownSession(t *testing.T) {
	idx := newTestSessionIndex(t)
	mgr, _ := newTestManager(t, idx)

	// No panic, no error — just no-op.
	mgr.RegisterPromptCancelListener("nonexistent-sk", "req-x", func(string) {
		t.Error("listener should not fire for unknown session")
	})
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

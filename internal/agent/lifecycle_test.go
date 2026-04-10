package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestReloadSystem(t *testing.T) {
	// Proves that ReloadSystem reloads the bootstrap from disk, calls
	// NudgeReloadFunc, invalidates system caches, and — when ReloadSystemFn
	// is set — replaces ExtraSystemBlocks and returns the count from the callback.
	t.Run("with_reload_fn", func(t *testing.T) {
		// When ReloadSystemFn is set, ReloadSystem should call it, set
		// ExtraSystemBlocks, and return the count.
		bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
		ag := &Agent{Bootstrap: bootstrap}

		var nudgeReloaded bool
		ag.NudgeReloadFunc = func() { nudgeReloaded = true }

		newBlocks := []provider.SystemBlock{{Text: "skill-1"}, {Text: "skill-2"}}
		ag.ReloadSystemFn = func() ([]provider.SystemBlock, int) {
			return newBlocks, 2
		}

		// Prime a session meta entry so we can verify cache invalidation.
		sm := ag.getSessionMeta("test/session/1")
		sm.systemBlocks = []provider.SystemBlock{{Text: "stale"}}

		count := ag.ReloadSystem()
		if count != 2 {
			t.Errorf("ReloadSystem() = %d, want 2", count)
		}
		if len(ag.ExtraSystemBlocks) != 2 {
			t.Errorf("ExtraSystemBlocks len = %d, want 2", len(ag.ExtraSystemBlocks))
		}
		if !nudgeReloaded {
			t.Error("NudgeReloadFunc was not called")
		}
		// System cache should be invalidated.
		if sm.systemBlocks != nil {
			t.Error("system cache should be nil after ReloadSystem")
		}
	})

	t.Run("without_reload_fn", func(t *testing.T) {
		// When ReloadSystemFn is nil, ReloadSystem should still reload the
		// bootstrap and nudge rules, but return 0 and leave ExtraSystemBlocks alone.
		bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
		ag := &Agent{
			Bootstrap:         bootstrap,
			ExtraSystemBlocks: []provider.SystemBlock{{Text: "existing"}},
		}

		var nudgeReloaded bool
		ag.NudgeReloadFunc = func() { nudgeReloaded = true }

		count := ag.ReloadSystem()
		if count != 0 {
			t.Errorf("ReloadSystem() = %d, want 0", count)
		}
		if !nudgeReloaded {
			t.Error("NudgeReloadFunc was not called")
		}
		// ExtraSystemBlocks unchanged when no reload fn.
		if len(ag.ExtraSystemBlocks) != 1 {
			t.Errorf("ExtraSystemBlocks len = %d, want 1 (unchanged)", len(ag.ExtraSystemBlocks))
		}
	})

	t.Run("nil_nudge_func", func(t *testing.T) {
		// When NudgeReloadFunc is nil, ReloadSystem should not panic.
		bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
		ag := &Agent{Bootstrap: bootstrap}

		// Should not panic.
		count := ag.ReloadSystem()
		if count != 0 {
			t.Errorf("ReloadSystem() = %d, want 0", count)
		}
	})
}

func TestResetSession_APIPath(t *testing.T) {
	// Proves that ResetSession for a traditional (non-delegated) agent fires
	// memory formation, rotates the session key, reloads the bootstrap, and
	// returns the new key.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/reset/1000000000"

	// Seed the session with enough messages so RotateKey has something to archive.
	if err := store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("hello")}); err != nil {
		t.Fatal(err)
	}
	if err := store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("hi")}); err != nil {
		t.Fatal(err)
	}

	ag := &Agent{
		Sessions:  store,
		Bootstrap: bootstrap,
		// MemoryFormationConfig.SessionEndEnabled defaults to false, so
		// FireSessionEndMemory is a no-op. This is fine — we're testing
		// that the method is called without crashing.
	}

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	var rotatedOld, rotatedNew string
	ag.SessionKeyRotatedFunc.Add(func(old, new string) {
		rotatedOld = old
		rotatedNew = new
	})

	var orientCalled bool
	ag.ResetOrientTemplateFn = func() string {
		orientCalled = true
		return "test orientation"
	}

	newKey, err := ag.ResetSession(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	if newKey == "" {
		t.Fatal("ResetSession returned empty new key")
	}
	if newKey == sessionKey {
		t.Error("ResetSession should return a different key after rotation")
	}
	if rotatedOld != sessionKey || rotatedNew != newKey {
		t.Errorf("rotation callback: old=%q new=%q, want %q/%q", rotatedOld, rotatedNew, sessionKey, newKey)
	}
	if !nudgeReloaded {
		t.Error("NudgeReloadFunc was not called after reset")
	}
	if !orientCalled {
		t.Error("ResetOrientTemplateFn was not called")
	}
}

func TestResetSession_ErrorWhenProcessing(t *testing.T) {
	// Proves that ResetSession returns an error when the agent is currently
	// processing a message, so the user must stop the turn first.
	ag := &Agent{}
	ag.SetProcessingForTest(1)

	_, err := ag.ResetSession(context.Background(), "test/busy/1")
	if err == nil {
		t.Fatal("expected error when processing")
	}
	if !strings.Contains(err.Error(), "processing") {
		t.Errorf("error = %q, want to contain 'processing'", err.Error())
	}
}

func TestCompactSession_HappyPath(t *testing.T) {
	// Proves the full manual compaction lifecycle: CompactSession calls doCompact,
	// rotates the session, fires hooks, and reloads the bootstrap afterward.
	var turnCount atomic.Int32
	client := compactionTestClient(&turnCount, -1) // no high-token turn

	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	compactor := compaction.NewCompactor(store, 0.8)

	sessionKey := "test/compact/1000000000"

	// Seed the session with 6 messages (3 turns) so we pass the >=5 check.
	for i := 0; i < 3; i++ {
		if err := store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent(fmt.Sprintf("msg %d", i))}); err != nil {
			t.Fatal(err)
		}
		if err := store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent(fmt.Sprintf("reply %d", i))}); err != nil {
			t.Fatal(err)
		}
	}

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Compactor: compactor,
		Model:     "claude-haiku-4-5",
	}

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	var notifyMsgs []string
	ag.CompactionNotifyFunc.Add(func(sk, msg string) {
		notifyMsgs = append(notifyMsgs, msg)
	})

	result, err := ag.CompactSession(context.Background(), sessionKey, false)
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if result.OldMessageCount != 6 {
		t.Errorf("OldMessageCount = %d, want 6", result.OldMessageCount)
	}
	if result.NewSessionKey == "" {
		t.Error("expected NewSessionKey after compaction")
	}
	if !nudgeReloaded {
		t.Error("NudgeReloadFunc was not called after compaction")
	}
	if len(notifyMsgs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifyMsgs))
	}
	if !strings.Contains(notifyMsgs[0], "6 messages") {
		t.Errorf("notification = %q, want to mention '6 messages'", notifyMsgs[0])
	}
}

func TestCompactSession_DryRun(t *testing.T) {
	// Proves that dry-run mode runs compaction but does NOT reload the
	// bootstrap, does NOT rotate the session, and returns an empty NewSessionKey.
	var turnCount atomic.Int32
	client := compactionTestClient(&turnCount, -1)

	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	compactor := compaction.NewCompactor(store, 0.8)

	sessionKey := "test/dryrun/1000000000"

	// Seed 6 messages.
	for i := 0; i < 3; i++ {
		if err := store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent(fmt.Sprintf("msg %d", i))}); err != nil {
			t.Fatal(err)
		}
		if err := store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent(fmt.Sprintf("reply %d", i))}); err != nil {
			t.Fatal(err)
		}
	}

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Compactor: compactor,
		Model:     "claude-haiku-4-5",
	}

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	var debugMsgs []string
	ag.CompactionDebugFunc.Add(func(sk, summary string) {
		debugMsgs = append(debugMsgs, summary)
	})

	result, err := ag.CompactSession(context.Background(), sessionKey, true)
	if err != nil {
		t.Fatalf("CompactSession dry-run: %v", err)
	}
	if result.NewSessionKey != "" {
		t.Errorf("NewSessionKey = %q, want empty on dry-run", result.NewSessionKey)
	}
	if nudgeReloaded {
		t.Error("NudgeReloadFunc should NOT be called on dry-run")
	}
	// Summary should have been passed to the debug hook.
	if len(debugMsgs) == 0 {
		t.Error("expected CompactionDebugFunc to receive summary on dry-run")
	}
	// Original session should be untouched.
	mc, _ := store.MessageCount(sessionKey)
	if mc != 6 {
		t.Errorf("session should still have 6 messages after dry-run, got %d", mc)
	}
}

func TestCompactSession_ErrorWhenCompactorNil(t *testing.T) {
	// Proves that CompactSession returns a clear error when the Compactor
	// is nil (compaction not configured).
	ag := &Agent{}

	_, err := ag.CompactSession(context.Background(), "test/s/1", false)
	if err == nil {
		t.Fatal("expected error when Compactor is nil")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error = %q, want to contain 'not configured'", err.Error())
	}
}

func TestCompactSession_ErrorWhenNoActiveSession(t *testing.T) {
	// Proves that CompactSession returns an error when the session key is empty.
	store := session.NewStore(t.TempDir())
	compactor := compaction.NewCompactor(store, 0.8)
	ag := &Agent{
		Sessions:  store,
		Compactor: compactor,
	}

	_, err := ag.CompactSession(context.Background(), "", false)
	if err == nil {
		t.Fatal("expected error for empty session key")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("error = %q, want to contain 'no active session'", err.Error())
	}
}

func TestCompactSession_ErrorWhenTooFewMessages(t *testing.T) {
	// Proves that CompactSession refuses to compact when the session has fewer
	// than 5 messages, returning a descriptive error with the message count.
	store := session.NewStore(t.TempDir())
	compactor := compaction.NewCompactor(store, 0.8)
	sessionKey := "test/few/1000000000"

	// Seed 4 messages (below the 5-message threshold).
	for i := 0; i < 2; i++ {
		if err := store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("x")}); err != nil {
			t.Fatal(err)
		}
		if err := store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("y")}); err != nil {
			t.Fatal(err)
		}
	}

	ag := &Agent{
		Sessions:  store,
		Compactor: compactor,
	}

	_, err := ag.CompactSession(context.Background(), sessionKey, false)
	if err == nil {
		t.Fatal("expected error for too few messages")
	}
	if !strings.Contains(err.Error(), "too few messages") {
		t.Errorf("error = %q, want to contain 'too few messages'", err.Error())
	}
	if !strings.Contains(err.Error(), "4") {
		t.Errorf("error = %q, want to contain actual count '4'", err.Error())
	}
}

func TestCompactSession_ErrorWhenEmptySession(t *testing.T) {
	// Proves that CompactSession handles a session with zero messages
	// by returning the "too few messages" error.
	store := session.NewStore(t.TempDir())
	compactor := compaction.NewCompactor(store, 0.8)
	ag := &Agent{
		Sessions:  store,
		Compactor: compactor,
	}

	_, err := ag.CompactSession(context.Background(), "test/empty/1000000000", false)
	if err == nil {
		t.Fatal("expected error for empty session")
	}
	if !strings.Contains(err.Error(), "too few messages") {
		t.Errorf("error = %q, want to contain 'too few messages'", err.Error())
	}
}

func TestResetDelegatedSession(t *testing.T) {
	// Proves that resetDelegatedSession sends memory formation to the backend,
	// detaches it, lets memory formation complete in the background, rotates the
	// foci session key, and reloads the bootstrap.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/delegated/1000000000"

	// Seed the session so RotateKey has a file to archive.
	if err := store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("hello")}); err != nil {
		t.Fatal(err)
	}
	if err := store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("hi")}); err != nil {
		t.Fatal(err)
	}

	// Build a mock DelegatedManager.
	var waitForTurnCalled atomic.Bool
	var backendClosed atomic.Bool

	mockBe := &mockBackend{
		waitForTurnFn: func(ctx context.Context) error {
			waitForTurnCalled.Store(true)
			return nil
		},
	}
	dm := &DelegatedManager{
		backends: map[string]*managedBackend{
			"test/delegated/1000000000": {be: mockBe},
		},
	}

	mockBe.closeFn = func() error {
		backendClosed.Store(true)
		return nil
	}

	// For the memory formation, we need HandleMessage to work. Since we're
	// in delegated mode, HandleMessage returns ("", nil) immediately.
	// We need a client for the OrchestrateFullTurn path.
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Role:       "assistant",
			Content:    provider.TextContent("memory formed"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		}
	})

	ag := &Agent{
		Client:           client,
		Sessions:         store,
		Tools:            tools.NewRegistry(),
		Bootstrap:        bootstrap,
		DelegatedManager: dm,
		Model:            "claude-haiku-4-5",
		MemoryFormationConfig: config.ResolvedMemoryFormation{
			SessionEndEnabled: true,
			SessionEndPrompt:  "form memories now",
		},
	}

	var notifyMsgs []string
	ag.ResetNotifyFunc.Add(func(sk, msg string) {
		notifyMsgs = append(notifyMsgs, msg)
	})

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	var rotatedOld, rotatedNew string
	ag.SessionKeyRotatedFunc.Add(func(old, new string) {
		rotatedOld = old
		rotatedNew = new
	})

	newKey, err := ag.ResetSession(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("ResetSession (delegated): %v", err)
	}
	if newKey == "" || newKey == sessionKey {
		t.Errorf("newKey = %q, want a rotated key different from %q", newKey, sessionKey)
	}

	// Verify the progress notification was sent.
	if len(notifyMsgs) == 0 {
		t.Error("expected a progress notification during delegated reset")
	}
	if len(notifyMsgs) > 0 && !strings.Contains(notifyMsgs[0], "Session reset") {
		t.Errorf("notification = %q, want to contain 'Session reset'", notifyMsgs[0])
	}

	// Memory formation runs in a background goroutine — give it a moment.
	time.Sleep(100 * time.Millisecond)

	// Verify WaitForTurn was called (background goroutine waits for memory formation).
	if !waitForTurnCalled.Load() {
		t.Error("WaitForTurn was not called")
	}

	// Verify the backend was closed (background goroutine closes after memory formation).
	if !backendClosed.Load() {
		t.Error("backend Close was not called after memory formation")
	}

	// Verify session rotation.
	if rotatedOld != sessionKey || rotatedNew != newKey {
		t.Errorf("rotation: old=%q new=%q, want %q/%q", rotatedOld, rotatedNew, sessionKey, newKey)
	}

	// Verify bootstrap was reloaded.
	if !nudgeReloaded {
		t.Error("NudgeReloadFunc was not called after delegated reset")
	}
}

func TestResetDelegatedSession_MemoryDisabled(t *testing.T) {
	// Proves that when SessionEndEnabled is false, resetDelegatedSession skips
	// memory formation but still resets the backend and rotates the key.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/delnomem/1000000000"

	if err := store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("hello")}); err != nil {
		t.Fatal(err)
	}
	if err := store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("hi")}); err != nil {
		t.Fatal(err)
	}

	var backendClosed bool
	mockBe := &mockBackend{
		closeFn: func() error {
			backendClosed = true
			return nil
		},
	}
	dm := &DelegatedManager{
		backends: map[string]*managedBackend{
			"test/delnomem/1000000000": {be: mockBe},
		},
	}

	ag := &Agent{
		Sessions:         store,
		Bootstrap:        bootstrap,
		DelegatedManager: dm,
		MemoryFormationConfig: config.ResolvedMemoryFormation{
			SessionEndEnabled: false,
		},
	}

	newKey, err := ag.ResetSession(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	if newKey == "" || newKey == sessionKey {
		t.Errorf("expected rotated key, got %q", newKey)
	}
	if !backendClosed {
		t.Error("backend should be closed even when memory is disabled")
	}
}

func TestReloadAfterMutation(t *testing.T) {
	// Proves that reloadAfterMutation calls Bootstrap.Reload, NudgeReloadFunc,
	// and InvalidateSystemCaches — the shared reload sequence used by reset,
	// compact, and reload operations.
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{Bootstrap: bootstrap}

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	// Prime two session meta entries with cached system blocks.
	sm1 := ag.getSessionMeta("test/s1/1")
	sm1.systemBlocks = []provider.SystemBlock{{Text: "cached-1"}}
	sm2 := ag.getSessionMeta("test/s2/1")
	sm2.systemBlocks = []provider.SystemBlock{{Text: "cached-2"}}

	ag.reloadAfterMutation()

	if !nudgeReloaded {
		t.Error("NudgeReloadFunc was not called")
	}
	if sm1.systemBlocks != nil {
		t.Error("session 1 system cache should be invalidated")
	}
	if sm2.systemBlocks != nil {
		t.Error("session 2 system cache should be invalidated")
	}
}

// mockBackend is a minimal mock implementing delegator.Delegator for lifecycle tests.
type mockBackend struct {
	waitForTurnFn func(ctx context.Context) error
	closeFn       func() error
	sendFn        func(ctx context.Context, prompt string) error
}

func (m *mockBackend) Start(_ context.Context, _ delegator.StartOptions) error { return nil }
func (m *mockBackend) SendToPane(_ context.Context, prompt string, _ *delegator.EventHandler) (*delegator.TurnResult, error) {
	if m.sendFn != nil {
		return nil, m.sendFn(context.Background(), prompt)
	}
	return nil, nil
}
func (m *mockBackend) WaitForTurn(ctx context.Context) error {
	if m.waitForTurnFn != nil {
		return m.waitForTurnFn(ctx)
	}
	return nil
}
func (m *mockBackend) IsTurnInFlight() bool                                    { return false }
func (m *mockBackend) SendCommand(_ context.Context, _ string, _ string) error { return nil }
func (m *mockBackend) IsRunning() bool                                         { return true }
func (m *mockBackend) Restart(_ context.Context) error                         { return nil }
func (m *mockBackend) SetReplyFunc(_ delegator.ReplyFunc)                        {}
func (m *mockBackend) SetPermissionPromptFunc(_ delegator.PermissionPromptFunc)  {}
func (m *mockBackend) SetOnPermissionCleared(_ func())                         {}
func (m *mockBackend) SetOnSessionReady(_ func(string))                        {}
func (m *mockBackend) SetTypingFunc(_ func(bool))                              {}
func (m *mockBackend) SendKeystroke(_ context.Context, _ string) error         { return nil }
func (m *mockBackend) SendSpecialKey(_ context.Context, _ string) error        { return nil }
func (m *mockBackend) Interrupt(_ context.Context) error                       { return nil }
func (m *mockBackend) SessionID() string                                       { return "" }
func (m *mockBackend) SessionFilePath() string                                 { return "" }
func (m *mockBackend) WaitReady(_ context.Context) error                       { return nil }
func (m *mockBackend) Close() error {
	if m.closeFn != nil {
		return m.closeFn()
	}
	return nil
}

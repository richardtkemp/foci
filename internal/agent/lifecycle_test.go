package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/compaction"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

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
		// Reflection.SessionEndEnabled defaults to false, so
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

func TestResetSessionHard_APIPath_RotatesEvenWhenProcessing(t *testing.T) {
	// Proves that ResetSessionHard does NOT check the in-flight gate and always
	// proceeds to rotate the key. The whole point of /reset hard is to
	// recover from a stuck turn — it must never refuse on the in-flight
	// gate that ResetSession uses.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/hard/1000000000"

	if err := store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("hello")}); err != nil {
		t.Fatal(err)
	}
	if err := store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("hi")}); err != nil {
		t.Fatal(err)
	}

	ag := &Agent{
		Sessions:  store,
		Bootstrap: bootstrap,
	}
	// Simulate an in-flight turn on this session — ResetSession would refuse here.
	ag.SetTurnInFlightForTest(sessionKey, true)

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	var rotatedOld, rotatedNew string
	ag.SessionKeyRotatedFunc.Add(func(old, new string) {
		rotatedOld = old
		rotatedNew = new
	})

	newKey, err := ag.ResetSessionHard(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("ResetSessionHard: %v (must not refuse while processing)", err)
	}
	if newKey == "" || newKey == sessionKey {
		t.Errorf("newKey = %q, want a rotated key different from %q", newKey, sessionKey)
	}
	if rotatedOld != sessionKey || rotatedNew != newKey {
		t.Errorf("rotation: old=%q new=%q, want %q/%q", rotatedOld, rotatedNew, sessionKey, newKey)
	}
	if !nudgeReloaded {
		t.Error("NudgeReloadFunc was not called after hard reset")
	}
}

func TestResetSessionHard_NoSessionKey(t *testing.T) {
	// Proves that ResetSessionHard rejects an empty session key with a
	// clear error rather than panicking.
	ag := &Agent{}
	_, err := ag.ResetSessionHard(context.Background(), "")
	if err == nil {
		t.Fatal("expected error when session key is empty")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("error = %q, want to contain 'no active session'", err.Error())
	}
}

func TestResetSessionHard_DelegatedPath_NoMemoryFormation(t *testing.T) {
	// Proves that ResetSessionHard for a delegated agent skips the memory
	// formation prompt entirely (the difference from ResetSession), still
	// closes the backend, and rotates the session key. SessionEndEnabled is
	// deliberately TRUE here — hard reset must skip memory formation
	// regardless of config.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/hardDelegated/1000000000"

	if err := store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("hello")}); err != nil {
		t.Fatal(err)
	}
	if err := store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("hi")}); err != nil {
		t.Fatal(err)
	}

	var memorySent atomic.Bool
	var backendClosed atomic.Bool

	dm := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			be := &mockBackendDM{running: true}
			be.closeFn = func() error {
				backendClosed.Store(true)
				return nil
			}
			be.sendToPaneFn = func(_ context.Context, _ string, _ *mockHandler) (*delegator.TurnResult, error) {
				memorySent.Store(true)
				return &delegator.TurnResult{}, nil
			}
			return be, nil
		},
		StartOpts:   delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:     "test",
		IdleTimeout: time.Hour,
	}
	t.Cleanup(func() { dm.Close() })

	if _, err := dm.Get(context.Background(), sessionKey); err != nil {
		t.Fatalf("pre-create backend: %v", err)
	}
	mb, _ := dm.getManaged(sessionKey)
	mock := mb.be.(*mockBackendDM)

	ag := &Agent{
		Sessions:         store,
		Tools:            tools.NewRegistry(),
		Bootstrap:        bootstrap,
		DelegatedManager: dm,
		Reflection: config.ResolvedReflection{
			SessionEndEnabled: true, // would normally fire memory formation
		},
	}

	newKey, err := ag.ResetSessionHard(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("ResetSessionHard (delegated): %v", err)
	}
	if newKey == "" || newKey == sessionKey {
		t.Errorf("newKey = %q, want a rotated key different from %q", newKey, sessionKey)
	}

	if memorySent.Load() {
		t.Error("memory formation was sent — hard reset must skip it")
	}
	if !mock.wasInterrupted() {
		t.Error("backend Interrupt was not called by hard reset")
	}
	if !backendClosed.Load() {
		t.Error("backend Close was not called by hard reset")
	}
}

func TestResetSession_ErrorWhenProcessing(t *testing.T) {
	// Proves that ResetSession returns an error when THIS session is currently
	// processing a turn, so the user must stop it first.
	ag := &Agent{}
	ag.SetTurnInFlightForTest("test/busy/1", true)

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

func TestCompactSession_FiresMemoryHook(t *testing.T) {
	// Proves that manual /compact fires the CompactionMemoryFunc hook before
	// summarising, matching auto-compaction behavior. Regression test for the
	// bug where manual compact silently skipped memory formation, losing the
	// chance to persist insights from the pre-compaction transcript.
	var turnCount atomic.Int32
	client := compactionTestClient(&turnCount, -1)

	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	compactor := compaction.NewCompactor(store, 0.8)

	sessionKey := "test/compactmem/1000000000"
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

	var memoryFiredFor string
	ag.CompactionMemoryFunc.Add(func(sk string) { memoryFiredFor = sk })

	if _, err := ag.CompactSession(context.Background(), sessionKey, false); err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if memoryFiredFor != sessionKey {
		t.Errorf("CompactionMemoryFunc fired for %q, want %q", memoryFiredFor, sessionKey)
	}

	// Dry-run must NOT fire memory formation (no side effects).
	memoryFiredFor = ""
	// Seed a new session for dry-run so the rotation from the first compaction
	// doesn't leave us pointing at a stale key.
	dryKey := "test/compactmemdry/1000000000"
	for i := 0; i < 3; i++ {
		if err := store.TestAppend(dryKey, provider.Message{Role: "user", Content: provider.TextContent("x")}); err != nil {
			t.Fatal(err)
		}
		if err := store.TestAppend(dryKey, provider.Message{Role: "assistant", Content: provider.TextContent("y")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ag.CompactSession(context.Background(), dryKey, true); err != nil {
		t.Fatalf("CompactSession dry-run: %v", err)
	}
	if memoryFiredFor != "" {
		t.Errorf("CompactionMemoryFunc fired during dry-run (for %q) — should be suppressed", memoryFiredFor)
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

func TestCompactSession_Delegated(t *testing.T) {
	// Proves that /compact on a delegated agent bypasses the foci message-count
	// gate, resolves the backend via DelegatedManager, sends "/compact <prompt>"
	// to CC, and fires start + notify hooks. This is the regression test for
	// the bug where foci's MessageCount returned 0 for ccstream sessions
	// (CC owns the session file) so /compact always failed with "too few messages".
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/delegatedcompact/1000000000"

	// Note: we deliberately do NOT seed any messages. Delegated agents have
	// empty foci session files — the whole point is that /compact must work
	// anyway.

	var sentCommand string
	dm := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			be := &mockBackendDM{running: true}
			be.sendCommandFn = func(_ context.Context, cmd string) error {
				sentCommand = cmd
				return nil
			}
			return be, nil
		},
		StartOpts:   delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:     "test",
		IdleTimeout: time.Hour,
	}
	t.Cleanup(func() { dm.Close() })

	// Pre-create the backend so Get() returns it.
	if _, err := dm.Get(context.Background(), sessionKey); err != nil {
		t.Fatalf("pre-create backend: %v", err)
	}

	ag := &Agent{
		Sessions:         store,
		Bootstrap:        bootstrap,
		DelegatedManager: dm,
		// Compactor is set on real delegated agents for the RunCompaction
		// threshold path, but manual /compact for delegated agents doesn't
		// need it — leaving nil here proves that.
	}

	var startMsgs, notifyMsgs []string
	ag.CompactionStartFunc.Add(func(_, msg string) { startMsgs = append(startMsgs, msg) })
	ag.CompactionNotifyFunc.Add(func(_, msg string) { notifyMsgs = append(notifyMsgs, msg) })

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	result, err := ag.CompactSession(context.Background(), sessionKey, false)
	if err != nil {
		t.Fatalf("CompactSession (delegated): %v", err)
	}
	if result.NewSessionKey != "" {
		t.Errorf("delegated compact should not rotate session key, got %q", result.NewSessionKey)
	}

	if !strings.HasPrefix(sentCommand, "/compact ") {
		t.Errorf("expected backend to receive /compact command, got %q", sentCommand)
	}
	if len(sentCommand) <= len("/compact ") {
		t.Errorf("/compact command has no summary prompt body: %q", sentCommand)
	}

	if len(startMsgs) != 1 {
		t.Errorf("expected 1 start message, got %d", len(startMsgs))
	}
	if len(notifyMsgs) != 1 {
		t.Errorf("expected 1 notify message, got %d", len(notifyMsgs))
	}
	if !nudgeReloaded {
		t.Error("NudgeReloadFunc should fire after delegated compaction")
	}
}

func TestCompactSession_Delegated_DryRunUnsupported(t *testing.T) {
	// Proves that dry-run mode returns a clear error for delegated agents,
	// because CC's /compact command has no dry-run counterpart.
	dm := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			return &mockBackendDM{running: true}, nil
		},
		StartOpts:   delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:     "test",
		IdleTimeout: time.Hour,
	}
	t.Cleanup(func() { dm.Close() })

	ag := &Agent{DelegatedManager: dm}

	_, err := ag.CompactSession(context.Background(), "test/session/1", true)
	if err == nil {
		t.Fatal("expected dry-run to be rejected for delegated agents")
	}
	if !strings.Contains(err.Error(), "dry-run") {
		t.Errorf("error = %q, want to contain 'dry-run'", err.Error())
	}
}

func TestResetDelegatedSession(t *testing.T) {
	// Proves the delegated soft reset is non-blocking: it rotates the foci key
	// and reloads the bootstrap synchronously, then runs memory formation on
	// the old CC session and closes that backend in the BACKGROUND. The backend
	// must stay open until reflection completes, then close.
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

	// The mock blocks inside reflection until the test releases it, so we can
	// observe that ResetSession returned without waiting.
	var memorySent atomic.Bool
	var backendClosed atomic.Bool
	reflectStarted := make(chan struct{})
	reflectRelease := make(chan struct{})
	dm := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			be := &mockBackendDM{running: true}
			be.closeFn = func() error {
				backendClosed.Store(true)
				return nil
			}
			return be, nil
		},
		StartOpts:   delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:     "test",
		IdleTimeout: time.Hour,
	}
	t.Cleanup(func() { dm.Close() })

	// Pre-create the backend so Get() finds it.
	_, err := dm.Get(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("pre-create backend: %v", err)
	}

	// Reflection (memory formation) lands here; signal start, then block until
	// the test releases us, then complete the turn.
	mb, _ := dm.getManaged(sessionKey)
	mock := mb.be.(*mockBackendDM)
	mock.sendToPaneFn = func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
		memorySent.Store(true)
		close(reflectStarted)
		<-reflectRelease
		result := &delegator.TurnResult{Text: "memory formed"}
		if handler != nil && handler.OnTurnComplete != nil {
			handler.OnTurnComplete(result)
		}
		return result, nil
	}

	ag := &Agent{
		Sessions:         store,
		Tools:            tools.NewRegistry(),
		Bootstrap:        bootstrap,
		DelegatedManager: dm,
		Model:            "claude-haiku-4-5",
		Reflection: config.ResolvedReflection{
			SessionEndEnabled: true,
		},
	}

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

	// Synchronous results: key rotated, rotation callback fired, bootstrap reloaded.
	if newKey == "" || newKey == sessionKey {
		t.Errorf("newKey = %q, want a rotated key different from %q", newKey, sessionKey)
	}
	if rotatedOld != sessionKey || rotatedNew != newKey {
		t.Errorf("rotation: old=%q new=%q, want %q/%q", rotatedOld, rotatedNew, sessionKey, newKey)
	}
	if !nudgeReloaded {
		t.Error("NudgeReloadFunc was not called after delegated reset")
	}

	// Reflection runs in the background — wait for it to start.
	select {
	case <-reflectStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reflection did not start in the background")
	}
	// While reflection is still in flight, the backend must NOT be closed.
	if backendClosed.Load() {
		t.Error("backend was closed before reflection completed")
	}

	// Release reflection; the backend must close only after it finishes.
	close(reflectRelease)
	deadline := time.Now().Add(2 * time.Second)
	for !backendClosed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !backendClosed.Load() {
		t.Error("backend was not closed after reflection completed")
	}
	if !memorySent.Load() {
		t.Error("memory formation was not sent to the backend")
	}
}

func TestResetDelegatedSession_MemoryDisabled(t *testing.T) {
	// Proves that when SessionEndEnabled is false, resetDelegatedSession skips
	// memory formation but still closes the backend and rotates the key.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/delnomem/1000000000"

	if err := store.TestAppend(sessionKey, provider.Message{Role: "user", Content: provider.TextContent("hello")}); err != nil {
		t.Fatal(err)
	}
	if err := store.TestAppend(sessionKey, provider.Message{Role: "assistant", Content: provider.TextContent("hi")}); err != nil {
		t.Fatal(err)
	}

	var backendClosed atomic.Bool
	dm := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			be := &mockBackendDM{running: true}
			be.closeFn = func() error {
				backendClosed.Store(true)
				return nil
			}
			return be, nil
		},
		StartOpts:   delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:     "test",
		IdleTimeout: time.Hour,
	}
	t.Cleanup(func() { dm.Close() })

	// Pre-create the backend.
	_, err := dm.Get(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("pre-create backend: %v", err)
	}

	ag := &Agent{
		Sessions:         store,
		Bootstrap:        bootstrap,
		DelegatedManager: dm,
		Reflection: config.ResolvedReflection{
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
	// Teardown is backgrounded even with reflection disabled — wait for close.
	deadline := time.Now().Add(2 * time.Second)
	for !backendClosed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !backendClosed.Load() {
		t.Error("backend should be closed in the background even when memory is disabled")
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

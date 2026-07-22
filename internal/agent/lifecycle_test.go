package agent

import (
	"context"
	"fmt"
	"path/filepath"
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

// seedSession appends count user/assistant message pairs to the session.
func seedSession(t *testing.T, store *session.Store, key string, pairs int) {
	t.Helper()
	for i := 0; i < pairs; i++ {
		if err := store.TestAppend(key, provider.Message{Role: "user", Content: provider.TextContent(fmt.Sprintf("msg %d", i))}); err != nil {
			t.Fatal(err)
		}
		if err := store.TestAppend(key, provider.Message{Role: "assistant", Content: provider.TextContent(fmt.Sprintf("reply %d", i))}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResetSession_APIPath(t *testing.T) {
	// Proves that ResetSession for a traditional (non-delegated) agent leaves
	// the session key stable, archives the history in place (the session
	// reloads empty), clears all per-session state (overrides + metadata
	// rows), and reloads the bootstrap.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()
	sessionKey := "test/ireset"

	seedSession(t, store, sessionKey, 1)

	ag := &Agent{
		Sessions:     store,
		Bootstrap:    bootstrap,
		SessionIndex: idx,
		// Reflection.SessionEndEnabled defaults to false, so the reflection
		// branch is never prepared — reset proceeds without memory formation.
	}

	// Per-session state that must be wiped by the reset.
	ag.SetSessionEffort(sessionKey, "high")
	ag.SetSessionNoCompact(sessionKey, true)

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	var orientCalled bool
	ag.ResetOrientTemplateFn = func() string {
		orientCalled = true
		return "test orientation"
	}

	outcome, err := ag.ResetSession(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	if outcome != ResetMemoryNone {
		t.Errorf("memory outcome = %v, want ResetMemoryNone (SessionEndEnabled false)", outcome)
	}

	// The key is a stable identity: the same key must now load an empty
	// (archived-in-place) session.
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 0 {
		t.Errorf("after reset: %d messages under %s, want 0 (archived in place)", len(msgs), sessionKey)
	}
	// Per-session overrides and metadata rows are gone.
	if got := ag.SessionEffort(sessionKey); got != "" {
		t.Errorf("effort override survived reset: %q", got)
	}
	if ag.SessionNoCompact(sessionKey) {
		t.Error("no_compact override survived reset")
	}
	if v, _ := idx.GetSessionMetadata(sessionKey, "effort"); v != "" {
		t.Errorf("effort metadata row survived reset: %q", v)
	}
	if v, _ := idx.GetSessionMetadata(sessionKey, "no_compact"); v != "" {
		t.Errorf("no_compact metadata row survived reset: %q", v)
	}
	if !nudgeReloaded {
		t.Error("NudgeReloadFunc was not called after reset")
	}
	if !orientCalled {
		t.Error("ResetOrientTemplateFn was not called")
	}
}

func TestResetSession_MemoryAlreadySaved(t *testing.T) {
	// Session-end memory enabled, but a prior reflection already covered the
	// session (nothing substantive since) → the reset saves nothing new and
	// reports ResetMemoryAlreadySaved. A freshly-registered index row seeds
	// last_reflection == last_activity, so it is redundant by construction.
	store := session.NewStore(t.TempDir())
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()
	sessionKey := "test/ireset"
	seedSession(t, store, sessionKey, 1)
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  sessionKey,
		FilePath:    "/x/root.jsonl",
		CreatedAt:   time.Now(),
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})

	ag := &Agent{
		Sessions:     store,
		Bootstrap:    workspace.NewBootstrap(t.TempDir(), []string{}),
		SessionIndex: idx,
		Reflection:   config.ResolvedReflection{SessionEndEnabled: true},
	}

	outcome, err := ag.ResetSession(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	if outcome != ResetMemoryAlreadySaved {
		t.Errorf("memory outcome = %v, want ResetMemoryAlreadySaved", outcome)
	}
}

func TestResetSessionHard_APIPath_ResetsEvenWhenProcessing(t *testing.T) {
	// Proves that ResetSessionHard does NOT check the in-flight gate and always
	// proceeds to archive the session. The whole point of /reset hard is to
	// recover from a stuck turn — it must never refuse on the in-flight
	// gate that ResetSession uses.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/ihard"

	seedSession(t, store, sessionKey, 1)

	ag := &Agent{
		Sessions:  store,
		Bootstrap: bootstrap,
	}
	// Simulate an in-flight turn on this session — ResetSession would refuse here.
	ag.SetTurnInFlightForTest(sessionKey, true)

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	if err := ag.ResetSessionHard(context.Background(), sessionKey); err != nil {
		t.Fatalf("ResetSessionHard: %v (must not refuse while processing)", err)
	}
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 0 {
		t.Errorf("after hard reset: %d messages, want 0 (archived in place)", len(msgs))
	}
	if !nudgeReloaded {
		t.Error("NudgeReloadFunc was not called after hard reset")
	}
}

func TestResetSessionHard_NoSessionKey(t *testing.T) {
	// Proves that ResetSessionHard rejects an empty session key with a
	// clear error rather than panicking.
	ag := &Agent{}
	err := ag.ResetSessionHard(context.Background(), "")
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
	// interrupts and closes the backend, and archives the session in place.
	// SessionEndEnabled is deliberately TRUE here — hard reset must skip
	// memory formation regardless of config.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/iharddel"

	seedSession(t, store, sessionKey, 1)

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

	if err := ag.ResetSessionHard(context.Background(), sessionKey); err != nil {
		t.Fatalf("ResetSessionHard (delegated): %v", err)
	}
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 0 {
		t.Errorf("after hard reset: %d messages, want 0 (archived in place)", len(msgs))
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
	ag.SetTurnInFlightForTest("test/ibusy", true)

	_, err := ag.ResetSession(context.Background(), "test/ibusy")
	if err == nil {
		t.Fatal("expected error when processing")
	}
	if !strings.Contains(err.Error(), "processing") {
		t.Errorf("error = %q, want to contain 'processing'", err.Error())
	}
}

func TestCompactSession_HappyPath(t *testing.T) {
	// Proves the full manual compaction lifecycle: CompactSession calls
	// doCompact, replaces the history in place under the SAME session key,
	// fires hooks, and reloads the bootstrap afterward.
	var turnCount atomic.Int32
	client := compactionTestClient(&turnCount, -1) // no high-token turn

	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	compactor := compaction.NewCompactor(store, 0.8)

	sessionKey := "test/icompactmanual"

	// Seed the session with 6 messages (3 turns) so we pass the >=5 check.
	seedSession(t, store, sessionKey, 3)

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

	var notifyMsgs, notifySummaries []string
	ag.CompactionNotifyFunc.Add(func(sk, msg, summary string) {
		notifyMsgs = append(notifyMsgs, msg)
		notifySummaries = append(notifySummaries, summary)
	})

	result, err := ag.CompactSession(context.Background(), sessionKey, false)
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}
	if result.OldMessageCount != 6 {
		t.Errorf("OldMessageCount = %d, want 6", result.OldMessageCount)
	}
	// The session key is stable: the compacted history (marker + summary +
	// handoff) loads under the original key.
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 3 {
		t.Errorf("after compaction: %d messages under %s, want 3", len(msgs), sessionKey)
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
	if len(notifySummaries) != 1 || notifySummaries[0] == "" {
		t.Errorf("expected CompactionNotifyFunc to receive a non-empty summary, got %q", notifySummaries)
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

	sessionKey := "test/icompactmem"
	seedSession(t, store, sessionKey, 3)

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
	// Seed a fresh session for dry-run — the first session was already
	// compacted down to 3 messages, below the >=5 gate.
	dryKey := "test/icompactmemdry"
	seedSession(t, store, dryKey, 3)
	if _, err := ag.CompactSession(context.Background(), dryKey, true); err != nil {
		t.Fatalf("CompactSession dry-run: %v", err)
	}
	if memoryFiredFor != "" {
		t.Errorf("CompactionMemoryFunc fired during dry-run (for %q) — should be suppressed", memoryFiredFor)
	}
}

func TestCompactSession_DryRun(t *testing.T) {
	// Proves that dry-run mode runs the full compaction pipeline but does NOT
	// reload the bootstrap and does NOT touch the session content.
	var turnCount atomic.Int32
	client := compactionTestClient(&turnCount, -1)

	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	compactor := compaction.NewCompactor(store, 0.8)

	sessionKey := "test/idryrun"

	seedSession(t, store, sessionKey, 3)

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

	var notifySummaries []string
	ag.CompactionNotifyFunc.Add(func(sk, msg, summary string) {
		notifySummaries = append(notifySummaries, summary)
	})

	if _, err := ag.CompactSession(context.Background(), sessionKey, true); err != nil {
		t.Fatalf("CompactSession dry-run: %v", err)
	}
	if nudgeReloaded {
		t.Error("NudgeReloadFunc should NOT be called on dry-run")
	}
	// Summary should have been passed to the debug hook.
	if len(debugMsgs) == 0 {
		t.Error("expected CompactionDebugFunc to receive summary on dry-run")
	}
	// And to the notify hook's 3rd arg too.
	if len(notifySummaries) != 1 || notifySummaries[0] == "" {
		t.Errorf("expected CompactionNotifyFunc to receive a non-empty summary on dry-run, got %q", notifySummaries)
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

	_, err := ag.CompactSession(context.Background(), "test/inocompactor", false)
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
	sessionKey := "test/ifew"

	// Seed 4 messages (below the 5-message threshold).
	seedSession(t, store, sessionKey, 2)

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

	_, err := ag.CompactSession(context.Background(), "test/iempty", false)
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
	sessionKey := "test/idelcompact"

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

	var startMsgs, notifyMsgs, notifySummaries []string
	ag.CompactionStartFunc.Add(func(_, msg string) { startMsgs = append(startMsgs, msg) })
	ag.CompactionNotifyFunc.Add(func(_, msg, summary string) {
		notifyMsgs = append(notifyMsgs, msg)
		notifySummaries = append(notifySummaries, summary)
	})

	var nudgeReloaded bool
	ag.NudgeReloadFunc = func() { nudgeReloaded = true }

	if _, err := ag.CompactSession(context.Background(), sessionKey, false); err != nil {
		t.Fatalf("CompactSession (delegated): %v", err)
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
	// Delegated compaction never computes a summary today — the notify
	// hook's 3rd arg should be empty (see runDelegatedCompact).
	if len(notifySummaries) != 1 || notifySummaries[0] != "" {
		t.Errorf("expected empty summary for delegated compaction, got %q", notifySummaries)
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

	_, err := ag.CompactSession(context.Background(), "test/isession", true)
	if err == nil {
		t.Fatal("expected dry-run to be rejected for delegated agents")
	}
	if !strings.Contains(err.Error(), "dry-run") {
		t.Errorf("error = %q, want to contain 'dry-run'", err.Error())
	}
}

func TestPrepareSessionEndMemory_RemapsBackendAndResumeID(t *testing.T) {
	// Proves the reflection-branch handoff for delegated agents:
	// PrepareSessionEndMemory creates the branch from the still-live history
	// BEFORE any reset, and RemapSession moves both the live backend and the
	// saved cc_resume_id row from the parent key to the branch key, leaving
	// the parent clean for a fresh backend.
	store := session.NewStore(t.TempDir())
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()
	sessionKey := "test/iprepare"

	seedSession(t, store, sessionKey, 1)

	dm := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			return &mockBackendDM{running: true}, nil
		},
		StartOpts:    delegator.StartOptions{WorkDir: t.TempDir()},
		AgentID:      "test",
		IdleTimeout:  time.Hour,
		SessionIndex: idx,
	}
	t.Cleanup(func() { dm.Close() })

	if _, err := dm.Get(context.Background(), sessionKey); err != nil {
		t.Fatalf("pre-create backend: %v", err)
	}
	// Simulate a persisted CC resume UUID for the live session.
	if err := idx.SetSessionMetadata(sessionKey, "cc_resume_id", "uuid-123"); err != nil {
		t.Fatalf("seed resume ID: %v", err)
	}

	ag := &Agent{
		Sessions:         store,
		SessionIndex:     idx,
		DelegatedManager: dm,
		Reflection: config.ResolvedReflection{
			SessionEndEnabled: true,
		},
	}

	branchKey, ok := ag.PrepareSessionEndMemory(sessionKey, "orient", false)
	if !ok {
		t.Fatal("PrepareSessionEndMemory returned ok=false, want a reflection branch")
	}
	if !strings.HasPrefix(branchKey, sessionKey+"/b") {
		t.Fatalf("branchKey = %q, want a 'b' child of %q", branchKey, sessionKey)
	}

	// The branch was created against the live history, before any reset.
	meta, err := store.GetBranchMeta(branchKey)
	if err != nil || meta == nil {
		t.Fatalf("GetBranchMeta(%s): meta=%v err=%v", branchKey, meta, err)
	}
	if !meta.NoResetHook {
		t.Error("reflection branch should have NoResetHook set")
	}

	// The live backend moved from the parent key to the branch key.
	if _, stillMapped := dm.getManaged(sessionKey); stillMapped {
		t.Error("backend still mapped under the parent key after remap")
	}
	if _, mapped := dm.getManaged(branchKey); !mapped {
		t.Error("backend not mapped under the branch key after remap")
	}

	// The cc_resume_id row moved with it.
	if v, _ := idx.GetSessionMetadata(branchKey, "cc_resume_id"); v != "uuid-123" {
		t.Errorf("branch cc_resume_id = %q, want uuid-123", v)
	}
	if v, _ := idx.GetSessionMetadata(sessionKey, "cc_resume_id"); v != "" {
		t.Errorf("parent cc_resume_id = %q, want empty after remap", v)
	}

	// Reflection branches must never auto-compact mid-reflection.
	if !ag.SessionNoCompact(branchKey) {
		t.Error("reflection branch should have no_compact set")
	}
}

func TestResetDelegatedSession(t *testing.T) {
	// Proves the delegated soft reset is non-blocking and key-stable: it
	// archives the history in place and reloads the bootstrap synchronously,
	// hands the live backend to the reflection branch, then runs memory
	// formation on that branch and closes its backend in the BACKGROUND. The
	// backend must stay open until reflection completes, then close.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/idelegated"

	seedSession(t, store, sessionKey, 1)

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

	outcome, err := ag.ResetSession(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("ResetSession (delegated): %v", err)
	}
	if outcome != ResetMemoryReflecting {
		t.Errorf("memory outcome = %v, want ResetMemoryReflecting (background reflection)", outcome)
	}

	// Synchronous results: history archived in place under the stable key,
	// bootstrap reloaded, and the live backend handed to the reflection
	// branch (parent key left clean).
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 0 {
		t.Errorf("after reset: %d messages under %s, want 0 (archived in place)", len(msgs), sessionKey)
	}
	if !nudgeReloaded {
		t.Error("NudgeReloadFunc was not called after delegated reset")
	}
	if _, stillMapped := dm.getManaged(sessionKey); stillMapped {
		t.Error("backend should have been remapped to the reflection branch, not left on the parent key")
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

func TestResetDelegatedSession_StampsReflectionForRedundancyGuard(t *testing.T) {
	// Reproduces #1465: "Memories from the previous session are being saved
	// in the background" fired even though a session-end-memory reflection
	// had already run and nothing happened since. RunSessionEndMemory never
	// called SessionIndex.StampReflection — only the unrelated periodic
	// interval-reflection pass (internal/periodic/reflection.go) did — so
	// ReflectionRedundant could never observe that a session-end pass (reset
	// / reclaim / scheduled reset) had just completed, and the very next
	// /reset with zero intervening activity still reported "reflecting"
	// instead of "already saved".
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	defer idx.Close()
	sessionKey := "test/istamp"

	seedSession(t, store, sessionKey, 1)

	created := time.Now().Add(-time.Hour)
	idx.Upsert(session.SessionIndexEntry{
		SessionKey:  sessionKey,
		FilePath:    "/x/root.jsonl",
		CreatedAt:   created,
		SessionType: session.SessionTypeChat,
		Status:      session.SessionStatusActive,
	})
	// Simulate real activity strictly after the row was created, so the
	// freshly-seeded last_reflection==last_activity coincidence (see
	// TestResetSession_MemoryAlreadySaved) can't mask the bug.
	activityAt := time.Now().Add(-time.Minute)
	idx.UpdateActivity(sessionKey, activityAt)

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

	if _, err := dm.Get(context.Background(), sessionKey); err != nil {
		t.Fatalf("pre-create backend: %v", err)
	}
	mb, _ := dm.getManaged(sessionKey)
	mock := mb.be.(*mockBackendDM)
	mock.sendToPaneFn = func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
		SessionIndex:     idx,
		Model:            "claude-haiku-4-5",
		Reflection: config.ResolvedReflection{
			SessionEndEnabled: true,
		},
	}

	outcome, err := ag.ResetSession(context.Background(), sessionKey)
	if err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	if outcome != ResetMemoryReflecting {
		t.Fatalf("memory outcome = %v, want ResetMemoryReflecting (first reset must actually reflect)", outcome)
	}

	select {
	case <-reflectStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reflection did not start")
	}
	close(reflectRelease)
	deadline := time.Now().Add(2 * time.Second)
	for !backendClosed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !backendClosed.Load() {
		t.Fatal("reflection never completed (backend not closed)")
	}

	// The reflection just ran and covered everything up to activityAt, with
	// no activity since — the redundancy guard must now report "redundant"
	// so a FOLLOWING reset says "already saved" instead of reflecting again.
	if !idx.ReflectionRedundant(sessionKey) {
		t.Error("ReflectionRedundant = false right after a completed session-end-memory pass with no further activity, want true — RunSessionEndMemory must stamp last_reflection on the parent key (#1465)")
	}
}

func TestResetDelegatedSession_MemoryDisabled(t *testing.T) {
	// Proves that when SessionEndEnabled is false, the delegated reset skips
	// memory formation but still closes the backend and archives the session
	// in place.
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	sessionKey := "test/idelnomem"

	seedSession(t, store, sessionKey, 1)

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

	if _, err := ag.ResetSession(context.Background(), sessionKey); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	msgs, _ := store.Load(sessionKey)
	if len(msgs) != 0 {
		t.Errorf("after reset: %d messages, want 0 (archived in place)", len(msgs))
	}
	// No reflection branch adopted the backend — it must be closed so the
	// next message starts a genuinely fresh CC session.
	deadline := time.Now().Add(2 * time.Second)
	for !backendClosed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !backendClosed.Load() {
		t.Error("backend should be closed when no reflection branch adopts it")
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
	sm1 := ag.getSessionMeta("test/is1")
	sm1.systemBlocks = []provider.SystemBlock{{Text: "cached-1"}}
	sm2 := ag.getSessionMeta("test/is2")
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

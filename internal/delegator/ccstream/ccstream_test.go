package ccstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Constructor / newFromConfig
// ---------------------------------------------------------------------------

func TestNewFromConfig(t *testing.T) {
	// Verifies that newFromConfig returns a Backend with initialised channels
	// and maps, storing the cfg for later use by Start.
	t.Parallel()

	cfg := map[string]any{"model": "opus"}
	b, err := newFromConfig(cfg)
	if err != nil {
		t.Fatalf("newFromConfig: %v", err)
	}
	be, ok := b.(*Backend)
	if !ok {
		t.Fatalf("expected *Backend, got %T", b)
	}
	if be.cfg["model"] != "opus" {
		t.Errorf("cfg[model] = %v, want %q", be.cfg["model"], "opus")
	}
	if be.readyCh == nil {
		t.Error("readyCh is nil, want initialised channel")
	}
	if be.pendingPerms == nil {
		t.Error("pendingPerms is nil, want initialised map")
	}
}

func TestNewFromConfig_EmptyConfig(t *testing.T) {
	// Verifies that an empty config produces a valid backend without error.
	t.Parallel()

	b, err := newFromConfig(map[string]any{})
	if err != nil {
		t.Fatalf("newFromConfig: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil Backend")
	}
}

func TestNewFromConfig_NilConfig(t *testing.T) {
	// Verifies that nil config is accepted (cfg will be nil but that's valid
	// before Start populates everything).
	t.Parallel()

	b, err := newFromConfig(nil)
	if err != nil {
		t.Fatalf("newFromConfig: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil Backend")
	}
}

// ---------------------------------------------------------------------------
// Callback setters
// ---------------------------------------------------------------------------

func TestCallbackSetters(t *testing.T) {
	// Verifies that each Set* method stores the callback so it can be invoked
	// by the handler methods. We set each callback, then verify the stored
	// field is non-nil and callable.
	t.Parallel()

	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}

	// SetPermissionPromptFunc
	var permCalled bool
	b.SetPermissionPromptFunc(func(reqID, text, summary string, choices []delegator.PromptChoice) {
		permCalled = true
	})
	if b.permPromptFn == nil {
		t.Error("permPromptFn is nil after SetPermissionPromptFunc")
	}
	b.permPromptFn("", "", "", nil)
	if !permCalled {
		t.Error("permPromptFn was not called")
	}

	// SetOnPromptsCleared — registered via the OutstandingRegistry's onEmpty
	// hook. We exercise it by registering+resolving a prompt and asserting the
	// hook fired exactly once.
	var clearedCalled bool
	b.SetOnPromptsCleared(func() { clearedCalled = true })
	b.outstanding.Register("test-req", OutstandingPermission)
	b.outstanding.Resolve("test-req")
	if !clearedCalled {
		t.Error("SetOnPromptsCleared callback was not fired when the registry emptied")
	}

	// SetOnSessionReady
	var readyID string
	b.SetOnSessionReady(func(id string) { readyID = id })
	if b.onSessionReady == nil {
		t.Error("onSessionReady is nil after SetOnSessionReady")
	}
	b.onSessionReady("sess-123")
	if readyID != "sess-123" {
		t.Errorf("readyID = %q, want %q", readyID, "sess-123")
	}

	// SetTypingFunc
	var typingVal bool
	b.SetTypingFunc(func(v bool) { typingVal = v })
	if b.typingFunc == nil {
		t.Error("typingFunc is nil after SetTypingFunc")
	}
	b.typingFunc(true)
	if !typingVal {
		t.Error("typingFunc(true) did not set value")
	}

	// SetOnCompactionStart
	var compStartCalled bool
	b.SetOnCompactionStart(func() { compStartCalled = true })
	if b.onCompactionStart == nil {
		t.Error("onCompactionStart is nil after SetOnCompactionStart")
	}
	b.onCompactionStart()
	if !compStartCalled {
		t.Error("onCompactionStart was not called")
	}

	// SetOnCompactionDone
	var compDoneTokens int
	b.SetOnCompactionDone(func(preTokens int) { compDoneTokens = preTokens })
	if b.onCompactionDone == nil {
		t.Error("onCompactionDone is nil after SetOnCompactionDone")
	}
	b.onCompactionDone(50000)
	if compDoneTokens != 50000 {
		t.Errorf("compDoneTokens = %d, want 50000", compDoneTokens)
	}

	// SetOnAgentStatus
	var agentStatusText string
	b.SetOnAgentStatus(func(text string) { agentStatusText = text })
	if b.agents.OnStatus == nil {
		t.Error("agents.OnStatus is nil after SetOnAgentStatus")
	}
	b.agents.OnStatus("running")
	if agentStatusText != "running" {
		t.Errorf("agentStatusText = %q, want %q", agentStatusText, "running")
	}
}

// ---------------------------------------------------------------------------
// State methods
// ---------------------------------------------------------------------------

func TestSessionID(t *testing.T) {
	// Verifies SessionID returns the stored session ID under the mutex.
	t.Parallel()

	b := &Backend{}
	if id := b.SessionID(); id != "" {
		t.Errorf("initial SessionID = %q, want empty", id)
	}
	b.sessionID = "sess-abc"
	if id := b.SessionID(); id != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", id, "sess-abc")
	}
}

func TestSessionFilePath(t *testing.T) {
	// Verifies SessionFilePath always returns empty string for the stream backend.
	t.Parallel()

	b := &Backend{}
	if p := b.SessionFilePath(); p != "" {
		t.Errorf("SessionFilePath = %q, want empty", p)
	}
}

func TestIsRunning(t *testing.T) {
	// Verifies IsRunning reflects the running state under the mutex.
	t.Parallel()

	b := &Backend{}
	if b.IsRunning() {
		t.Error("IsRunning = true initially, want false")
	}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()
	if !b.IsRunning() {
		t.Error("IsRunning = false after setting true")
	}
}

func TestSendKeystroke(t *testing.T) {
	// Verifies SendKeystroke returns an error (not supported in stream backend).
	t.Parallel()

	b := &Backend{}
	err := b.SendKeystroke(context.Background(), "a")
	if err == nil {
		t.Error("SendKeystroke returned nil, want error")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error = %q, want 'not supported'", err.Error())
	}
}

func TestSendSpecialKey(t *testing.T) {
	// Verifies SendSpecialKey returns an error (not supported in stream backend).
	t.Parallel()

	b := &Backend{}
	err := b.SendSpecialKey(context.Background(), "Escape")
	if err == nil {
		t.Error("SendSpecialKey returned nil, want error")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error = %q, want 'not supported'", err.Error())
	}
}

func TestInterrupt(t *testing.T) {
	// Verifies Interrupt sends an interrupt control request via the writer.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
	}

	if err := b.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["type"] != "control_request" {
		t.Errorf("type = %v, want %q", got["type"], "control_request")
	}
}

// ---------------------------------------------------------------------------
// Turn state: beginTurn, cancelTurn, IsTurnInFlight
// ---------------------------------------------------------------------------

func TestBeginTurnAndCancel(t *testing.T) {
	// Verifies beginTurn sets turn state correctly and cancelTurn reverses it.
	t.Parallel()

	b := &Backend{}

	handler := &delegator.EventHandler{
		OnText: func(text string) {},
	}
	b.beginTurn(handler)

	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after beginTurn")
	}
	b.turnMu.Lock()
	if b.turnHandler != handler {
		t.Error("turnHandler not set")
	}
	if b.turnResultCh == nil {
		t.Error("turnResultCh is nil")
	}
	b.turnMu.Unlock()

	b.cancelTurn()
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after cancelTurn")
	}
	b.turnMu.Lock()
	if b.turnHandler != nil {
		t.Error("turnHandler not nil after cancelTurn")
	}
	b.turnMu.Unlock()
}

func TestBeginTurnResetsState(t *testing.T) {
	// Verifies beginTurn resets accumulated text, tool count, and usage from
	// a prior turn.
	t.Parallel()

	b := &Backend{}

	// Simulate prior turn residue.
	b.turnText.WriteString("old text")
	b.turnTools = 5
	b.lastUsage = &TokenUsage{InputTokens: 100}

	handler := &delegator.EventHandler{}
	b.beginTurn(handler)

	b.turnMu.Lock()
	if b.turnText.String() != "" {
		t.Errorf("turnText = %q after beginTurn, want empty", b.turnText.String())
	}
	if b.turnTools != 0 {
		t.Errorf("turnTools = %d after beginTurn, want 0", b.turnTools)
	}
	b.turnMu.Unlock()

	b.mu.Lock()
	if b.lastUsage != nil {
		t.Error("lastUsage not nil after beginTurn")
	}
	b.mu.Unlock()
}

func TestIsTurnInFlight_InitiallyFalse(t *testing.T) {
	// Verifies a fresh backend has no turn in flight.
	t.Parallel()

	b := &Backend{}
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true on fresh backend")
	}
}

// ---------------------------------------------------------------------------
// WaitForTurn
// ---------------------------------------------------------------------------

func TestWaitForTurn_NoTurn(t *testing.T) {
	// Verifies WaitForTurn returns immediately when no turn is in progress
	// (turnResultCh is nil).
	t.Parallel()

	b := &Backend{}
	if err := b.WaitForTurn(context.Background()); err != nil {
		t.Fatalf("WaitForTurn: %v", err)
	}
}

func TestWaitForTurn_SignalledByResult(t *testing.T) {
	// Verifies WaitForTurn unblocks when a result is pushed to turnResultCh.
	t.Parallel()

	b := &Backend{}
	b.turnResultCh = make(chan *ResultMessage, 1)

	// Signal completion.
	b.turnResultCh <- &ResultMessage{Subtype: "success"}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := b.WaitForTurn(ctx); err != nil {
		t.Fatalf("WaitForTurn: %v", err)
	}
}

func TestWaitForTurn_ContextCancellation(t *testing.T) {
	// Verifies WaitForTurn respects context cancellation when no result arrives.
	t.Parallel()

	b := &Backend{}
	b.turnResultCh = make(chan *ResultMessage, 1)
	// Do NOT send a result — WaitForTurn should block until context is cancelled.

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- b.WaitForTurn(ctx)
	}()

	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("WaitForTurn err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForTurn did not return after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// sendUserMessage — internal primitive backing Inject's user-text paths
// ---------------------------------------------------------------------------

// TestSendUserMessage_Idle_NoRearm verifies the internal sendUserMessage
// primitive at idle emits a well-formed user message on the wire and
// does NOT increment pendingRearmCount even when autoRearm=true — there's
// no in-flight turn to rearm against. This is the wire-shape check that
// pre-Phase-4 lived under TestSendCommand_Idle; protocol-level coverage
// stays here, routing-level coverage lives in TestInject_*.
func TestSendUserMessage_Idle_NoRearm(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}

	if err := b.sendUserMessage(context.Background(), "/compact", true); err != nil {
		t.Fatalf("sendUserMessage: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["type"] != "user" {
		t.Errorf("type = %v, want %q", got["type"], "user")
	}
	if _, present := got["priority"]; present {
		t.Errorf("priority field should be absent post-Phase-5, got %v", got["priority"])
	}
	msg := got["message"].(map[string]any)
	if msg["content"] != "/compact" {
		t.Errorf("content = %v, want %q", msg["content"], "/compact")
	}

	b.turnMu.Lock()
	count := b.pendingRearmCount
	b.turnMu.Unlock()
	if count != 0 {
		t.Errorf("pendingRearmCount = %d, want 0 for idle send (no turn to rearm against)", count)
	}
}

// rearmCheckingWriter wraps a writer and snapshots the backend's rearm
// counter on the first call to Write — the moment the bytes hit the wire.
// Used to assert the count was already non-zero BEFORE the user message
// reaches CC, closing the race where OnResult could see count==0, clear
// the handler, and drop the queued response's text.
type rearmCheckingWriter struct {
	dst              *bytes.Buffer
	b                *Backend
	rearmAtFirstWrt  int
	wroteOnce        bool
}

func (w *rearmCheckingWriter) Write(p []byte) (int, error) {
	if !w.wroteOnce {
		w.b.turnMu.Lock()
		w.rearmAtFirstWrt = w.b.pendingRearmCount
		w.b.turnMu.Unlock()
		w.wroteOnce = true
	}
	return w.dst.Write(p)
}

func (w *rearmCheckingWriter) Close() error { return nil }

// TestSendUserMessage_DuringTurn_RearmSetBeforeWrite proves that the
// in-flight rearm path arms the count BEFORE bytes hit CC. Closes the
// race observed in production as the "text block dropped (no
// handler/OnText)" WARN: if SendUser fired first, OnResult could process
// the in-flight turn's result (clearing the handler) before the rearm
// slot had been marked — the response to the queued command would then
// arrive at turnHandler==nil and be dropped. Phase 5 follow-up.
func TestSendUserMessage_DuringTurn_RearmSetBeforeWrite(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{turnActive: true}
	checker := &rearmCheckingWriter{dst: &buf, b: b}
	b.writer = NewWriter(checker)

	if err := b.sendUserMessage(context.Background(), "follow-up", true); err != nil {
		t.Fatalf("sendUserMessage: %v", err)
	}
	if !checker.wroteOnce {
		t.Fatal("writer was never called — sendUserMessage never reached the wire")
	}
	if checker.rearmAtFirstWrt < 1 {
		t.Errorf("pendingRearmCount at first byte = %d, want >= 1 — order is wrong, race window open",
			checker.rearmAtFirstWrt)
	}
}

// TestSendUserMessage_WriteFailureRollsBackRearm proves that if the
// underlying write fails after the rearm slot was incremented, the
// increment is rolled back — no response is coming from CC, so leaving
// the slot set would mis-route the next legitimate OnResult through
// the rearm path.
func TestSendUserMessage_WriteFailureRollsBackRearm(t *testing.T) {
	t.Parallel()

	failingWC := &failingWriteCloser{err: fmt.Errorf("boom")}
	b := &Backend{
		writer:     NewWriter(failingWC),
		turnActive: true,
	}

	if err := b.sendUserMessage(context.Background(), "x", true); err == nil {
		t.Fatal("sendUserMessage: expected error, got nil")
	}

	b.turnMu.Lock()
	count := b.pendingRearmCount
	b.turnMu.Unlock()
	if count != 0 {
		t.Errorf("pendingRearmCount = %d after write failure, want 0 (rollback)", count)
	}
}

// TestSendUserMessage_NoAutoRearm proves the slash-command path
// (autoRearm=false) does NOT arm the count even when called mid-turn.
// This is what keeps Inject(SourceCompact) safe to call defensively
// without corrupting the in-flight rearm state.
func TestSendUserMessage_NoAutoRearm(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:     NewWriter(nopWriteCloser{&buf}),
		turnActive: true,
	}

	if err := b.sendUserMessage(context.Background(), "/compact", false); err != nil {
		t.Fatalf("sendUserMessage: %v", err)
	}

	b.turnMu.Lock()
	count := b.pendingRearmCount
	b.turnMu.Unlock()
	if count != 0 {
		t.Errorf("pendingRearmCount = %d with autoRearm=false, want 0", count)
	}
}

type failingWriteCloser struct{ err error }

func (f *failingWriteCloser) Write([]byte) (int, error) { return 0, f.err }
func (f *failingWriteCloser) Close() error              { return nil }

// ---------------------------------------------------------------------------
// Inject — canonical entry point for user-role events
// ---------------------------------------------------------------------------

// TestInject_User_Idle_BeginsTurn verifies Inject(SourceUser) at idle
// dispatches to the begin-turn path: turnActive becomes true, the handler
// is installed, and the writer receives the text.
func TestInject_User_Idle_BeginsTurn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	handler := &delegator.EventHandler{OnText: func(string) {}}

	if err := b.Inject(context.Background(), delegator.Inject{
		Source:  delegator.SourceUser,
		Text:    "hello",
		Handler: handler,
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after begin-turn Inject; want true")
	}
	b.turnMu.Lock()
	gotHandler := b.turnHandler
	b.turnMu.Unlock()
	if gotHandler != handler {
		t.Error("turnHandler != supplied handler — Inject did not install it")
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("writer missing prompt text; got: %q", buf.String())
	}
}

// TestInject_User_Idle_WithAttachments verifies Inject routes attachments
// through the structured-content path when a turn is being begun. The
// writer should see image/document blocks alongside the text.
func TestInject_User_Idle_WithAttachments(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	handler := &delegator.EventHandler{OnText: func(string) {}}

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "describe this",
		Attachments: []delegator.Attachment{
			{MimeType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
		},
		Handler: handler,
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "image") {
		t.Errorf("writer did not emit image block; got: %q", out)
	}
	if !strings.Contains(out, "describe this") {
		t.Errorf("writer missing prompt text; got: %q", out)
	}
}

// TestInject_User_InFlight_QueuesAndArmsRearm verifies Inject(SourceUser)
// during an in-flight turn queues the text via SendUser and increments
// the rearm count so the queued response reaches the existing handler.
// Attachments and Handler in the Inject struct are ignored on the
// follow-up path — the in-flight turn already has its handler installed.
func TestInject_User_InFlight_QueuesAndArmsRearm(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:     NewWriter(nopWriteCloser{&buf}),
		turnActive: true,
	}

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "follow-up",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	b.turnMu.Lock()
	count := b.pendingRearmCount
	b.turnMu.Unlock()
	if count != 1 {
		t.Errorf("pendingRearmCount = %d after in-flight User Inject, want 1", count)
	}
	if !strings.Contains(buf.String(), "follow-up") {
		t.Errorf("writer did not see follow-up text; got: %q", buf.String())
	}
}

// TestInject_Steer_InFlight_InterruptsAndArms verifies Inject(SourceSteer)
// during an in-flight turn calls Interrupt before queueing — the urgent
// dispatch pattern that telegram/discord agent_setup currently spells out
// as Interrupt + SendCommand. Empirically: writer captures both the
// interrupt control message and the queued user text, and the rearm
// count is incremented for the steer.
func TestInject_Steer_InFlight_InterruptsAndArms(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:           NewWriter(nopWriteCloser{&buf}),
		turnActive:       true,
		pendingControls:  make(map[string]chan json.RawMessage),
	}

	// Interrupt sends a control_request — it expects a control_response
	// from CC. We don't have a real CC stream, so we let it time out fast
	// by giving Interrupt a short context. The point of the test is the
	// ORDERING (Interrupt fires, then SendUser fires) and the rearm flag,
	// not Interrupt's success — Inject logs and continues even if
	// Interrupt errors.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_ = b.Inject(ctx, delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "urgent text",
	})

	out := buf.String()
	// SendUser must have fired despite Interrupt timing out — the steer
	// text should still reach CC.
	if !strings.Contains(out, "urgent text") {
		t.Errorf("Steer text missing from writer output; got: %q", out)
	}
	// Interrupt control message should appear in the writer too (it's
	// emitted before SendUser).
	if !strings.Contains(out, "interrupt") {
		t.Errorf("interrupt control_request missing from writer; got: %q", out)
	}

	b.turnMu.Lock()
	count := b.pendingRearmCount
	b.turnMu.Unlock()
	if count != 1 {
		t.Errorf("pendingRearmCount = %d after Steer Inject, want 1", count)
	}
}

// TestInject_Steer_Idle_BeginsTurn verifies the edge case where a steer
// arrives at idle (race between turn end and platform queue dispatch):
// Inject degrades to a fresh begin-turn rather than calling Interrupt
// on a non-existent in-flight turn.
func TestInject_Steer_Idle_BeginsTurn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	handler := &delegator.EventHandler{OnText: func(string) {}}

	if err := b.Inject(context.Background(), delegator.Inject{
		Source:  delegator.SourceSteer,
		Text:    "steer-at-idle",
		Handler: handler,
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after Steer-at-idle Inject; want true (degraded to begin-turn)")
	}
	if strings.Contains(buf.String(), "interrupt") {
		t.Errorf("interrupt should NOT have fired at idle; writer: %q", buf.String())
	}
}

// TestInject_Compact_NoRearm verifies Inject(SourceCompact) at idle sends
// the slash command without arming the rearm count — /compact is
// fire-and-forget, the response (compact_boundary system event) flows
// through CompactionWaiter, not the user-text rearm cascade.
func TestInject_Compact_NoRearm(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   "/compact summarise everything",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	b.turnMu.Lock()
	count := b.pendingRearmCount
	b.turnMu.Unlock()
	if count != 0 {
		t.Errorf("pendingRearmCount = %d after Compact Inject, want 0 (no rearm for slash commands)", count)
	}
	if !strings.Contains(buf.String(), "/compact") {
		t.Errorf("writer missing /compact text; got: %q", buf.String())
	}
}

// TestInject_Compact_InFlight_NoRearm verifies the slash-command path
// stays rearm-free even when (defensively) called mid-turn. /compact
// shouldn't be invoked mid-turn in practice, but if it is, the response
// must NOT route through the user-text rearm cascade — that would
// corrupt the in-flight turn's delivery handler.
func TestInject_Compact_InFlight_NoRearm(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:     NewWriter(nopWriteCloser{&buf}),
		turnActive: true,
	}

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   "/compact x",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	b.turnMu.Lock()
	count := b.pendingRearmCount
	b.turnMu.Unlock()
	if count != 0 {
		t.Errorf("pendingRearmCount = %d, want 0 (Compact must never arm rearm even mid-turn)", count)
	}
}

// TestInject_Pass_NoRearm mirrors Compact for the /pass passthrough
// path — slash commands like /context, /model don't rearm.
func TestInject_Pass_NoRearm(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:     NewWriter(nopWriteCloser{&buf}),
		turnActive: true,
	}

	if err := b.Inject(context.Background(), delegator.Inject{
		Source: delegator.SourcePass,
		Text:   "/model opus",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	b.turnMu.Lock()
	count := b.pendingRearmCount
	b.turnMu.Unlock()
	if count != 0 {
		t.Errorf("pendingRearmCount = %d, want 0 for Pass slash command", count)
	}
}

// ---------------------------------------------------------------------------
// SendToPane
// ---------------------------------------------------------------------------

func TestSendToPane_Success(t *testing.T) {
	// Verifies sendToPane (the internal begin-turn primitive) sends a user
	// message, sets turn state, and fires the typing callback. Wire-level
	// coverage; routing-level lives in TestInject_*.
	t.Parallel()

	var buf bytes.Buffer
	var typingCalls []bool
	b := &Backend{
		writer:   NewWriter(nopWriteCloser{&buf}),
		readyCh:  make(chan struct{}),
	}
	b.SetTypingFunc(func(v bool) { typingCalls = append(typingCalls, v) })

	handler := &delegator.EventHandler{}
	if err := b.sendToPane(context.Background(), "hello world", handler); err != nil {
		t.Fatalf("sendToPane: %v", err)
	}
	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after sendToPane")
	}
	if len(typingCalls) != 1 || !typingCalls[0] {
		t.Errorf("typingCalls = %v, want [true]", typingCalls)
	}

	// Verify JSON sent on the wire.
	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["type"] != "user" {
		t.Errorf("type = %v, want %q", got["type"], "user")
	}
	msg := got["message"].(map[string]any)
	if msg["content"] != "hello world" {
		t.Errorf("content = %v, want %q", msg["content"], "hello world")
	}
}

func TestSendToPaneWithAttachments(t *testing.T) {
	// Verifies sendToPaneWithAttachments emits structured content blocks
	// (text + image + document) on the wire. Wire-level coverage of the
	// primitive Inject reaches when len(inj.Attachments) > 0 at idle.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:  NewWriter(nopWriteCloser{&buf}),
		readyCh: make(chan struct{}),
	}

	handler := &delegator.EventHandler{}
	atts := []delegator.Attachment{
		{MimeType: "image/jpeg", Data: []byte("fake-jpeg")},
		{MimeType: "application/pdf", Data: []byte("fake-pdf")},
	}
	if err := b.sendToPaneWithAttachments(context.Background(), "describe these", atts, handler); err != nil {
		t.Fatalf("sendToPaneWithAttachments: %v", err)
	}
	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after sendToPaneWithAttachments")
	}

	// Parse the wire message.
	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["type"] != "user" {
		t.Errorf("type = %v, want %q", got["type"], "user")
	}

	// Content should be an array of blocks, not a string.
	msg := got["message"].(map[string]any)
	blocks, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content is not an array: %T", msg["content"])
	}
	if len(blocks) != 3 {
		t.Fatalf("len(blocks) = %d, want 3 (text + image + document)", len(blocks))
	}

	// Block 0: text
	b0 := blocks[0].(map[string]any)
	if b0["type"] != "text" {
		t.Errorf("block[0].type = %v, want %q", b0["type"], "text")
	}
	if b0["text"] != "describe these" {
		t.Errorf("block[0].text = %v, want %q", b0["text"], "describe these")
	}

	// Block 1: image
	b1 := blocks[1].(map[string]any)
	if b1["type"] != "image" {
		t.Errorf("block[1].type = %v, want %q", b1["type"], "image")
	}
	src1 := b1["source"].(map[string]any)
	if src1["type"] != "base64" {
		t.Errorf("block[1].source.type = %v, want %q", src1["type"], "base64")
	}
	if src1["media_type"] != "image/jpeg" {
		t.Errorf("block[1].source.media_type = %v, want %q", src1["media_type"], "image/jpeg")
	}

	// Block 2: document (PDF)
	b2 := blocks[2].(map[string]any)
	if b2["type"] != "document" {
		t.Errorf("block[2].type = %v, want %q", b2["type"], "document")
	}
}

func TestSendToPane_WriterError(t *testing.T) {
	// Verifies sendToPane cancels the turn if the writer fails — the
	// turn must NOT remain in flight on a failed begin-turn, otherwise
	// subsequent Inject calls would route through the follow-up path
	// against a non-existent turn handler.
	t.Parallel()

	b := &Backend{
		writer:  NewWriter(nopWriteCloser{&bytes.Buffer{}}),
		readyCh: make(chan struct{}),
	}
	// Close the writer to force errors.
	b.writer.Close()

	handler := &delegator.EventHandler{}
	if err := b.sendToPane(context.Background(), "hello", handler); err == nil {
		t.Fatal("expected error from closed writer")
	}
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight should be false after send failure")
	}
}

// ---------------------------------------------------------------------------
// OnAssistant
// ---------------------------------------------------------------------------

func TestOnAssistant_TextAccumulation(t *testing.T) {
	// Verifies OnAssistant accumulates text blocks into turnText and fires
	// the handler OnText callback.
	t.Parallel()

	var handlerTexts []string

	b := &Backend{}
	handler := &delegator.EventHandler{
		OnText: func(text string) { handlerTexts = append(handlerTexts, text) },
	}
	b.beginTurn(handler)

	msg := &AssistantMessage{
		Message: BetaMessage{
			Model: "claude-sonnet-4-20250514",
			Content: []ContentBlock{
				{Type: "text", Text: "Hello "},
				{Type: "text", Text: "world!"},
			},
			Usage: TokenUsage{InputTokens: 100, OutputTokens: 20},
		},
	}
	b.OnAssistant(msg)

	b.turnMu.Lock()
	text := b.turnText.String()
	b.turnMu.Unlock()

	if text != "Hello world!" {
		t.Errorf("turnText = %q, want %q", text, "Hello world!")
	}
	if len(handlerTexts) != 2 {
		t.Fatalf("handlerTexts count = %d, want 2", len(handlerTexts))
	}
}

func TestOnAssistant_ToolUseTracking(t *testing.T) {
	// Verifies OnAssistant increments the tool call counter for tool_use blocks
	// and fires the handler's OnToolStart callback.
	t.Parallel()

	var toolStarts []string

	b := &Backend{}
	handler := &delegator.EventHandler{
		OnToolStart: func(_ string, name string, input string) {
			toolStarts = append(toolStarts, name)
		},
	}
	b.beginTurn(handler)

	msg := &AssistantMessage{
		Message: BetaMessage{
			Content: []ContentBlock{
				{Type: "text", Text: "Let me check."},
				{Type: "tool_use", Name: "Read", Input: json.RawMessage(`{"file_path":"/tmp/test"}`)},
				{Type: "tool_use", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
			},
			Usage: TokenUsage{},
		},
	}
	b.OnAssistant(msg)

	b.turnMu.Lock()
	tools := b.turnTools
	b.turnMu.Unlock()

	if tools != 2 {
		t.Errorf("turnTools = %d, want 2", tools)
	}
	if len(toolStarts) != 2 {
		t.Fatalf("toolStarts count = %d, want 2", len(toolStarts))
	}
	if toolStarts[0] != "Read" || toolStarts[1] != "Bash" {
		t.Errorf("toolStarts = %v, want [Read Bash]", toolStarts)
	}
}

func TestOnAssistant_ModelAndUsageExtraction(t *testing.T) {
	// Verifies OnAssistant stores the model and per-call usage from the
	// assistant message for later use in OnResult.
	t.Parallel()

	b := &Backend{}
	b.beginTurn(&delegator.EventHandler{})

	msg := &AssistantMessage{
		Message: BetaMessage{
			Model: "claude-opus-4-20250514",
			Usage: TokenUsage{
				InputTokens:              500,
				OutputTokens:             120,
				CacheReadInputTokens:     300,
				CacheCreationInputTokens: 50,
			},
		},
	}
	b.OnAssistant(msg)

	b.mu.Lock()
	model := b.lastModel
	usage := b.lastUsage
	b.mu.Unlock()

	if model != "claude-opus-4-20250514" {
		t.Errorf("lastModel = %q, want %q", model, "claude-opus-4-20250514")
	}
	if usage == nil {
		t.Fatal("lastUsage is nil")
	}
	if usage.InputTokens != 500 {
		t.Errorf("usage.InputTokens = %d, want 500", usage.InputTokens)
	}
	if usage.OutputTokens != 120 {
		t.Errorf("usage.OutputTokens = %d, want 120", usage.OutputTokens)
	}
	if usage.CacheReadInputTokens != 300 {
		t.Errorf("usage.CacheReadInputTokens = %d, want 300", usage.CacheReadInputTokens)
	}
	if usage.CacheCreationInputTokens != 50 {
		t.Errorf("usage.CacheCreationInputTokens = %d, want 50", usage.CacheCreationInputTokens)
	}
}

func TestOnAssistant_EmptyModel(t *testing.T) {
	// Verifies OnAssistant does not overwrite lastModel when the assistant
	// message has an empty model (e.g. partial streaming messages).
	t.Parallel()

	b := &Backend{}
	b.lastModel = "claude-sonnet-4-20250514"
	b.beginTurn(&delegator.EventHandler{})

	msg := &AssistantMessage{
		Message: BetaMessage{
			Model:   "", // empty — should not overwrite
			Content: []ContentBlock{{Type: "text", Text: "hi"}},
			Usage:   TokenUsage{},
		},
	}
	b.OnAssistant(msg)

	b.mu.Lock()
	model := b.lastModel
	b.mu.Unlock()

	if model != "claude-sonnet-4-20250514" {
		t.Errorf("lastModel = %q, want original %q", model, "claude-sonnet-4-20250514")
	}
}

func TestOnAssistant_TypingRestarted(t *testing.T) {
	// Verifies the typing indicator is restarted when the assistant message's
	// stop_reason is not "end_turn" (i.e. more content is coming).
	t.Parallel()

	var typingCalls []bool
	b := &Backend{}
	b.typingFunc = func(v bool) { typingCalls = append(typingCalls, v) }
	b.beginTurn(&delegator.EventHandler{})

	// No stop_reason — typing should restart.
	msg := &AssistantMessage{
		Message: BetaMessage{
			Content: []ContentBlock{{Type: "text", Text: "thinking..."}},
			Usage:   TokenUsage{},
		},
	}
	b.OnAssistant(msg)

	if len(typingCalls) != 1 || !typingCalls[0] {
		t.Errorf("typingCalls = %v, want [true]", typingCalls)
	}
}

func TestOnAssistant_NoTypingOnEndTurn(t *testing.T) {
	// Verifies the typing indicator is NOT restarted when stop_reason is
	// "end_turn".
	t.Parallel()

	var typingCalls []bool
	b := &Backend{}
	b.typingFunc = func(v bool) { typingCalls = append(typingCalls, v) }
	b.beginTurn(&delegator.EventHandler{})

	endTurn := "end_turn"
	msg := &AssistantMessage{
		Message: BetaMessage{
			Content:    []ContentBlock{{Type: "text", Text: "done"}},
			StopReason: &endTurn,
			Usage:      TokenUsage{},
		},
	}
	b.OnAssistant(msg)

	if len(typingCalls) != 0 {
		t.Errorf("typingCalls = %v, want empty (no restart on end_turn)", typingCalls)
	}
}

func TestOnAssistant_NilHandler(t *testing.T) {
	// Verifies OnAssistant works without a turn handler (e.g. if an assistant
	// message arrives before SendToPane has been called). Should not panic.
	t.Parallel()

	b := &Backend{}

	msg := &AssistantMessage{
		Message: BetaMessage{
			Content: []ContentBlock{
				{Type: "text", Text: "hello"},
			},
			Usage: TokenUsage{InputTokens: 10},
		},
	}
	// Should not panic.
	b.OnAssistant(msg)

	b.mu.Lock()
	usage := b.lastUsage
	b.mu.Unlock()
	if usage == nil {
		t.Fatal("lastUsage should still be set even without handler")
	}
}

func TestOnAssistant_NilCallbacks(t *testing.T) {
	// Verifies OnAssistant doesn't panic when replyFunc and typingFunc are nil.
	t.Parallel()

	b := &Backend{}
	b.beginTurn(&delegator.EventHandler{})

	msg := &AssistantMessage{
		Message: BetaMessage{
			Content: []ContentBlock{
				{Type: "text", Text: "hello"},
			},
			Usage: TokenUsage{},
		},
	}
	// Should not panic even with nil typingFunc and no handler callbacks.
	b.OnAssistant(msg)
}

func TestOnAssistant_ThinkingBlock(t *testing.T) {
	// Verifies thinking blocks are silently ignored (no text accumulation,
	// no callbacks fired).
	t.Parallel()

	var handlerTexts []string
	b := &Backend{}
	b.beginTurn(&delegator.EventHandler{
		OnText: func(text string) { handlerTexts = append(handlerTexts, text) },
	})

	msg := &AssistantMessage{
		Message: BetaMessage{
			Content: []ContentBlock{
				{Type: "thinking", Thinking: "let me think about this..."},
				{Type: "text", Text: "result"},
			},
			Usage: TokenUsage{},
		},
	}
	b.OnAssistant(msg)

	// Only "result" should appear; thinking block should be silent.
	if len(handlerTexts) != 1 || handlerTexts[0] != "result" {
		t.Errorf("handlerTexts = %v, want [result]", handlerTexts)
	}
	b.turnMu.Lock()
	text := b.turnText.String()
	b.turnMu.Unlock()
	if text != "result" {
		t.Errorf("turnText = %q, want %q", text, "result")
	}
}

func TestOnAssistant_AgentToolUseTracking(t *testing.T) {
	// Verifies Agent tool_use blocks are tracked via the shared AgentTracker,
	// mirroring the tmux backend's behavior.
	t.Parallel()

	var statusMessages []string
	b := &Backend{}
	b.SetOnAgentStatus(func(text string) { statusMessages = append(statusMessages, text) })
	b.beginTurn(&delegator.EventHandler{})

	agentInput := json.RawMessage(`{"description":"search for patterns"}`)
	msg := &AssistantMessage{
		Message: BetaMessage{
			Content: []ContentBlock{
				{Type: "tool_use", ID: "ag1", Name: "Agent", Input: agentInput},
			},
			Usage: TokenUsage{},
		},
	}
	b.OnAssistant(msg)

	if b.agents.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1", b.agents.Pending())
	}
	if len(statusMessages) != 1 {
		t.Fatalf("OnStatus called %d times, want 1", len(statusMessages))
	}
	if !strings.Contains(statusMessages[0], "search for patterns") {
		t.Errorf("status = %q, want to contain description", statusMessages[0])
	}
}

func TestOnAssistant_AgentDuplicateIgnored(t *testing.T) {
	// Verifies the same Agent tool_use ID isn't double-counted when
	// --include-partial-messages replays assistant messages.
	t.Parallel()

	b := &Backend{}
	b.SetOnAgentStatus(func(string) {})
	b.beginTurn(&delegator.EventHandler{})

	msg := &AssistantMessage{
		Message: BetaMessage{
			Content: []ContentBlock{
				{Type: "tool_use", ID: "ag1", Name: "Agent", Input: json.RawMessage(`{"description":"task"}`)},
			},
			Usage: TokenUsage{},
		},
	}
	b.OnAssistant(msg)
	b.OnAssistant(msg) // replay

	if b.agents.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1 (duplicate should be ignored)", b.agents.Pending())
	}
}

func TestOnResult_ClearsTrackedAgents(t *testing.T) {
	// Verifies OnResult clears any remaining tracked agents as a safety net.
	t.Parallel()

	var statusMessages []string
	b := &Backend{}
	b.SetOnAgentStatus(func(text string) { statusMessages = append(statusMessages, text) })
	b.agents.Add("ag1", "still running")
	statusMessages = nil // clear Add notification

	b.beginTurn(&delegator.EventHandler{})
	b.OnResult(&ResultMessage{Subtype: "success", Usage: TokenUsage{}})

	if b.agents.Pending() != 0 {
		t.Fatalf("Pending() = %d, want 0 after OnResult", b.agents.Pending())
	}
	if len(statusMessages) != 1 {
		t.Fatalf("OnStatus called %d times, want 1 (completion)", len(statusMessages))
	}
	if !strings.Contains(statusMessages[0], "complete") {
		t.Errorf("status = %q, want completion message", statusMessages[0])
	}
}

// ---------------------------------------------------------------------------
// OnResult
// ---------------------------------------------------------------------------

func TestOnResult_BasicTurnCompletion(t *testing.T) {
	// Verifies OnResult builds a TurnResult from accumulated turn state and
	// fires the handler's OnTurnComplete callback. Also checks that typing
	// is stopped and turnActive is cleared.
	t.Parallel()

	var completedResult *delegator.TurnResult
	var typingCalls []bool

	b := &Backend{}
	b.typingFunc = func(v bool) { typingCalls = append(typingCalls, v) }

	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	b.beginTurn(handler)

	// Simulate assistant message setting model and usage.
	b.mu.Lock()
	b.lastModel = "claude-sonnet-4-20250514"
	b.lastUsage = &TokenUsage{
		InputTokens:  500,
		OutputTokens: 120,
	}
	b.mu.Unlock()

	// Accumulate text and tool count.
	b.turnMu.Lock()
	b.turnText.WriteString("The answer is 42.")
	b.turnTools = 3
	b.turnMu.Unlock()

	result := &ResultMessage{
		Subtype:    "success",
		Result:     "", // empty — should fallback to turnText
		ModelUsage: map[string]ModelUsage{},
	}
	b.OnResult(result)

	if completedResult == nil {
		t.Fatal("OnTurnComplete was not called")
	}
	if completedResult.Text != "The answer is 42." {
		t.Errorf("result.Text = %q, want %q", completedResult.Text, "The answer is 42.")
	}
	if completedResult.ToolCalls != 3 {
		t.Errorf("result.ToolCalls = %d, want 3", completedResult.ToolCalls)
	}
	if completedResult.Model != "claude-sonnet-4-20250514" {
		t.Errorf("result.Model = %q, want %q", completedResult.Model, "claude-sonnet-4-20250514")
	}
	// Per-call usage from lastUsage should be preferred.
	if completedResult.Usage == nil {
		t.Fatal("result.Usage is nil")
	}
	if completedResult.Usage.InputTokens != 500 {
		t.Errorf("usage.InputTokens = %d, want 500", completedResult.Usage.InputTokens)
	}
	if completedResult.Usage.OutputTokens != 120 {
		t.Errorf("usage.OutputTokens = %d, want 120", completedResult.Usage.OutputTokens)
	}

	// Verify typing stopped.
	if len(typingCalls) != 1 || typingCalls[0] {
		t.Errorf("typingCalls = %v, want [false]", typingCalls)
	}
	// Verify turn cleared.
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after OnResult")
	}
}

func TestOnResult_UsesResultTextWhenPresent(t *testing.T) {
	// Verifies OnResult prefers the result message's Result field over
	// accumulated turnText when both are available.
	t.Parallel()

	var completedResult *delegator.TurnResult

	b := &Backend{}
	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	b.beginTurn(handler)

	b.turnMu.Lock()
	b.turnText.WriteString("accumulated text")
	b.turnMu.Unlock()

	result := &ResultMessage{
		Subtype: "success",
		Result:  "final result text",
		Usage:   TokenUsage{InputTokens: 100, OutputTokens: 50},
	}
	b.OnResult(result)

	if completedResult == nil {
		t.Fatal("OnTurnComplete was not called")
	}
	// turnText is preferred over msg.Result — it accumulates all text
	// across multi-segment turns (text → tool → text).
	if completedResult.Text != "accumulated text" {
		t.Errorf("result.Text = %q, want %q (turnText preferred)", completedResult.Text, "accumulated text")
	}
}

// TestOnAssistant_SubagentSurfacesBlockquotedText proves that a subagent
// assistant message (ParentToolUseID != nil) fires OnText with blockquoted
// text so the user can follow sub-agent progress, but does NOT fire
// OnToolStart (the parent tracker owns tool visibility) and does NOT mutate
// the parent's per-turn accumulator state (turnText, turnTools).
func TestOnAssistant_SubagentSurfacesBlockquotedText(t *testing.T) {
	t.Parallel()

	var textEvents []string
	var toolStarts []string

	b := &Backend{}
	handler := &delegator.EventHandler{
		OnText:      func(text string) { textEvents = append(textEvents, text) },
		OnToolStart: func(_, name, _ string) { toolStarts = append(toolStarts, name) },
	}
	b.beginTurn(handler)

	parentID := "toolu_parent"
	b.OnAssistant(&AssistantMessage{
		ParentToolUseID: &parentID,
		Message: BetaMessage{
			Model: "claude-haiku-4-5",
			Content: []ContentBlock{
				{Type: "text", Text: "sub-agent reply"},
				{Type: "tool_use", ID: "tu_nested", Name: "Read", Input: json.RawMessage(`{}`)},
			},
			Usage: TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
	})

	// Text is surfaced as a blockquote.
	if len(textEvents) != 1 || textEvents[0] != "> sub-agent reply" {
		t.Errorf("textEvents = %v, want [> sub-agent reply]", textEvents)
	}
	// Tool calls are NOT forwarded.
	if len(toolStarts) != 0 {
		t.Errorf("OnToolStart fired for subagent: %v", toolStarts)
	}

	// Subagent content must not touch the parent's per-turn state.
	b.turnMu.Lock()
	turnText := b.turnText.String()
	turnTools := b.turnTools
	b.turnMu.Unlock()
	if turnText != "" {
		t.Errorf("turnText mutated by subagent: %q", turnText)
	}
	if turnTools != 0 {
		t.Errorf("turnTools mutated by subagent: %d", turnTools)
	}
}

// TestOnAssistant_SubagentMultilineBlockquote verifies that multiline
// sub-agent text gets every line prefixed with "> ".
func TestOnAssistant_SubagentMultilineBlockquote(t *testing.T) {
	t.Parallel()

	var textEvents []string
	b := &Backend{}
	b.beginTurn(&delegator.EventHandler{
		OnText: func(text string) { textEvents = append(textEvents, text) },
	})

	parentID := "toolu_parent"
	b.OnAssistant(&AssistantMessage{
		ParentToolUseID: &parentID,
		Message: BetaMessage{
			Content: []ContentBlock{
				{Type: "text", Text: "line one\nline two\nline three"},
			},
		},
	})

	want := "> line one\n> line two\n> line three"
	if len(textEvents) != 1 || textEvents[0] != want {
		t.Errorf("textEvents = %v, want [%s]", textEvents, want)
	}
}

// TestOnAssistant_SubagentEmptyTextSkipped verifies that empty sub-agent
// text blocks do not fire OnText.
func TestOnAssistant_SubagentEmptyTextSkipped(t *testing.T) {
	t.Parallel()

	var textEvents []string
	b := &Backend{}
	b.beginTurn(&delegator.EventHandler{
		OnText: func(text string) { textEvents = append(textEvents, text) },
	})

	parentID := "toolu_parent"
	b.OnAssistant(&AssistantMessage{
		ParentToolUseID: &parentID,
		Message: BetaMessage{
			Content: []ContentBlock{
				{Type: "text", Text: ""},
			},
		},
	})

	if len(textEvents) != 0 {
		t.Errorf("OnText fired for empty subagent text: %v", textEvents)
	}
}

func TestOnResult_SubagentDoesNotOverrideModel(t *testing.T) {
	// Verifies that a subagent's assistant message (parent_tool_use_id set)
	// does not overwrite lastModel. The primary model from top-level assistant
	// messages is preserved through to the TurnResult.
	t.Parallel()

	var completedResult *delegator.TurnResult

	b := &Backend{}
	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	b.beginTurn(handler)

	// Top-level assistant message sets the primary model.
	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Model: "claude-opus-4-20250514",
			Usage: TokenUsage{InputTokens: 100, OutputTokens: 50},
		},
		// ParentToolUseID is nil (top-level)
	})

	// Subagent assistant message should NOT override.
	subagentToolID := "toolu_sub123"
	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Model: "claude-haiku-4-5-20251001",
			Usage: TokenUsage{InputTokens: 10, OutputTokens: 5},
		},
		ParentToolUseID: &subagentToolID,
	})

	result := &ResultMessage{
		Subtype: "success",
		Result:  "done",
		ModelUsage: map[string]ModelUsage{
			"claude-opus-4-20250514": {
				ContextWindow: 200000,
			},
			"claude-haiku-4-5-20251001": {
				ContextWindow: 200000,
			},
		},
		Usage: TokenUsage{InputTokens: 110, OutputTokens: 55},
	}
	b.OnResult(result)

	if completedResult == nil {
		t.Fatal("OnTurnComplete was not called")
	}
	if completedResult.Model != "claude-opus-4-20250514" {
		t.Errorf("result.Model = %q, want %q", completedResult.Model, "claude-opus-4-20250514")
	}
	// Context window should match the primary model.
	b.mu.Lock()
	cw := b.contextWindow
	b.mu.Unlock()
	if cw != 200000 {
		t.Errorf("contextWindow = %d, want 200000", cw)
	}
}

func TestOnResult_FallbackToResultUsage(t *testing.T) {
	// Verifies OnResult uses the result's cumulative usage when no per-call
	// usage is available (lastUsage is nil).
	t.Parallel()

	var completedResult *delegator.TurnResult

	b := &Backend{}
	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	b.beginTurn(handler)
	// Ensure lastUsage is nil (beginTurn already resets it).

	result := &ResultMessage{
		Subtype: "success",
		Result:  "done",
		Usage: TokenUsage{
			InputTokens:              1000,
			OutputTokens:             200,
			CacheReadInputTokens:     400,
			CacheCreationInputTokens: 100,
		},
	}
	b.OnResult(result)

	if completedResult == nil {
		t.Fatal("OnTurnComplete was not called")
	}
	if completedResult.Usage == nil {
		t.Fatal("usage is nil")
	}
	if completedResult.Usage.InputTokens != 1000 {
		t.Errorf("usage.InputTokens = %d, want 1000", completedResult.Usage.InputTokens)
	}
	if completedResult.Usage.OutputTokens != 200 {
		t.Errorf("usage.OutputTokens = %d, want 200", completedResult.Usage.OutputTokens)
	}
	if completedResult.Usage.CacheReadInputTokens != 400 {
		t.Errorf("usage.CacheReadInputTokens = %d, want 400", completedResult.Usage.CacheReadInputTokens)
	}
	if completedResult.Usage.CacheCreationInputTokens != 100 {
		t.Errorf("usage.CacheCreationInputTokens = %d, want 100", completedResult.Usage.CacheCreationInputTokens)
	}
}

func TestOnResult_SignalsWaitForTurn(t *testing.T) {
	// Verifies OnResult pushes to turnResultCh so that WaitForTurn unblocks.
	t.Parallel()

	b := &Backend{}
	handler := &delegator.EventHandler{}
	b.beginTurn(handler)

	// Start WaitForTurn in a goroutine.
	done := make(chan error, 1)
	go func() {
		done <- b.WaitForTurn(context.Background())
	}()

	// Give the goroutine a moment to start waiting.
	time.Sleep(10 * time.Millisecond)

	result := &ResultMessage{
		Subtype: "success",
		Result:  "done",
		Usage:   TokenUsage{},
	}
	b.OnResult(result)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForTurn: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForTurn did not unblock after OnResult")
	}
}

func TestOnResult_NilHandler(t *testing.T) {
	// Verifies OnResult doesn't panic when no handler is set.
	t.Parallel()

	b := &Backend{}
	// No beginTurn — handler is nil.

	result := &ResultMessage{
		Subtype: "success",
		Result:  "orphan result",
		Usage:   TokenUsage{},
	}
	// Should not panic.
	b.OnResult(result)
}

func TestOnResult_NilResultCh(t *testing.T) {
	// Verifies OnResult doesn't panic when turnResultCh is nil.
	t.Parallel()

	var completedResult *delegator.TurnResult
	b := &Backend{}
	b.turnMu.Lock()
	b.turnActive = true
	b.turnHandler = &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	b.turnMu.Unlock()
	// turnResultCh is nil — should still complete without panic.

	result := &ResultMessage{
		Subtype: "success",
		Result:  "done",
		Usage:   TokenUsage{},
	}
	b.OnResult(result)

	if completedResult == nil {
		t.Fatal("OnTurnComplete was not called")
	}
}

func TestOnResult_ClearsLastUsage(t *testing.T) {
	// Verifies that OnResult resets lastUsage to nil for the next turn.
	t.Parallel()

	b := &Backend{}
	handler := &delegator.EventHandler{}
	b.beginTurn(handler)

	// Set lastUsage as if from an assistant message.
	b.mu.Lock()
	b.lastUsage = &TokenUsage{InputTokens: 500}
	b.mu.Unlock()

	result := &ResultMessage{
		Subtype: "success",
		Result:  "done",
		Usage:   TokenUsage{},
	}
	b.OnResult(result)

	b.mu.Lock()
	if b.lastUsage != nil {
		t.Error("lastUsage should be nil after OnResult")
	}
	b.mu.Unlock()
}

func TestOnResult_PreAnswerReDispatches(t *testing.T) {
	// Verifies that when PreAnswerNudgeFunc returns a non-empty follow-up on
	// the first OnResult, ccstream re-arms the same handler, sends the
	// follow-up via the writer with NO priority (plain user message), and
	// SKIPS firing OnTurnComplete. A second OnResult then fires
	// OnTurnComplete with the revised result. This is the delegated
	// equivalent of the API transport's pre_answer verification loop.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
	}
	b.typingFunc = func(bool) {}

	var completedCount int
	var completedResult *delegator.TurnResult
	var preAnswerCalls int
	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) {
			completedCount++
			completedResult = r
		},
		PreAnswerNudgeFunc: func(_ *delegator.TurnResult) string {
			preAnswerCalls++
			if preAnswerCalls == 1 {
				return "verify your answer"
			}
			return ""
		},
	}
	b.beginTurn(handler)

	b.turnMu.Lock()
	b.turnText.WriteString("original")
	b.turnMu.Unlock()

	// Round 1: pre_answer fires — should re-arm instead of completing.
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})

	if completedCount != 0 {
		t.Errorf("OnTurnComplete should not fire on round 1 (pre_answer re-dispatched); fired %d times", completedCount)
	}
	if preAnswerCalls != 1 {
		t.Errorf("PreAnswerNudgeFunc should have fired once, got %d", preAnswerCalls)
	}
	if !strings.Contains(buf.String(), "verify your answer") {
		t.Errorf("writer should contain follow-up prompt, got: %q", buf.String())
	}
	if !b.IsTurnInFlight() {
		t.Error("turn should be re-armed and in flight after pre_answer re-dispatch")
	}

	// Simulate round 2 text and result.
	b.turnMu.Lock()
	b.turnText.WriteString("revised")
	b.turnMu.Unlock()
	b.OnResult(&ResultMessage{Subtype: "success", Result: "", ModelUsage: map[string]ModelUsage{}})

	if completedCount != 1 {
		t.Errorf("OnTurnComplete should fire once after round 2, got %d", completedCount)
	}
	if completedResult == nil || completedResult.Text != "revised" {
		t.Errorf("final result.Text = %v, want %q", completedResult, "revised")
	}
	if b.IsTurnInFlight() {
		t.Error("turn should be completed after round 2")
	}
}

func TestOnResult_PreAnswerEmptyReturnCompletesNormally(t *testing.T) {
	// Verifies that when PreAnswerNudgeFunc returns "" (gate not firing),
	// OnResult falls through to the standard completion path without any
	// writer traffic. Matches the no-nudger case from the caller's side.
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
	}
	b.typingFunc = func(bool) {}

	var completed *delegator.TurnResult
	handler := &delegator.EventHandler{
		OnTurnComplete:     func(r *delegator.TurnResult) { completed = r },
		PreAnswerNudgeFunc: func(_ *delegator.TurnResult) string { return "" },
	}
	b.beginTurn(handler)
	b.turnMu.Lock()
	b.turnText.WriteString("final answer")
	b.turnMu.Unlock()

	b.OnResult(&ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}})

	if completed == nil {
		t.Fatal("OnTurnComplete should fire when pre_answer returns empty")
	}
	if completed.Text != "final answer" {
		t.Errorf("result.Text = %q, want %q", completed.Text, "final answer")
	}
	if buf.Len() != 0 {
		t.Errorf("writer should stay empty when pre_answer returns empty, got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Re-arm cascade — behavior tests
//
// These tests verify foci's "complete-and-rearm" contract: when an in-flight
// turn ends but a queued user-role injection (post-tool nudge, mid-turn
// steer, follow-up) needs the next round of CC output delivered to the
// original handler.
//
// The contract is asserted purely through observable behavior — no direct
// reads or writes of internal flag fields. State is set up by invoking the
// production trigger paths (handleHookResponse for nudge, checkAndSendSteers
// for steer, SendCommand priority="next" for follow-up). These tests are
// designed to survive an internal refactor that collapses the three flags
// (nudgePending / steerInjected / followUpQueued) into a single re-arm-reason
// representation, since they only depend on observable behavior:
//
//   - the original handler's OnTurnComplete fires exactly once,
//   - subsequent OnAssistant text reaches the original OnText,
//   - the second OnResult does NOT fire OnTurnComplete again,
//   - after the second OnResult, IsTurnInFlight() returns false,
//   - text arriving after the cycle ends is not delivered (handler cleared),
//   - a fresh beginTurn supersedes any pending re-arm reason.
// ---------------------------------------------------------------------------

// rearmTrigger is one of the production paths that sets up a pending
// re-arm. arm() fires the trigger; it must leave the backend in a state
// where the next OnResult takes the re-arm path. handler is the
// already-armed turn handler — arm() may install fields on it (e.g.
// PostToolNudgeFunc) that the production trigger reads.
//
// Post-Phase 5 there are two trigger shapes:
//   - nudge: handleHookResponse fires PostToolNudgeFunc, which sends a
//     plain user message that auto-arms the rearm cascade.
//   - urgent: SendCommand on an in-flight turn (covers both the follow-up
//     path in turn_delegated.go and the dispatchUrgent path that pairs
//     SendCommand with Interrupt).
//
// The third pre-Phase-5 path ("steer" via checkAndSendSteers) is gone —
// its responsibilities moved to platform.MessageQueue.dispatchUrgent which
// invokes the same Interrupt+SendCommand sequence as the urgent trigger.
type rearmTrigger struct {
	name string
	arm  func(t *testing.T, b *Backend, handler *delegator.EventHandler)
}

func rearmTriggers() []rearmTrigger {
	return []rearmTrigger{
		{
			name: "nudge",
			arm: func(t *testing.T, b *Backend, handler *delegator.EventHandler) {
				t.Helper()
				// handleHookResponse requires:
				//   - a matching hookInstallID,
				//   - handler.OnToolEnd non-nil (early return otherwise),
				//   - handler.PostToolNudgeFunc returning text — that's
				//     what marks the turn for re-arm.
				const installID = "test-install"
				b.mu.Lock()
				b.hookInstallID = installID
				b.mu.Unlock()
				if handler.OnToolEnd == nil {
					handler.OnToolEnd = func(_, _, _ string, _ bool) {}
				}
				handler.PostToolNudgeFunc = func(_ string, _ bool) []string {
					return []string{"nudge-text"}
				}
				stdout, err := json.Marshal(hookScriptOutput{
					HookEvent:    eventPostToolUse,
					InstallID:    installID,
					ToolUseID:    "toolu_test",
					ToolName:     "Bash",
					ToolResponse: "ok",
				})
				if err != nil {
					t.Fatalf("marshal hookScriptOutput: %v", err)
				}
				env, err := json.Marshal(hookResponseEnvelope{
					HookEvent: eventPostToolUse,
					Stdout:    string(stdout),
				})
				if err != nil {
					t.Fatalf("marshal hookResponseEnvelope: %v", err)
				}
				b.handleHookResponse(env)
			},
		},
		{
			name: "urgent",
			arm: func(t *testing.T, b *Backend, _ *delegator.EventHandler) {
				t.Helper()
				// Use the internal primitive directly — the rearm trigger
				// test owns the in-flight assumption (caller called
				// beginTurn first), so we exercise the same code path
				// Inject(SourceUser) takes mid-turn without bringing the
				// full Inject routing into the test.
				if err := b.sendUserMessage(context.Background(), "urgent text", true); err != nil {
					t.Fatalf("sendUserMessage: %v", err)
				}
			},
		},
	}
}

// newRearmBackend builds a backend wired up with the writer and typing func
// that the trigger paths and OnResult cascade exercise. Caller still calls
// beginTurn before triggering.
func newRearmBackend() *Backend {
	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	b.typingFunc = func(bool) {}
	return b
}

// emitText fires an OnAssistant event with a single top-level text block.
// Used to (a) accumulate turnText pre-trigger and (b) verify post-rearm
// delivery via OnText.
func emitText(b *Backend, text string) {
	b.OnAssistant(&AssistantMessage{
		Message: BetaMessage{
			Content: []ContentBlock{{Type: "text", Text: text}},
		},
	})
}

// successResult is a minimal CC result message used to drive turn completion
// in the cascade tests. ModelUsage must be a non-nil map (OnResult iterates).
func successResult() *ResultMessage {
	return &ResultMessage{Subtype: "success", ModelUsage: map[string]ModelUsage{}}
}

// TestRearm_FullCycle exercises the complete-and-rearm contract end-to-end
// for every trigger path. Replaces three near-identical RearmsHandler /
// DeliveryAfterRearm / SecondResultCleansUp test families with a single
// parameterised assertion of observable behavior.
func TestRearm_FullCycle(t *testing.T) {
	for _, tc := range rearmTriggers() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var completes []*delegator.TurnResult
			var textCalls []string
			handler := &delegator.EventHandler{
				OnTurnComplete: func(r *delegator.TurnResult) { completes = append(completes, r) },
				OnText:         func(text string) { textCalls = append(textCalls, text) },
				OnTextDelta:    func(string) {},
			}

			b := newRearmBackend()
			b.beginTurn(handler)
			emitText(b, "before injection")
			tc.arm(t, b, handler)

			// 1) First OnResult: original turn completes, handler re-arms.
			b.OnResult(successResult())

			if len(completes) != 1 {
				t.Fatalf("OnTurnComplete fired %d times after first OnResult, want 1", len(completes))
			}
			if !strings.Contains(completes[0].Text, "before injection") {
				t.Errorf("result.Text = %q, want it to include accumulated turn text", completes[0].Text)
			}
			if !b.IsTurnInFlight() {
				t.Error("IsTurnInFlight = false after re-arm; expected handler to remain armed")
			}

			// 2) Subsequent assistant text reaches OnText through the
			//    re-armed handler.
			emitText(b, "queued response text")

			if !slices.Contains(textCalls, "queued response text") {
				t.Errorf("textCalls = %v, want to include %q (handler not re-armed for delivery)",
					textCalls, "queued response text")
			}

			// 3) Second OnResult: re-armed turn ends. OnTurnComplete must
			//    NOT double-fire (re-armed handler has nil OnTurnComplete
			//    by contract, so the result cycle ends silently).
			b.OnResult(successResult())

			if len(completes) != 1 {
				t.Errorf("OnTurnComplete fired %d times overall, want 1 (re-armed handler must not double-fire)",
					len(completes))
			}
			if b.IsTurnInFlight() {
				t.Error("IsTurnInFlight = true after second OnResult; expected normal cleanup")
			}

			// 4) Late text after the cycle ends is not delivered — the
			//    handler is cleared.
			before := len(textCalls)
			emitText(b, "post-end text")
			if len(textCalls) != before {
				t.Errorf("text after second OnResult was delivered (%d new calls); handler should be cleared",
					len(textCalls)-before)
			}
		})
	}
}

// TestRearm_BeginTurnClearsStaleState verifies that a fresh user turn
// supersedes any pending re-arm reason: after the trigger fires, beginTurn
// for a new handler must reset the state so the next OnResult is a normal
// turn end (handler2's OnTurnComplete fires, no spurious re-arm).
//
// Replaces TestBeginTurn_Clears{Nudge,Steer,FollowUp} with one
// parameterised behavior assertion.
func TestRearm_BeginTurnClearsStaleState(t *testing.T) {
	for _, tc := range rearmTriggers() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handler1 := &delegator.EventHandler{
				OnTurnComplete: func(*delegator.TurnResult) {
					t.Error("handler1.OnTurnComplete must not fire — superseded by beginTurn")
				},
				OnText: func(string) {},
			}

			b := newRearmBackend()
			b.beginTurn(handler1)
			tc.arm(t, b, handler1)

			// Fresh turn supersedes handler1 before any OnResult arrives.
			var completes []*delegator.TurnResult
			handler2 := &delegator.EventHandler{
				OnTurnComplete: func(r *delegator.TurnResult) { completes = append(completes, r) },
				OnText:         func(string) {},
			}
			b.beginTurn(handler2)

			// OnResult must take the normal-completion path: handler2's
			// OnTurnComplete fires, turn ends, no re-arm.
			b.OnResult(successResult())

			if len(completes) != 1 {
				t.Fatalf("handler2.OnTurnComplete fired %d times, want 1 (stale re-arm reason leaked)", len(completes))
			}
			if b.IsTurnInFlight() {
				t.Error("IsTurnInFlight = true after normal-completion OnResult on fresh turn")
			}
		})
	}
}

// TestRearm_IdleSendCommandEndsNormally proves SendCommand at idle (no turn
// in flight, e.g. the /compact-at-turn-end path) does NOT set a rearm reason,
// so OnResult takes the normal-completion path. Inverse of TestRearm_FullCycle.
//
// The rearm flag is only set when a turn is in flight at SendCommand time;
// idle commands have no original handler to re-arm for.
func TestRearm_IdleSendCommandEndsNormally(t *testing.T) {
	t.Parallel()

	var completes []*delegator.TurnResult
	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completes = append(completes, r) },
		OnText:         func(string) {},
	}
	b := newRearmBackend()
	b.beginTurn(handler)

	// Simulate an idle slash-command (Inject(SourceCompact)) by clearing
	// turnActive before the call. The Compact path passes autoRearm=false
	// so even if the slot were armable, no rearm would fire.
	b.turnMu.Lock()
	b.turnActive = false
	b.turnMu.Unlock()
	if err := b.Inject(context.Background(), delegator.Inject{Source: delegator.SourceCompact, Text: "/compact"}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// Restore turnActive so OnResult sees the turn handler the caller
	// installed via beginTurn.
	b.turnMu.Lock()
	b.turnActive = true
	b.turnMu.Unlock()

	b.OnResult(successResult())

	if len(completes) != 1 {
		t.Fatalf("OnTurnComplete fired %d times, want 1 (idle slash command should not re-arm)", len(completes))
	}
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after normal completion")
	}
}

// TestRearm_CounterStacks verifies pendingRearmCount accumulates across
// multiple incRearm calls within a single in-flight turn. This is the
// counter's reason for existing: pre-counter, two stacked events on the
// same turn would race against handler clear and the second response
// dropped silently. The counter records every queued event, and OnResult
// drains them one delivery cycle at a time.
func TestRearm_CounterStacks(t *testing.T) {
	t.Parallel()

	b := newRearmBackend()
	b.beginTurn(&delegator.EventHandler{})

	b.turnMu.Lock()
	if got := b.pendingRearmCount; got != 0 {
		t.Errorf("pendingRearmCount after beginTurn = %d, want 0", got)
	}
	b.incRearm()
	b.incRearm()
	b.incRearm()
	got := b.pendingRearmCount
	b.turnMu.Unlock()

	if got != 3 {
		t.Errorf("pendingRearmCount after three incRearm calls = %d, want 3", got)
	}
}

// TestRearm_DecRearmClampsAtZero proves decRearm doesn't underflow when
// called on an empty counter — a defence against ordering bugs where a
// rollback path might decrement after another path already drained the
// slot. Returns the count BEFORE decrement so callers can branch.
func TestRearm_DecRearmClampsAtZero(t *testing.T) {
	t.Parallel()

	b := newRearmBackend()
	b.beginTurn(&delegator.EventHandler{})

	b.turnMu.Lock()
	prev := b.decRearm()
	count := b.pendingRearmCount
	b.turnMu.Unlock()

	if prev != 0 {
		t.Errorf("decRearm on empty returned %d, want 0", prev)
	}
	if count != 0 {
		t.Errorf("pendingRearmCount after decRearm on empty = %d, want 0 (clamped)", count)
	}
}

// TestRearm_StackedEventsBothDeliver is the integration test for the
// counter fix. Two user-role events stacked within a single in-flight
// turn (e.g. two post-tool nudges, or a nudge plus a follow-up
// SendCommand) must each get their own delivery cycle through the
// re-armed handler. Pre-counter, the second event's response was
// silently dropped because pendingRearmReason cleared on the first
// rearm cycle and the second OnResult took the normal-completion path
// (clearing the handler) before CC's response arrived.
func TestRearm_StackedEventsBothDeliver(t *testing.T) {
	t.Parallel()

	var (
		completes []*delegator.TurnResult
		textCalls []string
	)
	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completes = append(completes, r) },
		OnText:         func(text string) { textCalls = append(textCalls, text) },
		OnTextDelta:    func(string) {},
	}
	b := newRearmBackend()
	b.beginTurn(handler)
	emitText(b, "before injection")

	// Stack two pending rearm signals (e.g. two post-tool nudges fired
	// within one in-flight CC turn).
	b.turnMu.Lock()
	b.incRearm()
	b.incRearm()
	b.turnMu.Unlock()

	// Cycle 1: original turn boundary. OnTurnComplete fires once, count
	// drops 2 → 1, delivery handler installed for first queued response.
	b.OnResult(successResult())
	emitText(b, "queued response 1")

	if len(completes) != 1 {
		t.Fatalf("OnTurnComplete after first OnResult fired %d times, want 1", len(completes))
	}
	if !slices.Contains(textCalls, "queued response 1") {
		t.Errorf("queued response 1 missing from textCalls: %v", textCalls)
	}
	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after first OnResult; expected handler to remain armed for second event")
	}

	// Cycle 2: second queued response's boundary. Count drops 1 → 0,
	// delivery handler re-installed (with nil OnTurnComplete by contract).
	b.OnResult(successResult())
	emitText(b, "queued response 2")

	if len(completes) != 1 {
		t.Errorf("OnTurnComplete after second OnResult fired %d times overall, want 1 (re-armed handler must not double-fire)", len(completes))
	}
	if !slices.Contains(textCalls, "queued response 2") {
		t.Errorf("queued response 2 missing from textCalls: %v (the bug: pre-counter the second response was dropped here)", textCalls)
	}
	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after second OnResult; expected the third (idle) cycle still pending")
	}

	// Cycle 3: count is 0, normal completion. Handler clears.
	b.OnResult(successResult())

	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after third OnResult; expected normal cleanup")
	}
}

// ---------------------------------------------------------------------------
// OnSystem
// ---------------------------------------------------------------------------

func TestOnSystem_Init(t *testing.T) {
	// Verifies OnSystem/init sets sessionID, model, closes readyCh, and fires
	// onSessionReady.
	t.Parallel()

	var readySessID string
	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}
	b.SetOnSessionReady(func(id string) { readySessID = id })

	raw := json.RawMessage(`{
		"type": "system",
		"subtype": "init",
		"claude_code_version": "1.0.27",
		"cwd": "/tmp",
		"model": "claude-sonnet-4-20250514",
		"permissionMode": "default",
		"tools": ["Bash"],
		"session_id": "sess-init-001"
	}`)

	b.OnSystem("init", raw)

	if b.SessionID() != "sess-init-001" {
		t.Errorf("SessionID = %q, want %q", b.SessionID(), "sess-init-001")
	}
	b.mu.Lock()
	model := b.lastModel
	initMsg := b.initMsg
	b.mu.Unlock()
	if model != "claude-sonnet-4-20250514" {
		t.Errorf("lastModel = %q, want %q", model, "claude-sonnet-4-20250514")
	}
	if initMsg == nil {
		t.Error("initMsg is nil")
	}
	if readySessID != "sess-init-001" {
		t.Errorf("onSessionReady got %q, want %q", readySessID, "sess-init-001")
	}

	// readyCh should be closed.
	select {
	case <-b.readyCh:
		// OK.
	default:
		t.Error("readyCh not closed after init")
	}
}

func TestOnSystem_InitIdempotent(t *testing.T) {
	// Verifies OnSystem/init closes readyCh only once (readyOnce prevents
	// double-close panic).
	t.Parallel()

	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}

	raw := json.RawMessage(`{
		"type": "system", "subtype": "init",
		"claude_code_version": "1.0", "cwd": "/tmp",
		"model": "test", "permissionMode": "default", "tools": [],
		"session_id": "sess-1"
	}`)

	b.OnSystem("init", raw)
	// Second call should not panic on double-close.
	b.OnSystem("init", raw)

	select {
	case <-b.readyCh:
	default:
		t.Error("readyCh not closed")
	}
}

func TestOnSystem_InitBadJSON(t *testing.T) {
	// Verifies OnSystem/init silently ignores malformed JSON.
	t.Parallel()

	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}

	b.OnSystem("init", json.RawMessage(`{invalid json`))

	if b.SessionID() != "" {
		t.Error("SessionID should be empty after bad JSON")
	}
	// readyCh should NOT be closed.
	select {
	case <-b.readyCh:
		t.Error("readyCh should not be closed after bad JSON")
	default:
	}
}

func TestOnSystem_StatusCompacting(t *testing.T) {
	// Verifies OnSystem/status with status="compacting" fires
	// onCompactionStart.
	t.Parallel()

	var compStartCalled bool
	b := &Backend{}
	b.SetOnCompactionStart(func() { compStartCalled = true })

	status := "compacting"
	raw, _ := json.Marshal(StatusMessage{Status: &status})
	b.OnSystem("status", raw)

	if !compStartCalled {
		t.Error("onCompactionStart was not called")
	}
}

func TestOnSystem_StatusNonCompacting(t *testing.T) {
	// Verifies OnSystem/status with a non-compacting status does NOT fire
	// onCompactionStart.
	t.Parallel()

	var compStartCalled bool
	b := &Backend{}
	b.SetOnCompactionStart(func() { compStartCalled = true })

	other := "idle"
	raw, _ := json.Marshal(StatusMessage{Status: &other})
	b.OnSystem("status", raw)

	if compStartCalled {
		t.Error("onCompactionStart should not be called for non-compacting status")
	}
}

func TestOnSystem_StatusNilStatus(t *testing.T) {
	// Verifies OnSystem/status with nil status does NOT fire onCompactionStart.
	t.Parallel()

	var compStartCalled bool
	b := &Backend{}
	b.SetOnCompactionStart(func() { compStartCalled = true })

	raw, _ := json.Marshal(StatusMessage{Status: nil})
	b.OnSystem("status", raw)

	if compStartCalled {
		t.Error("onCompactionStart should not be called for nil status")
	}
}

func TestOnSystem_CompactBoundary(t *testing.T) {
	// Verifies OnSystem/compact_boundary fires onCompactionDone with the
	// correct preTokens value.
	t.Parallel()

	var gotTokens int
	b := &Backend{}
	b.SetOnCompactionDone(func(preTokens int) { gotTokens = preTokens })

	raw, _ := json.Marshal(CompactBoundaryMessage{
		CompactMetadata: CompactMetadata{
			Trigger:   "auto",
			PreTokens: 150000,
		},
	})
	b.OnSystem("compact_boundary", raw)

	if gotTokens != 150000 {
		t.Errorf("preTokens = %d, want 150000", gotTokens)
	}
}

func TestOnSystem_CompactBoundaryBadJSON(t *testing.T) {
	// Verifies OnSystem/compact_boundary silently ignores bad JSON.
	t.Parallel()

	var called bool
	b := &Backend{}
	b.SetOnCompactionDone(func(int) { called = true })

	b.OnSystem("compact_boundary", json.RawMessage(`{bad json`))

	if called {
		t.Error("onCompactionDone should not be called on bad JSON")
	}
}

func TestArmCompactionWait_SignaledByCompactBoundary(t *testing.T) {
	// Verifies that ArmCompactionWait + WaitForCompaction blocks until
	// compact_boundary is received.
	t.Parallel()

	b := &Backend{}
	b.ArmCompactionWait()

	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		done <- b.WaitForCompaction(ctx)
	}()

	// Give the goroutine time to block.
	time.Sleep(10 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("WaitForCompaction returned before compact_boundary")
	default:
	}

	// Fire compact_boundary.
	raw, _ := json.Marshal(CompactBoundaryMessage{
		CompactMetadata: CompactMetadata{PreTokens: 100000},
	})
	b.OnSystem("compact_boundary", raw)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForCompaction: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForCompaction did not return after compact_boundary")
	}
}

func TestArmCompactionWait_ContextCancellation(t *testing.T) {
	// Verifies WaitForCompaction respects context cancellation.
	t.Parallel()

	b := &Backend{}
	b.ArmCompactionWait()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.WaitForCompaction(ctx) }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("WaitForCompaction err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForCompaction did not return after context cancel")
	}
}

func TestWaitForCompaction_NoArm(t *testing.T) {
	// Verifies WaitForCompaction returns immediately if not armed.
	t.Parallel()

	b := &Backend{}
	if err := b.WaitForCompaction(context.Background()); err != nil {
		t.Fatalf("WaitForCompaction (unarmed): %v", err)
	}
}

func TestArmCompactionWait_OneShot(t *testing.T) {
	// Verifies compactDoneCh is cleared after compact_boundary fires,
	// so a second call to WaitForCompaction returns immediately.
	t.Parallel()

	b := &Backend{}
	b.ArmCompactionWait()

	raw, _ := json.Marshal(CompactBoundaryMessage{
		CompactMetadata: CompactMetadata{PreTokens: 50000},
	})
	b.OnSystem("compact_boundary", raw)

	// First call should unblock.
	ctx := context.Background()
	if err := b.WaitForCompaction(ctx); err != nil {
		t.Fatalf("WaitForCompaction (first): %v", err)
	}

	// Second call without re-arming: channel is nil, returns immediately.
	if err := b.WaitForCompaction(ctx); err != nil {
		t.Fatalf("WaitForCompaction (second, unarmed): %v", err)
	}
}

func TestOnSystem_TaskStarted(t *testing.T) {
	// task_started is a no-op — agent tracking happens in OnAssistant via
	// tool_use detection. Verify no status is emitted.
	t.Parallel()

	var called bool
	b := &Backend{}
	b.SetOnAgentStatus(func(string) { called = true })

	raw, _ := json.Marshal(TaskEvent{
		Subtype:     "task_started",
		Description: "Fixing the bug",
	})
	b.OnSystem("task_started", raw)

	if called {
		t.Error("OnStatus should not be called for task_started (tracking is via tool_use)")
	}
}

func TestOnSystem_TaskNotificationCompleted(t *testing.T) {
	// With no tracked agents, task_notification (completed) fires a fallback
	// message containing the summary.
	t.Parallel()

	var statusText string
	b := &Backend{}
	b.SetOnAgentStatus(func(text string) { statusText = text })

	raw, _ := json.Marshal(TaskEvent{
		Subtype: "task_notification",
		Status:  "completed",
		Summary: "Bug is fixed",
	})
	b.OnSystem("task_notification", raw)

	if !strings.Contains(statusText, "Bug is fixed") {
		t.Errorf("statusText = %q, want to contain %q", statusText, "Bug is fixed")
	}
}

func TestOnSystem_TaskNotificationCompleted_WithTracked(t *testing.T) {
	// With a tracked agent, task_notification (completed) removes one from
	// the tracker and fires the aggregated completion message.
	t.Parallel()

	var statusText string
	b := &Backend{}
	b.SetOnAgentStatus(func(text string) { statusText = text })
	b.agents.Add("ag1", "fix bug")

	raw, _ := json.Marshal(TaskEvent{
		Subtype: "task_notification",
		Status:  "completed",
		Summary: "Bug is fixed",
	})
	b.OnSystem("task_notification", raw)

	if b.agents.Pending() != 0 {
		t.Errorf("Pending() = %d, want 0", b.agents.Pending())
	}
	if !strings.Contains(statusText, "complete") {
		t.Errorf("statusText = %q, want completion message", statusText)
	}
}

func TestOnSystem_TaskProgress(t *testing.T) {
	// Verifies OnSystem/task_progress does NOT fire OnStatus.
	t.Parallel()

	var called bool
	b := &Backend{}
	b.SetOnAgentStatus(func(string) { called = true })

	raw, _ := json.Marshal(TaskEvent{
		Subtype:     "task_progress",
		Description: "Still working",
	})
	b.OnSystem("task_progress", raw)

	if called {
		t.Error("OnStatus should not be called for task_progress")
	}
}

func TestOnSystem_UnknownSubtype(t *testing.T) {
	// Verifies OnSystem silently ignores unknown subtypes.
	t.Parallel()

	b := &Backend{}
	// Should not panic.
	b.OnSystem("unknown_future_subtype", json.RawMessage(`{"data":"whatever"}`))
}

func TestOnSystem_NilCallbacks(t *testing.T) {
	// Verifies OnSystem doesn't panic when all callbacks are nil.
	t.Parallel()

	b := &Backend{
		readyCh: make(chan struct{}),
	}

	// Init without callbacks.
	raw := json.RawMessage(`{
		"type": "system", "subtype": "init",
		"claude_code_version": "1.0", "cwd": "/tmp",
		"model": "test", "permissionMode": "default", "tools": [],
		"session_id": "s1"
	}`)
	b.OnSystem("init", raw) // onSessionReady is nil — should not panic.

	// Status without callback.
	status := "compacting"
	sRaw, _ := json.Marshal(StatusMessage{Status: &status})
	b.OnSystem("status", sRaw) // onCompactionStart is nil — should not panic.
}

// ---------------------------------------------------------------------------
// OnReaderStopped
// ---------------------------------------------------------------------------

func TestOnReaderStopped_ClearsTurnState(t *testing.T) {
	// Verifies OnReaderStopped marks the backend as not running, fires
	// OnTurnComplete with an error message, stops typing, and unblocks
	// WaitForTurn.
	t.Parallel()

	var completedResult *delegator.TurnResult
	var typingCalls []bool

	b := &Backend{}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()
	b.typingFunc = func(v bool) { typingCalls = append(typingCalls, v) }

	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	b.beginTurn(handler)

	testErr := fmt.Errorf("pipe broken")
	b.OnReaderStopped(testErr)

	// Running should be false.
	if b.IsRunning() {
		t.Error("IsRunning = true after OnReaderStopped")
	}
	// Turn should be cleared.
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after OnReaderStopped")
	}
	// OnTurnComplete should have been called with error info.
	if completedResult == nil {
		t.Fatal("OnTurnComplete was not called")
	}
	if !strings.Contains(completedResult.Text, "pipe broken") {
		t.Errorf("result.Text = %q, want to contain %q", completedResult.Text, "pipe broken")
	}
	// Typing should be stopped.
	if len(typingCalls) != 1 || typingCalls[0] {
		t.Errorf("typingCalls = %v, want [false]", typingCalls)
	}
}

func TestOnReaderStopped_UnblocksWaitForTurn(t *testing.T) {
	// Verifies that OnReaderStopped pushes to turnResultCh so WaitForTurn
	// unblocks.
	t.Parallel()

	b := &Backend{}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()
	handler := &delegator.EventHandler{}
	b.beginTurn(handler)

	done := make(chan error, 1)
	go func() {
		done <- b.WaitForTurn(context.Background())
	}()

	time.Sleep(10 * time.Millisecond)
	b.OnReaderStopped(fmt.Errorf("crash"))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForTurn: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForTurn did not unblock after OnReaderStopped")
	}
}

func TestOnReaderStopped_NoTurnInFlight(t *testing.T) {
	// Verifies OnReaderStopped handles the case where no turn is in flight
	// without panicking.
	t.Parallel()

	b := &Backend{}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	// Should not panic even with no turn.
	b.OnReaderStopped(fmt.Errorf("unexpected EOF"))

	if b.IsRunning() {
		t.Error("IsRunning should be false after OnReaderStopped")
	}
}

func TestOnReaderStopped_NilCallbacks(t *testing.T) {
	// Verifies OnReaderStopped doesn't panic when typingFunc, handler, and
	// resultCh are all nil/unset.
	t.Parallel()

	b := &Backend{}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	// No typing, no handler, no result channel. Should not panic.
	b.OnReaderStopped(fmt.Errorf("test error"))
}

func TestOnReaderStopped_ExpectedClose(t *testing.T) {
	// Verifies that when closing=true (set by Close), OnReaderStopped uses a
	// non-error turn-complete message instead of "exited unexpectedly".
	t.Parallel()

	var completedResult *delegator.TurnResult

	b := &Backend{}
	b.mu.Lock()
	b.running = true
	b.closing = true
	b.mu.Unlock()
	b.typingFunc = func(bool) {}

	handler := &delegator.EventHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	b.beginTurn(handler)

	b.OnReaderStopped(io.EOF)

	if completedResult == nil {
		t.Fatal("OnTurnComplete was not called")
	}
	if strings.Contains(completedResult.Text, "unexpectedly") {
		t.Errorf("result.Text = %q, should not say 'unexpectedly' for expected close", completedResult.Text)
	}
	if !strings.Contains(completedResult.Text, "Session closed") {
		t.Errorf("result.Text = %q, want to contain 'Session closed'", completedResult.Text)
	}
}

// ---------------------------------------------------------------------------
// OnToolProgress
// ---------------------------------------------------------------------------

func TestOnToolProgress_KeepsTypingAlive(t *testing.T) {
	// Verifies OnToolProgress fires the typing indicator to keep it alive.
	t.Parallel()

	var typingCalls []bool
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&bytes.Buffer{}}),
	}
	b.typingFunc = func(v bool) { typingCalls = append(typingCalls, v) }

	msg := &ToolProgressMessage{
		ToolUseID:          "t1",
		ToolName:           "Bash",
		ElapsedTimeSeconds: 5,
	}
	b.OnToolProgress(msg)

	if len(typingCalls) != 1 || !typingCalls[0] {
		t.Errorf("typingCalls = %v, want [true]", typingCalls)
	}
}

// ---------------------------------------------------------------------------
// OnStreamEvent
// ---------------------------------------------------------------------------

func TestOnStreamEvent_TextDelta(t *testing.T) {
	// Verifies OnStreamEvent extracts text_delta content from stream events
	// and fires the handler's OnText callback.
	t.Parallel()

	var deltas []string
	b := &Backend{}
	b.turnHandler = &delegator.EventHandler{
		OnTextDelta: func(delta string) { deltas = append(deltas, delta) },
	}

	raw := json.RawMessage(`{
		"type": "stream_event",
		"event": {
			"type": "content_block_delta",
			"delta": {
				"type": "text_delta",
				"text": "Hello"
			}
		}
	}`)
	b.OnStreamEvent(raw)

	if len(deltas) != 1 || deltas[0] != "Hello" {
		t.Errorf("deltas = %v, want [Hello]", deltas)
	}
}

func TestOnStreamEvent_NonTextDelta(t *testing.T) {
	// Verifies OnStreamEvent ignores events that are not text_delta or thinking_delta.
	t.Parallel()

	var called bool
	b := &Backend{}
	b.turnHandler = &delegator.EventHandler{
		OnText: func(text string) { called = true },
	}

	raw := json.RawMessage(`{
		"type": "stream_event",
		"event": {
			"type": "content_block_start",
			"delta": {
				"type": "tool_use",
				"text": ""
			}
		}
	}`)
	b.OnStreamEvent(raw)

	if called {
		t.Error("OnText should not be called for non-text_delta events")
	}
}

// TestOnStreamEvent_ThinkingDelta proves that thinking_delta subtypes in
// content_block_delta stream events dispatch to OnThinkingDelta (not
// OnTextDelta). This is the path that carries per-token thinking streaming
// from CC — previously dropped, now wired through turnevent.ThinkingDelta.
func TestOnStreamEvent_ThinkingDelta(t *testing.T) {
	t.Parallel()

	var textDeltas []string
	var thinkingDeltas []string
	b := &Backend{}
	b.turnHandler = &delegator.EventHandler{
		OnTextDelta:     func(delta string) { textDeltas = append(textDeltas, delta) },
		OnThinkingDelta: func(delta string) { thinkingDeltas = append(thinkingDeltas, delta) },
	}

	raw := json.RawMessage(`{
		"type": "stream_event",
		"event": {
			"type": "content_block_delta",
			"delta": {
				"type": "thinking_delta",
				"thinking": "I should check "
			}
		}
	}`)
	b.OnStreamEvent(raw)

	if len(textDeltas) != 0 {
		t.Errorf("OnTextDelta fired for thinking_delta: %v", textDeltas)
	}
	if len(thinkingDeltas) != 1 || thinkingDeltas[0] != "I should check " {
		t.Errorf("thinkingDeltas = %v, want [I should check ]", thinkingDeltas)
	}
}

// TestOnStreamEvent_EmptyThinking proves empty thinking_delta payloads are
// dropped, matching the existing text_delta behaviour and keeping downstream
// emit helpers from firing no-op events.
func TestOnStreamEvent_EmptyThinking(t *testing.T) {
	t.Parallel()

	var called bool
	b := &Backend{}
	b.turnHandler = &delegator.EventHandler{
		OnThinkingDelta: func(delta string) { called = true },
	}

	raw := json.RawMessage(`{
		"type": "stream_event",
		"event": {
			"type": "content_block_delta",
			"delta": {
				"type": "thinking_delta",
				"thinking": ""
			}
		}
	}`)
	b.OnStreamEvent(raw)

	if called {
		t.Error("OnThinkingDelta should not fire for empty thinking delta")
	}
}

func TestOnStreamEvent_EmptyText(t *testing.T) {
	// Verifies OnStreamEvent ignores text_delta with empty text.
	t.Parallel()

	var called bool
	b := &Backend{}
	b.turnHandler = &delegator.EventHandler{
		OnText: func(text string) { called = true },
	}

	raw := json.RawMessage(`{
		"type": "stream_event",
		"event": {
			"type": "content_block_delta",
			"delta": {
				"type": "text_delta",
				"text": ""
			}
		}
	}`)
	b.OnStreamEvent(raw)

	if called {
		t.Error("OnText should not be called for empty text delta")
	}
}

func TestOnStreamEvent_NilHandler(t *testing.T) {
	// Verifies OnStreamEvent doesn't panic when no handler is set.
	t.Parallel()

	b := &Backend{}
	raw := json.RawMessage(`{
		"type": "stream_event",
		"event": {
			"type": "content_block_delta",
			"delta": {"type": "text_delta", "text": "hello"}
		}
	}`)
	// Should not panic.
	b.OnStreamEvent(raw)
}

func TestOnStreamEvent_InvalidJSON(t *testing.T) {
	// Verifies OnStreamEvent handles malformed JSON gracefully.
	t.Parallel()

	var called bool
	b := &Backend{}
	b.turnHandler = &delegator.EventHandler{
		OnText: func(text string) { called = true },
	}

	b.OnStreamEvent(json.RawMessage(`{not valid json`))

	if called {
		t.Error("OnText should not be called for invalid JSON")
	}
}

// TestOnStreamEvent_SubagentFiltered proves that stream events with
// parent_tool_use_id set are filtered out, matching the guard in OnAssistant.
// Without this, sub-agent deltas leak into the parent turn's StreamWriter.
func TestOnStreamEvent_SubagentFiltered(t *testing.T) {
	t.Parallel()

	var deltas []string
	b := &Backend{}
	b.turnHandler = &delegator.EventHandler{
		OnTextDelta: func(delta string) { deltas = append(deltas, delta) },
	}

	// Sub-agent stream event — should be filtered.
	raw := json.RawMessage(`{
		"type": "stream_event",
		"parent_tool_use_id": "toolu_sub123",
		"event": {
			"type": "content_block_delta",
			"delta": {
				"type": "text_delta",
				"text": "sub-agent delta"
			}
		}
	}`)
	b.OnStreamEvent(raw)

	if len(deltas) != 0 {
		t.Errorf("OnTextDelta fired for sub-agent stream event: %v", deltas)
	}

	// Top-level stream event — should pass through.
	raw = json.RawMessage(`{
		"type": "stream_event",
		"event": {
			"type": "content_block_delta",
			"delta": {
				"type": "text_delta",
				"text": "top-level delta"
			}
		}
	}`)
	b.OnStreamEvent(raw)

	if len(deltas) != 1 || deltas[0] != "top-level delta" {
		t.Errorf("deltas = %v, want [top-level delta]", deltas)
	}
}

// ---------------------------------------------------------------------------
// OnControlResponse / OnControlCancelRequest
// ---------------------------------------------------------------------------

func TestOnControlResponse_NoMatchingWaiter(t *testing.T) {
	// Verifies OnControlResponse is safe when no waiter is registered.
	t.Parallel()

	b := &Backend{}
	// Should not panic — no pending controls, unknown request_id.
	b.OnControlResponse(json.RawMessage(`{"type":"control_response","response":{"subtype":"success","request_id":"unknown","response":{}}}`))
}

func TestOnControlResponse_RoutesToWaiter(t *testing.T) {
	// Verifies that OnControlResponse routes by request_id to the pending channel.
	t.Parallel()

	b := &Backend{
		pendingControls: make(map[string]chan json.RawMessage),
	}
	ch := make(chan json.RawMessage, 1)
	b.pendingControls["req-ctx-1"] = ch

	raw := json.RawMessage(`{"type":"control_response","response":{"subtype":"success","request_id":"req-ctx-1","response":{"totalTokens":50000,"maxTokens":200000}}}`)
	b.OnControlResponse(raw)

	select {
	case got := <-ch:
		if string(got) != string(raw) {
			t.Errorf("got %s, want %s", got, raw)
		}
	default:
		t.Error("expected response on channel, got nothing")
	}

	// Channel should be removed from pending map.
	b.pendingControlMu.Lock()
	_, stillPending := b.pendingControls["req-ctx-1"]
	b.pendingControlMu.Unlock()
	if stillPending {
		t.Error("request_id should be removed from pendingControls after delivery")
	}
}

func TestGetContextUsage(t *testing.T) {
	// End-to-end test: GetContextUsage sends request, receives routed response.
	t.Parallel()

	// Create a pipe; drain reader so writer.SendControl doesn't block.
	pr, pw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, pr) }()

	b := &Backend{
		writer:          NewWriter(pw),
		pendingControls: make(map[string]chan json.RawMessage),
	}

	// Run GetContextUsage in a goroutine (it blocks waiting for response).
	type result struct {
		usage *delegator.ContextUsage
		err   error
	}
	resCh := make(chan result, 1)
	ctx := context.Background()

	go func() {
		u, err := b.GetContextUsage(ctx)
		resCh <- result{u, err}
	}()

	// Wait for the pending control to be registered.
	var reqID string
	for i := 0; i < 100; i++ {
		time.Sleep(time.Millisecond)
		b.pendingControlMu.Lock()
		for k := range b.pendingControls {
			reqID = k
		}
		b.pendingControlMu.Unlock()
		if reqID != "" {
			break
		}
	}
	if reqID == "" {
		t.Fatal("GetContextUsage didn't register a pending control request")
	}

	// Simulate CC returning the response via OnControlResponse.
	resp := fmt.Sprintf(`{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"totalTokens":50000,"maxTokens":200000,"percentage":25,"autoCompactThreshold":160000,"model":"claude-sonnet-4-6"}}}`, reqID)
	b.OnControlResponse(json.RawMessage(resp))

	// Read result.
	r := <-resCh
	if r.err != nil {
		t.Fatalf("GetContextUsage error: %v", r.err)
	}
	if r.usage.TotalTokens != 50000 {
		t.Errorf("TotalTokens = %d, want 50000", r.usage.TotalTokens)
	}
	if r.usage.MaxTokens != 200000 {
		t.Errorf("MaxTokens = %d, want 200000", r.usage.MaxTokens)
	}
	if r.usage.Percentage != 25 {
		t.Errorf("Percentage = %d, want 25", r.usage.Percentage)
	}
	if r.usage.AutoCompactThreshold != 160000 {
		t.Errorf("AutoCompactThreshold = %d, want 160000", r.usage.AutoCompactThreshold)
	}
	if r.usage.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want %q", r.usage.Model, "claude-sonnet-4-6")
	}

	pw.Close()
}

func TestOnControlCancelRequest(t *testing.T) {
	// Verifies OnControlCancelRequest removes the pending permission and
	// fires the registry's onEmpty hook when no more prompts are outstanding.
	t.Parallel()

	var clearedCalled bool
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { clearedCalled = true })

	// Add a pending permission via both stores (the production path stores
	// in pendingPerms and registers in outstanding atomically).
	b.pendingPerms["req-1"] = &pendingPermission{
		requestID: "req-1",
		toolName:  "Bash",
	}
	b.outstanding.Register("req-1", OutstandingPermission)

	b.OnControlCancelRequest("req-1")

	b.permMu.Lock()
	count := len(b.pendingPerms)
	b.permMu.Unlock()

	if count != 0 {
		t.Errorf("pending permissions count = %d, want 0", count)
	}
	if !clearedCalled {
		t.Error("onPromptsCleared was not called")
	}
}

func TestOnControlCancelRequest_StillPending(t *testing.T) {
	// Verifies onPromptsCleared is NOT fired when other prompts are still
	// outstanding after a cancel.
	t.Parallel()

	var clearedCalled bool
	b := &Backend{
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { clearedCalled = true })

	b.pendingPerms["req-1"] = &pendingPermission{requestID: "req-1"}
	b.pendingPerms["req-2"] = &pendingPermission{requestID: "req-2"}
	b.outstanding.Register("req-1", OutstandingPermission)
	b.outstanding.Register("req-2", OutstandingPermission)

	b.OnControlCancelRequest("req-1")

	if clearedCalled {
		t.Error("onPromptsCleared should not be called when other prompts remain")
	}
	b.permMu.Lock()
	count := len(b.pendingPerms)
	b.permMu.Unlock()
	if count != 1 {
		t.Errorf("pending permissions count = %d, want 1", count)
	}
}

// ---------------------------------------------------------------------------
// WaitReady
// ---------------------------------------------------------------------------

func TestWaitReady_AlreadyReady(t *testing.T) {
	// Verifies WaitReady returns immediately when the backend is already
	// initialised (readyCh already closed).
	t.Parallel()

	b := &Backend{
		readyCh: make(chan struct{}),
	}
	close(b.readyCh) // Already ready.

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := b.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestWaitReady_ContextCancellation(t *testing.T) {
	// Verifies WaitReady respects context cancellation when the backend
	// is not ready.
	t.Parallel()

	b := &Backend{
		readyCh: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- b.WaitReady(ctx)
	}()

	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("WaitReady err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitReady did not return after context cancellation")
	}
}

func TestWaitReady_UnblockedByInit(t *testing.T) {
	// Verifies WaitReady unblocks when OnSystem/init is received.
	t.Parallel()

	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
	}

	done := make(chan error, 1)
	go func() {
		done <- b.WaitReady(context.Background())
	}()

	// Simulate init message.
	raw := json.RawMessage(`{
		"type": "system", "subtype": "init",
		"claude_code_version": "1.0", "cwd": "/tmp",
		"model": "test", "permissionMode": "default", "tools": [],
		"session_id": "s1"
	}`)
	time.Sleep(10 * time.Millisecond)
	b.OnSystem("init", raw)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitReady: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitReady did not unblock after init")
	}
}

func TestWaitReady_UnblockedByInitControlResponse(t *testing.T) {
	// Verifies WaitReady unblocks when a control_response matching the
	// initialize request ID is received. This is the code path for fresh
	// sessions (no --resume) where CC responds with control_response
	// instead of system/init.
	t.Parallel()

	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
		initReqID:    "init-42",
	}

	done := make(chan error, 1)
	go func() {
		done <- b.WaitReady(context.Background())
	}()

	// Simulate control_response to the initialize request.
	raw := json.RawMessage(`{
		"type": "control_response",
		"response": {
			"subtype": "success",
			"request_id": "init-42",
			"response": {}
		}
	}`)
	time.Sleep(10 * time.Millisecond)
	b.OnControlResponse(raw)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitReady: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitReady did not unblock after initialize control_response")
	}

	// initReqID should be consumed (cleared).
	b.mu.Lock()
	if b.initReqID != "" {
		t.Errorf("initReqID not cleared, got %q", b.initReqID)
	}
	b.mu.Unlock()
}

func TestWaitReady_ControlResponseIgnoresNonInit(t *testing.T) {
	// Verifies that a control_response with a different request ID does
	// NOT close readyCh.
	t.Parallel()

	b := &Backend{
		readyCh:      make(chan struct{}),
		pendingPerms: make(map[string]*pendingPermission),
		outstanding:  NewOutstandingRegistry(),
		initReqID:    "init-42",
	}

	raw := json.RawMessage(`{
		"type": "control_response",
		"response": {
			"subtype": "success",
			"request_id": "other-99",
			"response": {}
		}
	}`)
	b.OnControlResponse(raw)

	// readyCh should still be open.
	select {
	case <-b.readyCh:
		t.Fatal("readyCh closed by non-matching control_response")
	default:
		// expected
	}
}

// ---------------------------------------------------------------------------
// Close (partial — only tests state; subprocess logic needs real process)
// ---------------------------------------------------------------------------

func TestClose_NotRunning(t *testing.T) {
	// Verifies Close returns nil immediately when the backend is not running.
	t.Parallel()

	b := &Backend{}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	// Verifies that multiple Close calls on a non-running backend are safe.
	t.Parallel()

	b := &Backend{}
	_ = b.Close()
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestConcurrentOnAssistantAndOnResult(t *testing.T) {
	// Verifies that concurrent calls to OnAssistant and OnResult do not race.
	// This is a basic concurrency smoke test rather than a full race condition
	// proof — the -race detector catches actual data races.
	t.Parallel()

	b := &Backend{}

	handler := &delegator.EventHandler{
		OnText:         func(string) {},
		OnToolStart:    func(string, string, string) {},
		OnTurnComplete: func(*delegator.TurnResult) {},
	}
	b.beginTurn(handler)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			b.OnAssistant(&AssistantMessage{
				Message: BetaMessage{
					Model: "test",
					Content: []ContentBlock{
						{Type: "text", Text: fmt.Sprintf("msg-%d", i)},
					},
					Usage: TokenUsage{InputTokens: i},
				},
			})
		}
	}()

	go func() {
		defer wg.Done()
		// Wait a bit, then send result.
		time.Sleep(5 * time.Millisecond)
		b.OnResult(&ResultMessage{
			Subtype: "success",
			Result:  "done",
			Usage:   TokenUsage{},
		})
	}()

	wg.Wait()
}

func TestNewRequestID_Unique(t *testing.T) {
	// Verifies newRequestID generates unique values across rapid calls.
	t.Parallel()

	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := newRequestID()
		if seen[id] {
			t.Fatalf("duplicate request ID: %s", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// logComponent
// ---------------------------------------------------------------------------

func TestLogComponent_WithLabel(t *testing.T) {
	// Proves logComponent includes the label when set.
	t.Parallel()
	b := &Backend{label: "helen-c123"}
	if got := b.logComponent(); got != "ccstream:helen-c123" {
		t.Errorf("logComponent() = %q, want %q", got, "ccstream:helen-c123")
	}
}

func TestLogComponent_NoLabel(t *testing.T) {
	// Proves logComponent returns the bare prefix when no label is set.
	t.Parallel()
	b := &Backend{}
	if got := b.logComponent(); got != "ccstream" {
		t.Errorf("logComponent() = %q, want %q", got, "ccstream")
	}
}

// ---------------------------------------------------------------------------
// describeExitError
// ---------------------------------------------------------------------------

func TestDescribeExitError_Nil(t *testing.T) {
	// Proves nil error returns a clean status message.
	t.Parallel()
	if got := describeExitError(nil); got != "exit status 0" {
		t.Errorf("describeExitError(nil) = %q, want %q", got, "exit status 0")
	}
}

func TestDescribeExitError_NonExitError(t *testing.T) {
	// Proves non-exec.ExitError falls back to err.Error().
	t.Parallel()
	err := errors.New("some weird error")
	if got := describeExitError(err); got != "some weird error" {
		t.Errorf("describeExitError = %q, want %q", got, "some weird error")
	}
}

func TestDescribeExitError_RealProcess(t *testing.T) {
	// Proves describeExitError extracts a real exit code from a failed process.
	t.Parallel()
	cmd := exec.Command("sh", "-c", "exit 42")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected error from exit 42")
	}
	got := describeExitError(err)
	if !strings.Contains(got, "exit code 42") {
		t.Errorf("describeExitError = %q, want it to contain 'exit code 42'", got)
	}
}

func TestDescribeExitError_Signal(t *testing.T) {
	// Proves describeExitError reports the signal when a process is killed.
	t.Parallel()
	cmd := exec.Command("sh", "-c", "kill -KILL $$")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected error from killed process")
	}
	got := describeExitError(err)
	if !strings.Contains(got, "signal") {
		t.Errorf("describeExitError = %q, want it to contain 'signal'", got)
	}
}

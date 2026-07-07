package ccstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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
		outstanding:  delegator.NewOutstandingRegistry(),
	}

	// SetPermissionPromptFunc
	var permCalled bool
	b.SetPermissionPromptFunc(func(reqID, text, summary, attachmentPath string, choices []delegator.PromptChoice) {
		permCalled = true
	})
	if b.permPromptFn == nil {
		t.Error("permPromptFn is nil after SetPermissionPromptFunc")
	}
	b.permPromptFn("", "", "", "", nil)
	if !permCalled {
		t.Error("permPromptFn was not called")
	}

	// SetOnPromptsCleared — registered via the delegator.OutstandingRegistry's onEmpty
	// hook. We exercise it by registering+resolving a prompt and asserting the
	// hook fired exactly once.
	var clearedCalled bool
	b.SetOnPromptsCleared(func() { clearedCalled = true })
	b.outstanding.Register("test-req", delegator.OutstandingPermission)
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

	// SetOnSubagentStatus
	var agentStatusText string
	b.SetOnSubagentStatus(func(detail string) { agentStatusText = detail })
	if b.agents.OnStatus == nil {
		t.Error("agents.OnStatus is nil after SetOnSubagentStatus")
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

	handler := &testHandler{
		OnText: func(text string) {},
	}
	applyHandler(b, handler)

	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after beginTurn")
	}
	b.turnMu.Lock()
	if b.turnEvents == nil {
		t.Error("turnEvents not set after applyHandler")
	}
	if b.turnResultCh == nil {
		t.Error("turnResultCh is nil")
	}
	b.turnMu.Unlock()
	if b.sessionEvents.Load() == nil {
		t.Error("sessionEvents not set after applyHandler")
	}

	b.cancelTurn()
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after cancelTurn")
	}
	b.turnMu.Lock()
	if b.turnEvents != nil {
		t.Error("turnEvents not nil after cancelTurn")
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

	handler := &testHandler{}
	applyHandler(b, handler)

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

// TestSendUserMessage_WireShape verifies the internal sendUserMessage
// primitive emits a well-formed user message on the wire with the
// default queue priority (omitted, so CC defaults to "next"). Wire-shape
// coverage stays here; routing coverage lives in TestInject_*.
func TestSendUserMessage_WireShape(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}

	if err := b.sendUserMessage("hello"); err != nil {
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
		t.Errorf("priority field should be absent for default sendUserMessage, got %v", got["priority"])
	}
	msg := got["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Errorf("content = %v, want %q", msg["content"], "hello")
	}
}

// TestSendUserMessagePriority_WireShape verifies the priority-bearing
// primitive emits the priority field in the envelope so CC's queue can
// dequeue it ahead of default-priority items at the next mid-turn drain.
func TestSendUserMessagePriority_WireShape(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}

	if err := b.sendUserMessagePriority("urgent text", "now"); err != nil {
		t.Fatalf("sendUserMessagePriority: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["priority"] != "now" {
		t.Errorf("priority = %v, want %q", got["priority"], "now")
	}
}

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
	handler := &testHandler{OnText: func(string) {}}
	b.AttachSessionEvents(handler.session())

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "hello",
		Turn:   handler.turn(),
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after begin-turn Inject; want true")
	}
	b.turnMu.Lock()
	gotTurn := b.turnEvents
	b.turnMu.Unlock()
	if gotTurn == nil {
		t.Error("turnEvents nil — Inject did not install bookkeeping")
	}
	if b.sessionEvents.Load() == nil {
		t.Error("sessionEvents nil — Inject did not install delivery")
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
	handler := &testHandler{OnText: func(string) {}}
	b.AttachSessionEvents(handler.session())

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "describe this",
		Attachments: []delegator.Attachment{
			{MimeType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}},
		},
		Turn: handler.turn(),
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

// TestInject_User_InFlight_FoldsViaSendUser verifies Inject(SourceUser)
// during an in-flight turn queues the text via SendUser at default
// priority (no priority field on the wire; CC defaults to "next"). CC's
// mid-turn drain at the next tool boundary folds the message into the
// current ask() as an attachment — there is no rearm bookkeeping and
// no separate result cycle to wait for.
func TestInject_User_InFlight_FoldsViaSendUser(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:     NewWriter(nopWriteCloser{&buf}),
		turnActive: true,
	}

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceUser,
		Text:   "follow-up",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "follow-up") {
		t.Errorf("writer did not see follow-up text; got: %q", out)
	}
	// Follow-up SourceUser uses default priority — no priority field on the wire.
	if strings.Contains(out, `"priority"`) {
		t.Errorf("priority field should be absent on follow-up SourceUser; got: %q", out)
	}
	// No interrupt — only Steer would do that, and we removed it from Steer too.
	if strings.Contains(out, "interrupt") {
		t.Errorf("interrupt should NOT appear; got: %q", out)
	}
}

// TestInject_System_Idle_BeginsTurn verifies Inject(SourceSystem) at idle
// begins a fresh tracked turn exactly like SourceUser: bookkeeping installed,
// prompt written.
func TestInject_System_Idle_BeginsTurn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	handler := &testHandler{OnText: func(string) {}}
	b.AttachSessionEvents(handler.session())

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSystem,
		Text:   "[keepalive]",
		Turn:   handler.turn(),
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = false after system begin-turn Inject; want true")
	}
	if !strings.Contains(buf.String(), "[keepalive]") {
		t.Errorf("writer missing prompt text; got: %q", buf.String())
	}
}

// TestInject_System_InFlight_Rejects verifies Inject(SourceSystem) during an
// in-flight turn returns ErrTurnInFlight, writes nothing to CC, and leaves
// the running turn's bookkeeping untouched — system input never folds into
// (steers) a running turn; the caller waits for completion and retries.
func TestInject_System_InFlight_Rejects(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	existing := &delegator.TurnEvents{}
	b := &Backend{
		writer:     NewWriter(nopWriteCloser{&buf}),
		turnActive: true,
		turnEvents: existing,
	}

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSystem,
		Text:   "[keepalive]",
		Turn:   &delegator.TurnEvents{},
	})
	if !errors.Is(err, delegator.ErrTurnInFlight) {
		t.Fatalf("Inject err = %v, want ErrTurnInFlight", err)
	}
	if buf.Len() != 0 {
		t.Errorf("system inject wrote to CC during in-flight turn: %q", buf.String())
	}
	b.turnMu.Lock()
	gotTurn := b.turnEvents
	b.turnMu.Unlock()
	if gotTurn != existing {
		t.Error("rejected system inject replaced the running turn's TurnEvents")
	}
}

// TestInject_Steer_InFlight_NoInterrupt_PriorityNext verifies that
// Inject(SourceSteer) during an in-flight turn does NOT call Interrupt and
// does NOT use priority "now" (which makes CC abort the in-flight ask —
// reserved for NYI aggressive-steer gating). The steer is queued at "next"
// so CC folds it into the running ask at the next tool boundary, matching
// CC's own class for user input.
func TestInject_Steer_InFlight_NoInterrupt_PriorityNext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:     NewWriter(nopWriteCloser{&buf}),
		turnActive: true,
	}

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "urgent text",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "urgent text") {
		t.Errorf("Steer text missing from writer output; got: %q", out)
	}
	// Critical: NO interrupt control_request should be emitted.
	if strings.Contains(out, "interrupt") {
		t.Errorf("interrupt should NOT appear post-rearm-removal; got: %q", out)
	}
	// Priority "now" would make CC abort the in-flight ask — steers must not.
	if strings.Contains(out, `"priority":"now"`) {
		t.Errorf("steer sent at priority \"now\" (aborts the ask); want \"next\": %q", out)
	}
	if !strings.Contains(out, `"priority":"next"`) {
		t.Errorf("priority=\"next\" missing from Steer envelope; got: %q", out)
	}
}

// TestInject_Steer_Idle_BeginsTurn verifies the edge case where a steer
// arrives at idle (race between turn end and platform queue dispatch) and
// carries turn bookkeeping (here via the legacy Handler, which the shim turns
// into a Turn): Inject degrades to a fresh begin-turn rather than calling
// Interrupt on a non-existent in-flight turn. (The Turn-less inbox shape is
// covered by TestInject_Steer_Idle_NoTurn_ReturnsErrTurnNotInFlight.)
func TestInject_Steer_Idle_BeginsTurn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}
	handler := &testHandler{OnText: func(string) {}}
	b.AttachSessionEvents(handler.session())

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "steer-at-idle",
		Turn:   handler.turn(),
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

// TestInject_Steer_Idle_NoTurn_ReturnsErrTurnNotInFlight verifies the inbox
// race fix: a steer that arrives at idle with neither Turn nor Handler (the
// shape the inbox builds) is declined with ErrTurnNotInFlight rather than
// beginning an untracked turn (nil TurnEvents → no OnTurnComplete). No turn
// must start and nothing must be written to the backend.
func TestInject_Steer_Idle_NoTurn_ReturnsErrTurnNotInFlight(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}

	err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceSteer,
		Text:   "steer-at-idle-no-turn",
	})
	if !errors.Is(err, delegator.ErrTurnNotInFlight) {
		t.Fatalf("Inject err = %v, want ErrTurnNotInFlight", err)
	}
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true; a declined steer must not begin a turn")
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should have been written; got: %q", buf.String())
	}
}

// TestInject_Compact verifies Inject(SourceCompact) at idle sends the
// slash command via SendUser. /compact is fire-and-forget: the response
// (compact_boundary system event) flows through CompactionWaiter.
func TestInject_Compact(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{writer: NewWriter(nopWriteCloser{&buf})}

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   "/compact summarise everything",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if !strings.Contains(buf.String(), "/compact") {
		t.Errorf("writer missing /compact text; got: %q", buf.String())
	}
}

// TestInject_Compact_InFlight verifies the slash-command path is
// callable mid-turn without disturbing turn state. /compact shouldn't
// normally be invoked mid-turn, but if it is, the call must succeed.
func TestInject_Compact_InFlight(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:     NewWriter(nopWriteCloser{&buf}),
		turnActive: true,
	}

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourceCompact,
		Text:   "/compact x",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
}

// TestInject_Pass mirrors Compact for the /pass passthrough path —
// slash commands like /context, /model are sent via SendUser and don't
// disturb turn state.
func TestInject_Pass(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := &Backend{
		writer:     NewWriter(nopWriteCloser{&buf}),
		turnActive: true,
	}

	if err := b.ImmediateInject(context.Background(), delegator.Inject{
		Source: delegator.SourcePass,
		Text:   "/model opus",
	}); err != nil {
		t.Fatalf("Inject: %v", err)
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
		writer:  NewWriter(nopWriteCloser{&buf}),
		readyCh: make(chan struct{}),
	}
	b.SetTypingFunc(func(v bool) { typingCalls = append(typingCalls, v) })

	turn := &delegator.TurnEvents{}
	if err := b.sendToPane(context.Background(), "hello world", turn, false); err != nil {
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

	turn := &delegator.TurnEvents{}
	atts := []delegator.Attachment{
		{MimeType: "image/jpeg", Data: []byte("fake-jpeg")},
		{MimeType: "application/pdf", Data: []byte("fake-pdf")},
	}
	if err := b.sendToPaneWithAttachments(context.Background(), "describe these", atts, turn, false); err != nil {
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

	turn := &delegator.TurnEvents{}
	if err := b.sendToPane(context.Background(), "hello", turn, false); err == nil {
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
	handler := &testHandler{
		OnText: func(text string) { handlerTexts = append(handlerTexts, text) },
	}
	applyHandler(b, handler)

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

// TestOnAssistant_CrossMessageSeparation pins TODO #819: text from separate
// assistant messages within one turn (segments split by tool calls) must be
// joined with a blank line, not glued. Pre-tool-call narration was arriving
// concatenated onto the next segment (e.g. "...correctly.Καλημέρα").
func TestOnAssistant_CrossMessageSeparation(t *testing.T) {
	t.Parallel()

	b := &Backend{}
	applyHandler(b, &testHandler{})

	mkMsg := func(blocks ...ContentBlock) *AssistantMessage {
		return &AssistantMessage{Message: BetaMessage{
			Model:   "claude-sonnet-4-20250514",
			Content: blocks,
			Usage:   TokenUsage{InputTokens: 100, OutputTokens: 20},
		}}
	}

	// Segment 1: narration + tool call (same message).
	b.OnAssistant(mkMsg(
		ContentBlock{Type: "text", Text: "Daily quiz time."},
		ContentBlock{Type: "tool_use", ID: "t1", Name: "Bash"},
	))
	// Segment 2: narration split across two text blocks in one message +
	// another tool call. The intra-message split must NOT be separated.
	b.OnAssistant(mkMsg(
		ContentBlock{Type: "text", Text: "Good "},
		ContentBlock{Type: "text", Text: "queue."},
		ContentBlock{Type: "tool_use", ID: "t2", Name: "Bash"},
	))
	// Segment 3: final answer.
	b.OnAssistant(mkMsg(ContentBlock{Type: "text", Text: "Καλημέρα!"}))

	b.turnMu.Lock()
	got := b.turnText.String()
	b.turnMu.Unlock()

	want := "Daily quiz time.\n\nGood queue.\n\nΚαλημέρα!"
	if got != want {
		t.Errorf("turnText = %q, want %q", got, want)
	}
}

// TestTextDelivery_NoHandler_GoesToSessionEvents is the regression test for
// TODO #747: text emitted AFTER a turn's OnResult clears the per-turn
// TurnEvents must still deliver via the always-live SessionEvents path.
//
// Before the SessionEvents/TurnEvents split, ccstream's text-emit path read
// `b.turnHandler` under turnMu and dropped on nil with a "text block dropped:
// handler_nil=true" warning — re-exposed by commit 57445dc6 which removed
// the rearm cascade that had been keeping turnHandler non-nil across stacked
// CC results. The fix divorces delivery (session lifetime) from bookkeeping
// (turn lifetime). This test pins that invariant: drive a turn to completion,
// then emit more text, and assert the SessionEvents.OnText callback still
// fires.
func TestTextDelivery_NoHandler_GoesToSessionEvents(t *testing.T) {
	t.Parallel()

	var sessionTexts []string
	var turnCompletedFired bool

	b := &Backend{}
	b.AttachSessionEvents(&delegator.SessionEvents{
		OnText: func(text string) { sessionTexts = append(sessionTexts, text) },
	})
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(_ *delegator.TurnResult) { turnCompletedFired = true },
	})

	// Round 1: text during the turn — SessionEvents.OnText should fire.
	roundOne := &AssistantMessage{
		Message: BetaMessage{
			Model: "claude-sonnet-4-20250514",
			Content: []ContentBlock{
				{Type: "text", Text: "first round text"},
			},
		},
	}
	b.OnAssistant(roundOne)

	// Turn ends. OnResult clears b.turnEvents but leaves b.sessionEvents
	// untouched (the lifetime split). turnCompletedFired flips to true.
	b.OnResult(&ResultMessage{Subtype: "success", Result: "ok", Usage: TokenUsage{}})
	if !turnCompletedFired {
		t.Fatal("OnTurnComplete did not fire on OnResult")
	}
	b.turnMu.Lock()
	turnEventsAfterResult := b.turnEvents
	b.turnMu.Unlock()
	if turnEventsAfterResult != nil {
		t.Fatal("b.turnEvents not cleared after OnResult — invariant broken")
	}
	if b.sessionEvents.Load() == nil {
		t.Fatal("b.sessionEvents was cleared by OnResult — should live for the session")
	}

	// Round 2: text emitted post-OnResult — this is the failure scenario
	// that pre-TODO #747 dropped with the "text block dropped: handler nil"
	// warning. Now SessionEvents.OnText should still fire.
	roundTwo := &AssistantMessage{
		Message: BetaMessage{
			Model: "claude-sonnet-4-20250514",
			Content: []ContentBlock{
				{Type: "text", Text: "post-result text"},
			},
		},
	}
	b.OnAssistant(roundTwo)

	if len(sessionTexts) != 2 {
		t.Fatalf("got %d session texts, want 2; texts=%v", len(sessionTexts), sessionTexts)
	}
	if sessionTexts[0] != "first round text" {
		t.Errorf("first round text = %q, want %q", sessionTexts[0], "first round text")
	}
	if sessionTexts[1] != "post-result text" {
		t.Errorf("post-result text = %q, want %q", sessionTexts[1], "post-result text")
	}
}

func TestOnAssistant_ToolUseTracking(t *testing.T) {
	// Verifies OnAssistant increments the tool call counter for tool_use blocks
	// and fires the handler's OnToolStart callback.
	t.Parallel()

	var toolStarts []string

	b := &Backend{}
	handler := &testHandler{
		OnToolStart: func(_ string, name string, input string) {
			toolStarts = append(toolStarts, name)
		},
	}
	applyHandler(b, handler)

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
	applyHandler(b, &testHandler{})

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
	applyHandler(b, &testHandler{})

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
	applyHandler(b, &testHandler{})

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
	applyHandler(b, &testHandler{})

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
	applyHandler(b, &testHandler{})

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
	applyHandler(b, &testHandler{
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
	// Verifies Agent tool_use blocks are tracked via the shared SubagentTracker,
	// mirroring the tmux backend's behavior.
	t.Parallel()

	var statusMessages []string
	b := &Backend{}
	b.SetOnSubagentStatus(func(text string) { statusMessages = append(statusMessages, text) })
	applyHandler(b, &testHandler{})

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
	b.SetOnSubagentStatus(func(string) {})
	applyHandler(b, &testHandler{})

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

func TestOnResult_KeepsTrackedAgentsAcrossTurn(t *testing.T) {
	// Background agents outlive the turn that spawned them, so completing a turn
	// must NOT clear them — they persist until task_notification, the max-age
	// prune, or session exit.
	t.Parallel()

	var statusMessages []string
	b := &Backend{}
	b.SetOnSubagentStatus(func(text string) { statusMessages = append(statusMessages, text) })
	b.agents.Add("ag1", "still running")
	statusMessages = nil // clear Add notification

	applyHandler(b, &testHandler{})
	b.OnResult(&ResultMessage{Subtype: "success", Usage: TokenUsage{}})

	if b.agents.Pending() != 1 {
		t.Fatalf("Pending() = %d, want 1 (agent survives turn completion)", b.agents.Pending())
	}
	if len(statusMessages) != 0 {
		t.Fatalf("OnStatus called %d times, want 0 (no clear on turn complete)", len(statusMessages))
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

	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	applyHandler(b, handler)

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

func TestOnResult_OutputTokensFromModelUsage(t *testing.T) {
	// Regression for #721: lastUsage carries an early/partial output_tokens
	// snapshot from the live stream (e.g. ≈1) that never refreshes to the
	// final count. OnResult must correct OUTPUT from the authoritative
	// per-model result accounting (ModelUsage[resultModel]), while keeping
	// input/cache from lastUsage (the final call's context fill).
	t.Parallel()

	var completedResult *delegator.TurnResult
	b := &Backend{}
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	applyHandler(b, handler)

	b.mu.Lock()
	b.lastModel = "claude-opus-4-20250514"
	b.lastUsage = &TokenUsage{
		InputTokens:          131,
		OutputTokens:         4, // partial snapshot — the bug value
		CacheReadInputTokens: 90000,
	}
	b.mu.Unlock()

	result := &ResultMessage{
		Subtype: "success",
		Result:  "a long substantive reply",
		Usage:   TokenUsage{OutputTokens: 99}, // all-model fallback (unused: key matches)
		ModelUsage: map[string]ModelUsage{
			"claude-opus-4-20250514":  {OutputTokens: 2187, ContextWindow: 200000},
			"claude-haiku-4-20250514": {OutputTokens: 50}, // subagent — must be excluded
		},
	}
	b.OnResult(result)

	if completedResult == nil || completedResult.Usage == nil {
		t.Fatal("no result usage")
	}
	// Output corrected to the primary model's authoritative total.
	if completedResult.Usage.OutputTokens != 2187 {
		t.Errorf("OutputTokens = %d, want 2187 (from ModelUsage[primary])", completedResult.Usage.OutputTokens)
	}
	// Input/cache untouched — still the final call's context fill.
	if completedResult.Usage.InputTokens != 131 {
		t.Errorf("InputTokens = %d, want 131 (preserved from lastUsage)", completedResult.Usage.InputTokens)
	}
	if completedResult.Usage.CacheReadInputTokens != 90000 {
		t.Errorf("CacheReadInputTokens = %d, want 90000 (preserved from lastUsage)", completedResult.Usage.CacheReadInputTokens)
	}
}

func TestOnResult_OutputFloorFromResultUsageOnKeyMiss(t *testing.T) {
	// When resultModel has no ModelUsage entry, fall back to the result's
	// accumulated all-model total (msg.Usage) as a floor — still far better
	// than the partial lastUsage snapshot.
	t.Parallel()

	var completedResult *delegator.TurnResult
	b := &Backend{}
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	applyHandler(b, handler)

	b.mu.Lock()
	b.lastModel = "claude-opus-4-20250514"
	b.lastUsage = &TokenUsage{InputTokens: 131, OutputTokens: 4}
	b.mu.Unlock()

	result := &ResultMessage{
		Subtype:    "success",
		Result:     "reply",
		Usage:      TokenUsage{OutputTokens: 1500},
		ModelUsage: map[string]ModelUsage{"some-other-model": {OutputTokens: 10}},
	}
	b.OnResult(result)

	if completedResult == nil || completedResult.Usage == nil {
		t.Fatal("no result usage")
	}
	if completedResult.Usage.OutputTokens != 1500 {
		t.Errorf("OutputTokens = %d, want 1500 (msg.Usage floor on key miss)", completedResult.Usage.OutputTokens)
	}
}

func TestOnResult_UsesResultTextWhenPresent(t *testing.T) {
	// Verifies OnResult prefers the result message's Result field over
	// accumulated turnText when both are available.
	t.Parallel()

	var completedResult *delegator.TurnResult

	b := &Backend{}
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	applyHandler(b, handler)

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
	handler := &testHandler{
		OnText:      func(text string) { textEvents = append(textEvents, text) },
		OnToolStart: func(_, name, _ string) { toolStarts = append(toolStarts, name) },
	}
	applyHandler(b, handler)

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
	applyHandler(b, &testHandler{
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
	applyHandler(b, &testHandler{
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
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	applyHandler(b, handler)

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
	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	applyHandler(b, handler)
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
	handler := &testHandler{}
	applyHandler(b, handler)

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
	b.turnEvents = &delegator.TurnEvents{
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
	handler := &testHandler{}
	applyHandler(b, handler)

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
	handler := &testHandler{
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
	applyHandler(b, handler)

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
	handler := &testHandler{
		OnTurnComplete:     func(r *delegator.TurnResult) { completed = r },
		PreAnswerNudgeFunc: func(_ *delegator.TurnResult) string { return "" },
	}
	applyHandler(b, handler)
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
		outstanding:  delegator.NewOutstandingRegistry(),
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
		outstanding:  delegator.NewOutstandingRegistry(),
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
		outstanding:  delegator.NewOutstandingRegistry(),
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
	b.SetOnSubagentStatus(func(string) { called = true })

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
	// With no tracked subagents, task_notification (completed) signals the
	// cleared state with an empty detail (no subagents running).
	t.Parallel()

	statusCalled := false
	var statusText string
	b := &Backend{}
	b.SetOnSubagentStatus(func(detail string) { statusCalled = true; statusText = detail })

	raw, _ := json.Marshal(TaskEvent{
		Subtype: "task_notification",
		Status:  "completed",
		Summary: "Bug is fixed",
	})
	b.OnSystem("task_notification", raw)

	if !statusCalled || statusText != "" {
		t.Errorf("statusText = %q (called=%v), want empty cleared detail", statusText, statusCalled)
	}
}

func TestOnSystem_TaskNotificationCompleted_WithTracked(t *testing.T) {
	// With a tracked subagent, task_notification (completed) removes one from
	// the tracker; the last removal resolves to the empty (cleared) detail.
	t.Parallel()

	var statusText string
	b := &Backend{}
	b.SetOnSubagentStatus(func(detail string) { statusText = detail })
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
	if statusText != "" {
		t.Errorf("statusText = %q, want empty cleared detail", statusText)
	}
}

func TestOnSystem_TaskProgress(t *testing.T) {
	// Verifies OnSystem/task_progress does NOT fire OnStatus.
	t.Parallel()

	var called bool
	b := &Backend{}
	b.SetOnSubagentStatus(func(string) { called = true })

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

	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	applyHandler(b, handler)

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
	handler := &testHandler{}
	applyHandler(b, handler)

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

	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completedResult = r },
	}
	applyHandler(b, handler)

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
// finalizeExit — guards against the wedge-on-death cascade documented in
// TODO #744. Two paths can observe process death (waiter goroutine via
// cmd.Wait, reader goroutine via scanner EOF). Bookkeeping must run exactly
// once regardless of which fires first; if neither fires (the bug we hit),
// the backend is wedged with running=true and an in-flight handler that
// never completes.
// ---------------------------------------------------------------------------

func TestFinalizeExit_RunsOnlyOnce(t *testing.T) {
	// Verifies that calling finalizeExit twice (once per goroutine path)
	// only fires OnTurnComplete once. Without sync.Once, the in-flight
	// handler would receive a duplicate completion and the second call
	// would race with the cleared turnHandler.
	t.Parallel()

	var completionCount int
	b := &Backend{}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()
	b.typingFunc = func(bool) {}

	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completionCount++ },
	}
	applyHandler(b, handler)

	b.finalizeExit(fmt.Errorf("first call"))
	b.finalizeExit(fmt.Errorf("second call"))

	if completionCount != 1 {
		t.Errorf("OnTurnComplete fired %d times, want 1", completionCount)
	}
	if b.IsRunning() {
		t.Error("IsRunning = true after finalizeExit")
	}
}

func TestFinalizeExit_ConcurrentCallsRunOnce(t *testing.T) {
	// Stresses the sync.Once gate: 50 goroutines call finalizeExit
	// concurrently, mimicking the race between waiter and reader paths.
	// Exactly one OnTurnComplete must fire.
	t.Parallel()

	var completionCount atomic.Int32
	b := &Backend{}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()
	b.typingFunc = func(bool) {}

	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) { completionCount.Add(1) },
	}
	applyHandler(b, handler)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			b.finalizeExit(fmt.Errorf("racer"))
		}()
	}
	close(start)
	wg.Wait()

	if got := completionCount.Load(); got != 1 {
		t.Errorf("OnTurnComplete fired %d times under contention, want 1", got)
	}
}

func TestFinalizeExit_SubsequentOnReaderStoppedIsNoOp(t *testing.T) {
	// Simulates the production order: waiter goroutine calls finalizeExit
	// first (because cmd.Wait sees the death), then the reader goroutine
	// belatedly calls OnReaderStopped (which delegates to finalizeExit).
	// The reader path must not re-fire the completion handler or re-flip
	// state. This is the primary guarantee that fixes TODO #744.
	t.Parallel()

	var completionTexts []string
	b := &Backend{}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()
	b.typingFunc = func(bool) {}

	handler := &testHandler{
		OnTurnComplete: func(r *delegator.TurnResult) {
			completionTexts = append(completionTexts, r.Text)
		},
	}
	applyHandler(b, handler)

	// Waiter wins the race.
	b.finalizeExit(fmt.Errorf("exit code 1"))
	// Reader belatedly notices.
	b.OnReaderStopped(fmt.Errorf("scanner: read |0: file already closed"))

	if len(completionTexts) != 1 {
		t.Errorf("OnTurnComplete fired %d times, want 1: %v", len(completionTexts), completionTexts)
	}
	if len(completionTexts) > 0 && !strings.Contains(completionTexts[0], "exit code 1") {
		t.Errorf("completion text = %q, want first-call reason", completionTexts[0])
	}
	if b.IsRunning() {
		t.Error("IsRunning = true after finalizeExit")
	}
}

func TestClose_BoundedWaitWhenWaiterStalls(t *testing.T) {
	// Regression for the 2026-05-06 deadlock: when CC dies but the waiter
	// goroutine fails to deliver to b.waitCh (e.g. a stalled callback inside
	// finalizeExit), Close used to block forever on the post-SIGKILL receive.
	// That kept m.mu held in ResetSession/Get and silently froze every
	// subsequent inbound message for the agent. Close must now return within
	// a bounded time so the caller can release locks and respawn.
	t.Parallel()

	// Shrink production timeouts for the test. Restore on cleanup.
	prevG, prevT, prevK := closeGracefulWait, closeSigtermWait, closeSigkillWait
	closeGracefulWait = 50 * time.Millisecond
	closeSigtermWait = 25 * time.Millisecond
	closeSigkillWait = 25 * time.Millisecond
	t.Cleanup(func() {
		closeGracefulWait = prevG
		closeSigtermWait = prevT
		closeSigkillWait = prevK
	})

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
		cmd:    &exec.Cmd{}, // Process: nil → Signal/Kill skip cleanly
		// waitCh intentionally never receives — simulates the stalled
		// waiter goroutine observed in production.
		waitCh: make(chan error, 1),
		// done already closed so the reader-goroutine wait at the bottom
		// of Close returns immediately.
		done:   closedDone(),
		cancel: func() {},
	}
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()

	// Cap the test to 5x the worst-case bounded shutdown — generous enough to
	// tolerate scheduler jitter, tight enough to fail fast if the bound regresses.
	worst := closeGracefulWait + closeSigtermWait + closeSigkillWait
	deadline := time.Now().Add(5 * worst)

	doneCh := make(chan struct{})
	go func() {
		_ = b.Close()
		close(doneCh)
	}()

	select {
	case <-doneCh:
		// Close returned within the bound — exactly what we want.
	case <-time.After(time.Until(deadline)):
		t.Fatalf("Close did not return within %s; expected ~%s when waiter stalls", 5*worst, worst)
	}
}

func closedDone() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
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
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnTextDelta: func(delta string) { deltas = append(deltas, delta) },
	})

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
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnText: func(text string) { called = true },
	})

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
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnTextDelta:     func(delta string) { textDeltas = append(textDeltas, delta) },
		OnThinkingDelta: func(delta string) { thinkingDeltas = append(thinkingDeltas, delta) },
	})

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

// TestOnStreamEvent_EmptyThinking proves an empty thinking_delta still fires
// OnThinkingDelta. The thinking indicator keys off the presence of thinking
// activity, not content — this model streams thinking with empty plaintext
// (only the signature), so gating on non-empty text would never light it.
func TestOnStreamEvent_EmptyThinking(t *testing.T) {
	t.Parallel()

	var called bool
	b := &Backend{}
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnThinkingDelta: func(delta string) { called = true },
	})

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

	if !called {
		t.Error("OnThinkingDelta should fire on presence, even for empty thinking delta")
	}
}

func TestOnStreamEvent_EmptyText(t *testing.T) {
	// Verifies OnStreamEvent ignores text_delta with empty text.
	t.Parallel()

	var called bool
	b := &Backend{}
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnText: func(text string) { called = true },
	})

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
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnText: func(text string) { called = true },
	})

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
	b.sessionEvents.Store(&delegator.SessionEvents{
		OnTextDelta: func(delta string) { deltas = append(deltas, delta) },
	})

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

func TestGetContextWindow(t *testing.T) {
	// End-to-end test: GetContextWindow sends request, receives routed response.
	t.Parallel()

	// Create a pipe; drain reader so writer.SendControl doesn't block.
	pr, pw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, pr) }()

	b := &Backend{
		writer:          NewWriter(pw),
		pendingControls: make(map[string]chan json.RawMessage),
	}

	// Run GetContextWindow in a goroutine (it blocks waiting for response).
	type result struct {
		wnd *delegator.ContextWindow
		err error
	}
	resCh := make(chan result, 1)
	ctx := context.Background()

	go func() {
		w, err := b.GetContextWindow(ctx)
		resCh <- result{w, err}
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
		t.Fatal("GetContextWindow didn't register a pending control request")
	}

	// Simulate CC returning the response via OnControlResponse.
	resp := fmt.Sprintf(`{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"totalTokens":50000,"maxTokens":200000,"percentage":25,"autoCompactThreshold":160000,"model":"claude-sonnet-4-6"}}}`, reqID)
	b.OnControlResponse(json.RawMessage(resp))

	// Read result.
	r := <-resCh
	if r.err != nil {
		t.Fatalf("GetContextWindow error: %v", r.err)
	}
	if r.wnd.MaxTokens != 200000 {
		t.Errorf("MaxTokens = %d, want 200000", r.wnd.MaxTokens)
	}
	if r.wnd.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want %q", r.wnd.Model, "claude-sonnet-4-6")
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
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { clearedCalled = true })

	// Add a pending permission via both stores (the production path stores
	// in pendingPerms and registers in outstanding atomically).
	b.pendingPerms["req-1"] = &pendingPermission{
		requestID: "req-1",
		toolName:  "Bash",
	}
	b.outstanding.Register("req-1", delegator.OutstandingPermission)

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
		outstanding:  delegator.NewOutstandingRegistry(),
	}
	b.SetOnPromptsCleared(func() { clearedCalled = true })

	b.pendingPerms["req-1"] = &pendingPermission{requestID: "req-1"}
	b.pendingPerms["req-2"] = &pendingPermission{requestID: "req-2"}
	b.outstanding.Register("req-1", delegator.OutstandingPermission)
	b.outstanding.Register("req-2", delegator.OutstandingPermission)

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
		outstanding:  delegator.NewOutstandingRegistry(),
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
		outstanding:  delegator.NewOutstandingRegistry(),
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
		outstanding:  delegator.NewOutstandingRegistry(),
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

func TestClose_KillsLiveProcessWhenNotRunning(t *testing.T) {
	// P1-9: even when `running` has already been flipped to false (e.g. by a
	// finalize path), Close must still run the kill ladder and reap a live
	// subprocess — otherwise the CC process, its goroutines and stdin pipe leak
	// forever. Previously Close early-returned on !running and never killed.
	//
	// Not t.Parallel(): it mutates the package-global closeGracefulWait, which
	// would race the (parallel) TestClose_BoundedWaitWhenWaiterStalls.
	prevG := closeGracefulWait
	closeGracefulWait = 50 * time.Millisecond
	t.Cleanup(func() { closeGracefulWait = prevG })

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	waitCh := make(chan error, 1)
	waited := make(chan struct{})
	go func() {
		waitCh <- cmd.Wait()
		close(waited)
	}()

	var buf bytes.Buffer
	b := &Backend{
		writer: NewWriter(nopWriteCloser{&buf}),
		cmd:    cmd,
		waitCh: waitCh,
		done:   closedDone(),
		cancel: func() {},
	}
	// running stays false — the backend was already marked dead by a finalize.

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-waited:
		// Process was signalled and reaped — correct.
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill() // cleanup so we don't leak the test process
		t.Fatal("live subprocess was not killed by Close when running==false")
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

	handler := &testHandler{
		OnText:         func(string) {},
		OnToolStart:    func(string, string, string) {},
		OnTurnComplete: func(*delegator.TurnResult) {},
	}
	applyHandler(b, handler)

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

package opencode

import (
	"context"
	"errors"
	"os/exec"
	"strings"
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

// DISABLED(opencode): asserts control_request NDJSON wire shape; opencode uses POST /session/:id/abort.
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

// DISABLED(opencode): asserts stdin NDJSON wire shape; opencode uses POST /session/:id/prompt_async.
// // TestSendUserMessage_WireShape verifies the internal sendUserMessage
// // primitive emits a well-formed user message on the wire with the
// // default queue priority (omitted, so CC defaults to "next"). Wire-shape
// // coverage stays here; routing coverage lives in TestInject_*.
// DISABLED(opencode): asserts CC priority field in NDJSON envelope; opencode has no priority concept.
// // TestSendUserMessagePriority_WireShape verifies the priority-bearing
// // primitive emits the priority field in the envelope so CC's queue can
// // dequeue it ahead of default-priority items at the next mid-turn drain.
// ---------------------------------------------------------------------------
// Inject — canonical entry point for user-role events
// ---------------------------------------------------------------------------

// DISABLED(opencode): asserts writer (stdin) buffer; opencode sends prompts via HTTP POST.
// // TestInject_User_Idle_BeginsTurn verifies Inject(SourceUser) at idle
// // dispatches to the begin-turn path: turnActive becomes true, the handler
// // is installed, and the writer receives the text.
// DISABLED(opencode): asserts writer (stdin) buffer for attachment blocks; opencode uses file parts in HTTP body.
// // TestInject_User_Idle_WithAttachments verifies Inject routes attachments
// // through the structured-content path when a turn is being begun. The
// // writer should see image/document blocks alongside the text.
// DISABLED(opencode): asserts CC mid-turn drain via sendUserMessage; opencode buffers to steerBuf instead.
// // TestInject_User_InFlight_FoldsViaSendUser verifies Inject(SourceUser)
// // during an in-flight turn queues the text via SendUser at default
// // priority (no priority field on the wire; CC defaults to "next"). CC's
// // mid-turn drain at the next tool boundary folds the message into the
// // current ask() as an attachment — there is no rearm bookkeeping and
// // no separate result cycle to wait for.
// DISABLED(opencode): asserts CC priority='now' field; opencode has no priority concept.
// // TestInject_Steer_InFlight_NoInterrupt_PriorityNow verifies that
// // Inject(SourceSteer) during an in-flight turn does NOT call Interrupt
// // (the rearm-cascade era required interrupting; we now rely on CC's
// // mid-turn drain instead). The steer text is queued at priority "now"
// // so it dequeues ahead of any other queued items at the next tool
// // boundary, without aborting the in-flight tool.
// TODO(opencode): rewrite — opencode has no mid-turn queue; steers buffer-and-flush instead; see plan section 6.
// // TestInject_Steer_Idle_BeginsTurn verifies the edge case where a steer
// // arrives at idle (race between turn end and platform queue dispatch) and
// // carries turn bookkeeping (here via the legacy Handler, which the shim turns
// // into a Turn): Inject degrades to a fresh begin-turn rather than calling
// // Interrupt on a non-existent in-flight turn. (The Turn-less inbox shape is
// // covered by TestInject_Steer_Idle_NoTurn_ReturnsErrTurnNotInFlight.)
// TODO(opencode): rewrite — opencode has no mid-turn queue; steers buffer-and-flush instead; see plan section 6.
// // TestInject_Steer_Idle_NoTurn_ReturnsErrTurnNotInFlight verifies the inbox
// // race fix: a steer that arrives at idle with neither Turn nor Handler (the
// // shape the inbox builds) is declined with ErrTurnNotInFlight rather than
// // beginning an untracked turn (nil TurnEvents → no OnTurnComplete). No turn
// // must start and nothing must be written to the backend.
// DISABLED(opencode): asserts /compact slash command on stdin; opencode uses POST /session/:id/command.
// // TestInject_Compact verifies Inject(SourceCompact) at idle sends the
// // slash command via SendUser. /compact is fire-and-forget: the response
// // (compact_boundary system event) flows through CompactionWaiter.
// DISABLED(opencode): asserts /compact slash command on stdin; opencode uses POST /session/:id/command.
// // TestInject_Compact_InFlight verifies the slash-command path is
// // callable mid-turn without disturbing turn state. /compact shouldn't
// // normally be invoked mid-turn, but if it is, the call must succeed.
// DISABLED(opencode): asserts slash command on stdin; opencode uses POST /session/:id/command.
// // TestInject_Pass mirrors Compact for the /pass passthrough path —
// // slash commands like /context, /model are sent via SendUser and don't
// // disturb turn state.
// ---------------------------------------------------------------------------
// SendToPane
// ---------------------------------------------------------------------------

// DISABLED(opencode): asserts sendToPane stdin NDJSON wire shape; opencode sends via HTTP POST.
// DISABLED(opencode): asserts sendToPaneWithAttachments stdin wire shape; opencode uses file parts in HTTP body.
// DISABLED(opencode): asserts writer (stdin) error handling; opencode has no stdin writer.
// ---------------------------------------------------------------------------
// OnAssistant
// ---------------------------------------------------------------------------

// DISABLED(opencode): uses ccstream AssistantMessage/BetaMessage/ContentBlock types; opencode uses message.part.updated events (Step 7).
// DISABLED(opencode): uses ccstream ContentBlock types; opencode uses message.part.updated events (Step 7).
// // TestOnAssistant_CrossMessageSeparation pins TODO #819: text from separate
// // assistant messages within one turn (segments split by tool calls) must be
// // joined with a blank line, not glued. Pre-tool-call narration was arriving
// // concatenated onto the next segment (e.g. "...correctly.Καλημέρα").
// DISABLED(opencode): uses ccstream BetaMessage/ContentBlock types; opencode uses message.part.updated events (Step 7).
// // TestTextDelivery_NoHandler_GoesToSessionEvents is the regression test for
// // TODO #747: text emitted AFTER a turn's OnResult clears the per-turn
// // TurnEvents must still deliver via the always-live SessionEvents path.
// //
// // Before the SessionEvents/TurnEvents split, ccstream's text-emit path read
// // `b.turnHandler` under turnMu and dropped on nil with a "text block dropped:
// // handler_nil=true" warning — re-exposed by commit 57445dc6 which removed
// // the rearm cascade that had been keeping turnHandler non-nil across stacked
// // CC results. The fix divorces delivery (session lifetime) from bookkeeping
// // (turn lifetime). This test pins that invariant: drive a turn to completion,
// // then emit more text, and assert the SessionEvents.OnText callback still
// // fires.
// DISABLED(opencode): uses ccstream ContentBlock tool_use type; opencode uses tool parts (Step 7).
// DISABLED(opencode): uses ccstream BetaMessage/TokenUsage types; opencode uses message.updated events (Step 7).
// DISABLED(opencode): uses ccstream BetaMessage/ContentBlock types; opencode uses message.updated events (Step 7).
// DISABLED(opencode): uses ccstream BetaMessage stop_reason typing logic; opencode uses session.status events (Step 7).
// DISABLED(opencode): uses ccstream BetaMessage stop_reason typing logic; opencode uses session.status events (Step 7).
// DISABLED(opencode): uses ccstream BetaMessage/ContentBlock types; opencode uses message.part.updated events (Step 7).
// DISABLED(opencode): uses ccstream BetaMessage/ContentBlock types; opencode uses message.part.updated events (Step 7).
// DISABLED(opencode): uses ccstream ContentBlock thinking type; opencode uses reasoning parts (Step 7).
// DISABLED(opencode): uses ccstream ContentBlock tool_use + AgentTracker; opencode uses tool parts (Step 7).
// DISABLED(opencode): uses ccstream ContentBlock tool_use type; opencode uses tool parts (Step 7).
// DISABLED(opencode): uses ccstream ResultMessage type; opencode uses session.idle events (Step 7).
// ---------------------------------------------------------------------------
// OnResult
// ---------------------------------------------------------------------------

// DISABLED(opencode): uses ccstream ResultMessage/TokenUsage types; opencode uses session.idle events (Step 7).
// DISABLED(opencode): uses ccstream ModelUsage correction logic; opencode uses message.tokens directly (Step 7).
// DISABLED(opencode): uses ccstream ModelUsage fallback logic; opencode uses message.tokens directly (Step 7).
// DISABLED(opencode): uses ccstream ResultMessage type; opencode uses accumulated message.part.updated text (Step 7).
// DISABLED(opencode): uses ccstream ParentToolUseID; opencode subtasks are subtask parts on parent message (Step 7).
// // TestOnAssistant_SubagentSurfacesBlockquotedText proves that a subagent
// // assistant message (ParentToolUseID != nil) fires OnText with blockquoted
// // text so the user can follow sub-agent progress, but does NOT fire
// // OnToolStart (the parent tracker owns tool visibility) and does NOT mutate
// // the parent's per-turn accumulator state (turnText, turnTools).
// DISABLED(opencode): uses ccstream ParentToolUseID; opencode subtasks are subtask parts on parent message (Step 7).
// // TestOnAssistant_SubagentMultilineBlockquote verifies that multiline
// // sub-agent text gets every line prefixed with "> ".
// DISABLED(opencode): uses ccstream ParentToolUseID; opencode subtasks are subtask parts on parent message (Step 7).
// // TestOnAssistant_SubagentEmptyTextSkipped verifies that empty sub-agent
// // text blocks do not fire OnText.
// DISABLED(opencode): uses ccstream ParentToolUseID + ModelUsage; opencode subtasks are subtask parts (Step 7).
// DISABLED(opencode): uses ccstream ResultMessage/TokenUsage types; opencode uses session.idle events (Step 7).
// DISABLED(opencode): uses ccstream ResultMessage type; opencode uses session.idle events (Step 7).
// DISABLED(opencode): uses ccstream ResultMessage type; opencode uses session.idle events (Step 7).
// DISABLED(opencode): uses ccstream ResultMessage type; opencode uses session.idle events (Step 7).
// DISABLED(opencode): uses ccstream ResultMessage/TokenUsage types; opencode uses session.idle events (Step 7).
// DISABLED(opencode): uses ccstream ResultMessage/ModelUsage + writer pre-answer loop; opencode uses session.idle events (Step 7).
// DISABLED(opencode): uses ccstream ResultMessage/ModelUsage + writer; opencode uses session.idle events (Step 7).
// ---------------------------------------------------------------------------
// OnSystem
// ---------------------------------------------------------------------------

// DISABLED(opencode): asserts ccstream system/init handshake; opencode uses POST /session response (Step 5).
// DISABLED(opencode): asserts ccstream system/init handshake; opencode uses POST /session response (Step 5).
// DISABLED(opencode): asserts ccstream system/init dispatch; opencode has no OnSystem method.
// DISABLED(opencode): asserts ccstream status/compacting dispatch; opencode uses session.status SSE events.
// DISABLED(opencode): asserts ccstream status dispatch; opencode uses session.status SSE events.
// DISABLED(opencode): asserts ccstream status dispatch; opencode uses session.status SSE events.
// DISABLED(opencode): asserts ccstream compact_boundary dispatch; opencode uses session.compacted SSE events.
// DISABLED(opencode): asserts ccstream compact_boundary dispatch; opencode uses session.compacted SSE events.
// DISABLED(opencode): uses ccstream CompactBoundaryMessage via OnSystem; opencode uses session.compacted SSE events (Step 8).
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

// DISABLED(opencode): uses ccstream CompactBoundaryMessage via OnSystem; opencode uses session.compacted SSE events (Step 8).
// DISABLED(opencode): asserts ccstream task_started dispatch; opencode has no equivalent.
// DISABLED(opencode): asserts ccstream task_notification dispatch; opencode has no equivalent.
// DISABLED(opencode): asserts ccstream task_notification + AgentTracker; opencode has no equivalent.
// DISABLED(opencode): asserts ccstream task_progress dispatch; opencode has no equivalent.
// DISABLED(opencode): asserts ccstream OnSystem dispatch; opencode has no OnSystem method.
// DISABLED(opencode): asserts ccstream OnSystem dispatch; opencode has no OnSystem method.
// ---------------------------------------------------------------------------
// OnReaderStopped
// ---------------------------------------------------------------------------

// DISABLED(opencode): ccstream reader-death dispatch; opencode SSE subscriber has different lifecycle (Step 3/4).
// DISABLED(opencode): ccstream reader-death dispatch; opencode SSE subscriber has different lifecycle (Step 3/4).
// DISABLED(opencode): ccstream reader-death dispatch; opencode SSE subscriber has different lifecycle (Step 3/4).
// DISABLED(opencode): ccstream reader-death dispatch; opencode SSE subscriber has different lifecycle (Step 3/4).
// DISABLED(opencode): ccstream reader-death dispatch; opencode SSE subscriber has different lifecycle (Step 3/4).
// ---------------------------------------------------------------------------
// finalizeExit — guards against the wedge-on-death cascade documented in
// TODO #744. Two paths can observe process death (waiter goroutine via
// cmd.Wait, reader goroutine via scanner EOF). Bookkeeping must run exactly
// once regardless of which fires first; if neither fires (the bug we hit),
// the backend is wedged with running=true and an in-flight handler that
// never completes.
// ---------------------------------------------------------------------------

// DISABLED(opencode): ccstream finalizeExit subprocess death cascade; opencode Server lifecycle is different (Step 3).
// DISABLED(opencode): ccstream finalizeExit subprocess death cascade; opencode Server lifecycle is different (Step 3).
// DISABLED(opencode): ccstream finalizeExit subprocess death cascade; opencode Server lifecycle is different (Step 3).
// DISABLED(opencode): ccstream subprocess kill-ladder timing; opencode Server Close mirrors this but different struct (Step 3).
func closedDone() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// ---------------------------------------------------------------------------
// OnToolProgress
// ---------------------------------------------------------------------------

// DISABLED(opencode): ccstream tool_progress via external hook; opencode emits tool parts directly (Step 7).
// ---------------------------------------------------------------------------
// OnStreamEvent
// ---------------------------------------------------------------------------

// DISABLED(opencode): ccstream stream_event dispatch (content_block_delta); opencode uses message.part.updated events (Step 7).
// DISABLED(opencode): ccstream stream_event dispatch; opencode uses message.part.updated events (Step 7).
// DISABLED(opencode): ccstream stream_event dispatch (thinking_delta); opencode uses message.part.updated events (Step 7).
// // TestOnStreamEvent_ThinkingDelta proves that thinking_delta subtypes in
// // content_block_delta stream events dispatch to OnThinkingDelta (not
// // OnTextDelta). This is the path that carries per-token thinking streaming
// // from CC — previously dropped, now wired through turnevent.ThinkingDelta.
// DISABLED(opencode): ccstream stream_event dispatch; opencode uses message.part.updated events (Step 7).
// // TestOnStreamEvent_EmptyThinking proves empty thinking_delta payloads are
// // dropped, matching the existing text_delta behaviour and keeping downstream
// // emit helpers from firing no-op events.
// DISABLED(opencode): ccstream stream_event dispatch; opencode uses message.part.updated events (Step 7).
// DISABLED(opencode): ccstream stream_event dispatch; opencode uses message.part.updated events (Step 7).
// DISABLED(opencode): ccstream stream_event dispatch; opencode uses message.part.updated events (Step 7).
// DISABLED(opencode): ccstream stream_event + parent_tool_use_id filter; opencode uses subtask parts (Step 7).
// // TestOnStreamEvent_SubagentFiltered proves that stream events with
// // parent_tool_use_id set are filtered out, matching the guard in OnAssistant.
// // Without this, sub-agent deltas leak into the parent turn's StreamWriter.
// ---------------------------------------------------------------------------
// OnControlResponse / OnControlCancelRequest
// ---------------------------------------------------------------------------

// DISABLED(opencode): ccstream control_response routing via pendingControls; opencode has no control_response equivalent.
// DISABLED(opencode): ccstream control_response routing via pendingControls; opencode has no control_response equivalent.
// DISABLED(opencode): ccstream get_context_usage control_request; opencode has no equivalent (Step 8.3 — skipped for v1).
// DISABLED(opencode): ccstream control_request cancel dispatch; opencode permission lifecycle is different (Step 9).
// DISABLED(opencode): ccstream control_request cancel dispatch; opencode permission lifecycle is different (Step 9).
// ---------------------------------------------------------------------------
// WaitReady
// ---------------------------------------------------------------------------

// DISABLED(opencode): ccstream init handshake via readyCh; opencode uses POST /session response (Step 5).
// DISABLED(opencode): ccstream init handshake via readyCh; opencode uses POST /session response (Step 5).
// DISABLED(opencode): ccstream system/init handshake; opencode uses POST /session response (Step 5).
// DISABLED(opencode): ccstream control_response init handshake; opencode uses POST /session response (Step 5).
// DISABLED(opencode): ccstream control_response init filter; opencode uses POST /session response (Step 5).
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

// DISABLED(opencode): ccstream subprocess kill-ladder; opencode Server Close has different struct (Step 3).
// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// DISABLED(opencode): uses ccstream OnAssistant/OnResult types; opencode dispatch is event-driven (Step 7).
// ---------------------------------------------------------------------------
// logComponent
// ---------------------------------------------------------------------------

// DISABLED(opencode): asserts 'ccstream' tag; opencode tag is 'opencode'.
// DISABLED(opencode): asserts 'ccstream' tag; opencode tag is 'opencode'.
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

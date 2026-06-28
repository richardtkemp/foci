package opencode

import (
	"encoding/json"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

// newHandlerTestBackend returns a Backend wired with capturing
// SessionEvents callbacks. Tests call the On* methods directly to
// simulate event dispatch without spinning up HTTP or SSE.
func newHandlerTestBackend(t *testing.T) *Backend {
	t.Helper()
	b := &Backend{
		sessionID:     "sess-test",
		readyCh:       make(chan struct{}),
		outstanding:   delegator.NewOutstandingRegistry(),
		compactDoneCh: make(chan struct{}, 1),
	}

	var texts, textDeltas, thinkingDeltas []string
	var toolStarts, toolEnds []toolCall
	var subagentTexts []string
	var completed *delegator.TurnResult

	b.AttachSessionEvents(&delegator.SessionEvents{
		OnText:          func(text string) { texts = append(texts, text) },
		OnTextDelta:     func(d string) { textDeltas = append(textDeltas, d) },
		OnThinkingDelta: func(d string) { thinkingDeltas = append(thinkingDeltas, d) },
		OnToolStart:     func(id, name, input string) { toolStarts = append(toolStarts, toolCall{id, name, input}) },
		OnToolEnd:       func(id, name, output string, isErr bool) { toolEnds = append(toolEnds, toolCall{id, name, output}) },
		OnSubagentText:  func(group, text string) { subagentTexts = append(subagentTexts, text) },
	})
	b.beginTurn(&delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) { completed = r },
	})

	// Stash capture slices on the Backend via a test-only struct so
	// tests can read them. Go doesn't have anonymous-type fields, so
	// we use a package-level map keyed by pointer. (Simpler: tests
	// just read the closures' state through helper accessors below.)
	t.Cleanup(func() { _ = b.Close() })

	// Expose captures via a side-channel.
	captures := &handlerCaptures{
		texts:          &texts,
		textDeltas:     &textDeltas,
		thinkingDeltas: &thinkingDeltas,
		toolStarts:     &toolStarts,
		toolEnds:       &toolEnds,
		subagentTexts:  &subagentTexts,
		completed:      &completed,
	}
	handlerCaptureMap[b] = captures
	return b
}

type toolCall struct {
	id, name, detail string
}

type handlerCaptures struct {
	texts, textDeltas, thinkingDeltas, subagentTexts *[]string
	toolStarts, toolEnds                             *[]toolCall
	completed                                        **delegator.TurnResult
}

var handlerCaptureMap = map[*Backend]*handlerCaptures{}

func (b *Backend) captures() *handlerCaptures {
	return handlerCaptureMap[b]
}

// ---------------------------------------------------------------------------
// message.part.updated — text
// ---------------------------------------------------------------------------

func TestOnMessagePartUpdated_TextDeltaFiresOnTextDelta(t *testing.T) {
	// Verifies that a text part update with a non-empty delta fires
	// OnTextDelta (for streaming display) but NOT OnText (which fires
	// only on part completion).
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onMessagePartUpdated(Part{
		Type: PartText,
		ID:   "pt-1",
		Text: "Hello",
		Time: &PartTime{Start: 1000}, // no End → not complete
	}, "Hello")

	if len(*c.textDeltas) != 1 || (*c.textDeltas)[0] != "Hello" {
		t.Errorf("textDeltas = %v, want [Hello]", *c.textDeltas)
	}
	if len(*c.texts) != 0 {
		t.Errorf("OnText fired %d times on a non-complete part, want 0", len(*c.texts))
	}
}

func TestOnMessagePartUpdated_TextCompleteFiresOnText(t *testing.T) {
	// Verifies that when a text part has Time.End set (complete),
	// OnText fires with the full accumulated text AND the text is
	// accumulated into turnText for the TurnResult.
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onMessagePartUpdated(Part{
		Type: PartText,
		ID:   "pt-1",
		Text: "Hello world",
		Time: &PartTime{Start: 1000, End: 2000},
	}, "")

	if len(*c.texts) != 1 || (*c.texts)[0] != "Hello world" {
		t.Errorf("texts = %v, want [Hello world]", *c.texts)
	}
	b.turnMu.Lock()
	text := b.turnText.String()
	b.turnMu.Unlock()
	if text != "Hello world" {
		t.Errorf("turnText = %q, want %q", text, "Hello world")
	}
}

func TestOnMessagePartUpdated_SyntheticTextIgnored(t *testing.T) {
	// Verifies synthetic:true parts are silently dropped — the server
	// injects UI banners (e.g., "Compacting...") that foci must not
	// surface as model text.
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onMessagePartUpdated(Part{
		Type:      PartText,
		ID:        "pt-syn",
		Text:      "server banner",
		Synthetic: true,
		Time:      &PartTime{Start: 1000, End: 2000},
	}, "server banner")

	if len(*c.texts) != 0 {
		t.Errorf("synthetic text fired OnText %d times, want 0", len(*c.texts))
	}
	if len(*c.textDeltas) != 0 {
		t.Errorf("synthetic text fired OnTextDelta %d times, want 0", len(*c.textDeltas))
	}
}

func TestOnMessagePartUpdated_TextPartNotDoubleFired(t *testing.T) {
	// Verifies a text part's OnText fires exactly once even if
	// opencode re-sends the complete event. Dedup is via seenTextParts.
	b := newHandlerTestBackend(t)
	c := b.captures()

	for i := 0; i < 3; i++ {
		b.onMessagePartUpdated(Part{
			Type: PartText,
			ID:   "pt-dup",
			Text: "same text",
			Time: &PartTime{Start: 1000, End: 2000},
		}, "")
	}
	if len(*c.texts) != 1 {
		t.Errorf("OnText fired %d times for same part ID, want 1", len(*c.texts))
	}
}

func TestOnMessagePartUpdated_TextCrossPartSeparatedByBlankLine(t *testing.T) {
	// Verifies multiple text parts in one turn are accumulated into
	// turnText separated by \n\n — mirrors ccstream's cross-message
	// separation rule.
	b := newHandlerTestBackend(t)

	for _, p := range []struct {
		id, text string
	}{
		{"pt-a", "first segment"},
		{"pt-b", "second segment"},
	} {
		b.onMessagePartUpdated(Part{
			Type: PartText, ID: p.id, Text: p.text,
			Time: &PartTime{Start: 1000, End: 2000},
		}, "")
	}
	b.turnMu.Lock()
	got := b.turnText.String()
	b.turnMu.Unlock()
	want := "first segment\n\nsecond segment"
	if got != want {
		t.Errorf("turnText = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// message.part.updated — reasoning
// ---------------------------------------------------------------------------

func TestOnMessagePartUpdated_ReasoningFiresOnThinkingDelta(t *testing.T) {
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onMessagePartUpdated(Part{
		Type: PartReasoning,
		ID:   "pr-1",
		Text: "Let me think...",
	}, "Let me think...")

	if len(*c.thinkingDeltas) != 1 || (*c.thinkingDeltas)[0] != "Let me think..." {
		t.Errorf("thinkingDeltas = %v, want [Let me think...]", *c.thinkingDeltas)
	}
}

// ---------------------------------------------------------------------------
// message.part.updated — tool lifecycle
// ---------------------------------------------------------------------------

func TestOnMessagePartUpdated_ToolRunningFiresOnToolStart(t *testing.T) {
	// Verifies state.status=="running" fires OnToolStart and
	// increments turnTools.
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onMessagePartUpdated(Part{
		Type:   PartTool,
		ID:     "tool-1",
		CallID: "call-abc",
		Tool:   "bash",
		State: &ToolState{
			Status: ToolStateRunning,
			Input:  json.RawMessage(`{"command":"ls"}`),
		},
	}, "")

	if len(*c.toolStarts) != 1 {
		t.Fatalf("toolStarts = %d, want 1", len(*c.toolStarts))
	}
	tc := (*c.toolStarts)[0]
	if tc.id != "call-abc" || tc.name != "bash" || tc.detail != `{"command":"ls"}` {
		t.Errorf("toolStart = %+v", tc)
	}
	b.turnMu.Lock()
	tools := b.turnTools
	b.turnMu.Unlock()
	if tools != 1 {
		t.Errorf("turnTools = %d, want 1", tools)
	}
}

func TestOnMessagePartUpdated_ToolCompletedFiresOnToolEnd(t *testing.T) {
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onMessagePartUpdated(Part{
		Type:   PartTool,
		ID:     "tool-1",
		CallID: "call-abc",
		Tool:   "bash",
		State: &ToolState{
			Status: ToolStateCompleted,
			Output: "total 0",
		},
	}, "")

	if len(*c.toolEnds) != 1 {
		t.Fatalf("toolEnds = %d, want 1", len(*c.toolEnds))
	}
	tc := (*c.toolEnds)[0]
	if tc.id != "call-abc" || tc.name != "bash" || tc.detail != "total 0" {
		t.Errorf("toolEnd = %+v", tc)
	}
}

func TestOnMessagePartUpdated_ToolErrorFiresOnToolEndWithError(t *testing.T) {
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onMessagePartUpdated(Part{
		Type:   PartTool,
		ID:     "tool-1",
		CallID: "call-abc",
		Tool:   "bash",
		State: &ToolState{
			Status: ToolStateError,
			Error:  "command not found",
		},
	}, "")

	if len(*c.toolEnds) != 1 {
		t.Fatalf("toolEnds = %d, want 1", len(*c.toolEnds))
	}
	tc := (*c.toolEnds)[0]
	if tc.detail != "command not found" {
		t.Errorf("toolEnd output = %q, want %q", tc.detail, "command not found")
	}
}

func TestOnMessagePartUpdated_ToolRunningDedupedByCallID(t *testing.T) {
	// Verifies OnToolStart fires once per callID even if opencode
	// re-emits "running" for the same call (e.g., partial updates).
	b := newHandlerTestBackend(t)
	c := b.captures()

	for i := 0; i < 3; i++ {
		b.onMessagePartUpdated(Part{
			Type:   PartTool,
			ID:     "tool-1",
			CallID: "call-dup",
			Tool:   "bash",
			State:  &ToolState{Status: ToolStateRunning, Input: json.RawMessage(`{}`)},
		}, "")
	}
	if len(*c.toolStarts) != 1 {
		t.Errorf("OnToolStart fired %d times for same callID, want 1", len(*c.toolStarts))
	}
	b.turnMu.Lock()
	tools := b.turnTools
	b.turnMu.Unlock()
	if tools != 1 {
		t.Errorf("turnTools = %d, want 1 (deduped)", tools)
	}
}

// ---------------------------------------------------------------------------
// message.part.updated — subtask
// ---------------------------------------------------------------------------

func TestOnMessagePartUpdated_SubtaskSurfacesBlockquotedDescription(t *testing.T) {
	// Verifies subtask descriptions are surfaced via OnSubagentText
	// with a "> " blockquote prefix — mirrors ccstream's blockquote
	// rule for subagent visibility.
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onMessagePartUpdated(Part{
		Type:        PartSubtask,
		ID:          "subtask-1",
		Description: "Searching for foo() usages",
	}, "")

	if len(*c.subagentTexts) != 1 {
		t.Fatalf("subagentTexts = %d, want 1", len(*c.subagentTexts))
	}
	want := "> Searching for foo() usages"
	if (*c.subagentTexts)[0] != want {
		t.Errorf("subagentText = %q, want %q", (*c.subagentTexts)[0], want)
	}
}

// ---------------------------------------------------------------------------
// message.updated — model + usage extraction
// ---------------------------------------------------------------------------

func TestOnMessageUpdated_AssistantSetsModelAndUsage(t *testing.T) {
	// Verifies OnMessageUpdated stores model + tokens for the
	// TurnResult that OnSessionIdle will build.
	b := newHandlerTestBackend(t)

	b.onMessageUpdated(Message{
		Role:       "assistant",
		ModelID:    "claude-sonnet-4",
		ProviderID: "anthropic",
		Tokens: &MessageTokens{
			Input:  1234,
			Output: 567,
			Cache:  struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		}{Read: 8900, Write: 1100},
		},
	})

	b.mu.Lock()
	model := b.lastModel
	usage := b.lastUsage
	b.mu.Unlock()

	if model != "claude-sonnet-4" {
		t.Errorf("lastModel = %q", model)
	}
	if usage == nil {
		t.Fatal("lastUsage nil")
	}
	if usage.InputTokens != 1234 || usage.OutputTokens != 567 {
		t.Errorf("usage = input:%d output:%d", usage.InputTokens, usage.OutputTokens)
	}
	if usage.CacheReadInputTokens != 8900 {
		t.Errorf("cache.read = %d", usage.CacheReadInputTokens)
	}
}

func TestOnMessageUpdated_AssistantEmptyModelPreserved(t *testing.T) {
	// Verifies OnMessageUpdated does NOT overwrite lastModel with an
	// empty string (partial/streaming messages may omit modelID).
	b := newHandlerTestBackend(t)

	b.mu.Lock()
	b.lastModel = "claude-sonnet-4"
	b.mu.Unlock()

	b.onMessageUpdated(Message{
		Role:    "assistant",
		ModelID: "", // empty — should not overwrite
	})

	b.mu.Lock()
	model := b.lastModel
	b.mu.Unlock()
	if model != "claude-sonnet-4" {
		t.Errorf("lastModel = %q, want preserved %q", model, "claude-sonnet-4")
	}
}

func TestOnMessageUpdated_ProviderAuthErrorFiresOnAuthFailure(t *testing.T) {
	// Verifies a ProviderAuthError in message.updated fires the
	// onAuthFailure callback with the provider's error message. This
	// is the detection path Step 11 wires into the relogin gate.
	var authDetail string
	var authFired bool
	b := &Backend{
		sessionID:     "sess-test",
		compactDoneCh: make(chan struct{}, 1),
		outstanding:   delegator.NewOutstandingRegistry(),
	}
	b.mu.Lock()
	b.onAuthFailure = func(d string) {
		authFired = true
		authDetail = d
	}
	b.mu.Unlock()

	errData, _ := json.Marshal(ProviderAuthErrorData{
		ProviderID: "anthropic",
		Message:    "invalid api key",
	})
	b.onMessageUpdated(Message{
		Role:  "assistant",
		Error: &MessageError{Name: ErrProviderAuth, Data: errData},
	})

	if !authFired {
		t.Fatal("onAuthFailure was not called for ProviderAuthError")
	}
	if authDetail != "invalid api key" {
		t.Errorf("authDetail = %q, want %q", authDetail, "invalid api key")
	}
}

func TestOnSessionError_ProviderAuthErrorFiresOnAuthFailure(t *testing.T) {
	// Verifies the same auth-failure detection path via session.error
	// events — the other place opencode surfaces ProviderAuthError.
	var authFired bool
	b := &Backend{
		sessionID:     "sess-test",
		compactDoneCh: make(chan struct{}, 1),
		outstanding:   delegator.NewOutstandingRegistry(),
	}
	b.mu.Lock()
	b.onAuthFailure = func(d string) { authFired = true }
	b.mu.Unlock()

	errData, _ := json.Marshal(ProviderAuthErrorData{
		ProviderID: "anthropic",
		Message:    "expired token",
	})
	b.onSessionError("sess-test", &MessageError{Name: ErrProviderAuth, Data: errData})

	if !authFired {
		t.Fatal("onAuthFailure was not called for session.error ProviderAuthError")
	}
}

func TestOnSessionError_MessageAbortedDoesNotFireAuthFailure(t *testing.T) {
	// Verifies MessageAbortedError (expected on /reset hard) does NOT
	// fire onAuthFailure — it's a user-initiated cancel, not an auth
	// failure.
	var authFired bool
	b := &Backend{
		sessionID:     "sess-test",
		compactDoneCh: make(chan struct{}, 1),
		outstanding:   delegator.NewOutstandingRegistry(),
	}
	b.mu.Lock()
	b.onAuthFailure = func(d string) { authFired = true }
	b.mu.Unlock()

	errData, _ := json.Marshal(MessageAbortedErrorData{Message: "user aborted"})
	b.onSessionError("sess-test", &MessageError{Name: ErrMessageAborted, Data: errData})

	if authFired {
		t.Error("onAuthFailure fired for MessageAbortedError — should only fire for auth failures")
	}
}

func TestOnMessageUpdated_NonAssistantIgnored(t *testing.T) {
	// Verifies user-role messages don't overwrite model/usage.
	b := newHandlerTestBackend(t)

	b.mu.Lock()
	b.lastModel = "prior-model"
	b.mu.Unlock()

	b.onMessageUpdated(Message{Role: "user", ModelID: "should-not-stick"})

	b.mu.Lock()
	model := b.lastModel
	b.mu.Unlock()
	if model != "prior-model" {
		t.Errorf("user message overwrote lastModel to %q", model)
	}
}

func TestOnMessageUpdated_FinishSetDoesNotFireTurnCompleteEarly(t *testing.T) {
	// Verifies the "wait for session.idle, not for the last assistant
	// message" invariant: onMessageUpdated with finish != "" and
	// time.completed != 0 stores model+usage but does NOT fire
	// OnTurnComplete. That only happens when session.idle arrives,
	// so any straggler message.part.updated events land first.
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onMessageUpdated(Message{
		Role:    "assistant",
		ModelID: "claude-sonnet-4",
		Finish:  "stop",
		Time:    MessageTime{Created: 1000, Completed: 2000},
	})

	if *c.completed != nil {
		t.Error("OnTurnComplete fired on message.updated — should only fire on session.idle")
	}

	// Model + usage should still be stored (for the TurnResult that
	// OnSessionIdle will build).
	b.mu.Lock()
	model := b.lastModel
	b.mu.Unlock()
	if model != "claude-sonnet-4" {
		t.Errorf("model = %q, want stored for later", model)
	}
}

// ---------------------------------------------------------------------------
// session.idle — turn completion
// ---------------------------------------------------------------------------

func TestOnSessionIdle_BuildsTurnResultFromAccumulatedState(t *testing.T) {
	// Verifies OnSessionIdle builds a TurnResult from accumulated
	// turnText + turnTools + lastModel + lastUsage, fires
	// OnTurnComplete, clears turn state, and signals turnResultCh.
	b := newHandlerTestBackend(t)
	c := b.captures()

	// Simulate accumulated state.
	b.turnMu.Lock()
	b.turnText.WriteString("the answer is 42")
	b.turnTools = 3
	b.turnMu.Unlock()
	b.mu.Lock()
	b.lastModel = "claude-sonnet-4"
	b.lastUsage = &TokenUsage{InputTokens: 100, OutputTokens: 50}
	b.mu.Unlock()

	b.onSessionIdle("sess-test")

	if *c.completed == nil {
		t.Fatal("OnTurnComplete was not called")
	}
	r := *c.completed
	if r.Text != "the answer is 42" {
		t.Errorf("result.Text = %q", r.Text)
	}
	if r.ToolCalls != 3 {
		t.Errorf("result.ToolCalls = %d, want 3", r.ToolCalls)
	}
	if r.Model != "claude-sonnet-4" {
		t.Errorf("result.Model = %q", r.Model)
	}
	if r.Usage == nil || r.Usage.InputTokens != 100 || r.Usage.OutputTokens != 50 {
		t.Errorf("result.Usage = %+v", r.Usage)
	}

	// Turn state should be cleared.
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight = true after onSessionIdle")
	}

	// WaitForTurn signal.
	b.turnMu.Lock()
	ch := b.turnResultCh
	b.turnMu.Unlock()
	if ch == nil {
		t.Error("turnResultCh nil after idle (should be signalled)")
	}
}

func TestOnSessionIdle_WrongSessionIgnored(t *testing.T) {
	// Verifies OnSessionIdle ignores events for other sessions
	// (defensive — shouldn't happen given per-session routing).
	b := newHandlerTestBackend(t)
	c := b.captures()

	b.onSessionIdle("other-session")

	if *c.completed != nil {
		t.Error("OnTurnComplete fired for wrong session")
	}
}

func TestOnSessionIdle_PreAnswerNudgeGate(t *testing.T) {
	// Verifies the PreAnswerNudgeFunc gate: if it returns non-empty
	// text, the turn is NOT completed — instead a follow-up prompt is
	// sent (re-begin turn). Mirrors ccstream's two-round logic.
	b := newHandlerTestBackend(t)
	c := b.captures()

	var nudgeMu sync.Mutex
	var nudgeFired bool
	b.turnMu.Lock()
	b.turnEvents = &delegator.TurnEvents{
		OnTurnComplete: func(r *delegator.TurnResult) {},
		PreAnswerNudgeFunc: func(r *delegator.TurnResult) string {
			nudgeMu.Lock()
			nudgeFired = true
			nudgeMu.Unlock()
			return "please verify your answer"
		},
	}
	b.turnMu.Unlock()

	// We can't call sendPrompt (no server), so just verify the gate
	// fires and the turn is NOT completed. The test expects a panic
	// from sendPrompt (nil server), which we catch via deferred recover.
	func() {
		defer func() {
			_ = recover() // expected: sendPrompt panics on nil server
		}()
		b.onSessionIdle("sess-test")
	}()

	nudgeMu.Lock()
	fired := nudgeFired
	nudgeMu.Unlock()
	if !fired {
		t.Error("PreAnswerNudgeFunc was not called")
	}
	// Turn should still be in flight (NOT completed).
	if !b.IsTurnInFlight() {
		t.Error("turn was completed despite PreAnswerNudgeFunc returning non-empty")
	}
	if *c.completed != nil {
		t.Error("OnTurnComplete fired despite nudge gate")
	}
}

func TestOnSessionIdle_FlushesSteerBuf(t *testing.T) {
	// Verifies OnSessionIdle flushes the steerBuf after completing
	// the current turn. Since we can't call sendPrompt without a real
	// server, this test asserts the steerBuf is drained (flushSteerBuf
	// calls sendPrompt which will panic on nil server — we catch it).
	b := newHandlerTestBackend(t)

	b.turnMu.Lock()
	b.steerBuf = []string{"queued steer 1", "queued steer 2"}
	b.turnMu.Unlock()

	// Catch the expected panic from sendPrompt (nil server).
	func() {
		defer func() { _ = recover() }()
		b.onSessionIdle("sess-test")
	}()

	b.turnMu.Lock()
	remaining := len(b.steerBuf)
	b.turnMu.Unlock()
	if remaining != 0 {
		t.Errorf("steerBuf not drained: %d items remain", remaining)
	}
}

// ---------------------------------------------------------------------------
// session.compacted
// ---------------------------------------------------------------------------

func TestOnSessionCompacted_FiresOnCompactionDone(t *testing.T) {
	// Verifies OnSessionCompacted fires onCompactionDone(0) and closes
	// compactDoneCh so WaitForCompaction unblocks.
	var compactionTokens int
	var compactionFired bool
	b := &Backend{
		sessionID:     "sess-test",
		compactDoneCh: make(chan struct{}, 1),
		outstanding:   delegator.NewOutstandingRegistry(),
	}
	b.mu.Lock()
	b.onCompactionDone = func(preTokens int) {
		compactionFired = true
		compactionTokens = preTokens
	}
	b.mu.Unlock()

	b.onSessionCompacted("sess-test")

	if !compactionFired {
		t.Error("onCompactionDone was not called")
	}
	if compactionTokens != 0 {
		t.Errorf("preTokens = %d, want 0 (unknown from session.compacted event)", compactionTokens)
	}

	// compactDoneCh should be closed.
	b.turnMu.Lock()
	ch := b.compactDoneCh
	b.turnMu.Unlock()
	select {
	case <-ch:
		// expected
	default:
		t.Error("compactDoneCh not closed after onSessionCompacted")
	}
}

// ---------------------------------------------------------------------------
// SessionEvents invariant: delivery works post-idle (no handler nil)
// ---------------------------------------------------------------------------

func TestTextDelivery_PostIdleFiresSessionEvents(t *testing.T) {
	// Verifies the SessionEvents/TurnEvents split (plan §7): after
	// onSessionIdle clears turnEvents (OnTurnComplete fired), text
	// arriving for the NEXT turn still fires SessionEvents.OnText.
	// This is the regression test for ccstream's TODO #747 — without
	// the split, post-idle text was silently dropped.
	b := newHandlerTestBackend(t)
	c := b.captures()

	// Complete the current turn.
	b.onSessionIdle("sess-test")

	// TurnEvents is now nil (OnTurnComplete was fired). Fire another
	// text event — SessionEvents.OnText must still fire because the
	// sessionEvents atomic pointer is independent of turnEvents.
	b.onMessagePartUpdated(Part{
		Type: PartText,
		ID:   "pt-postidle",
		Text: "text after idle",
		Time: &PartTime{Start: 1000, End: 2000},
	}, "")

	if len(*c.texts) == 0 {
		t.Error("OnText did not fire post-idle — SessionEvents invariant broken")
	}
}

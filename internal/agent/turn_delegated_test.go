package agent

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/compaction"
	"foci/internal/delegator"
	focilog "foci/internal/log"
	"foci/internal/nudge"
	"foci/internal/provider"
	"foci/internal/session"

	_ "modernc.org/sqlite"
)

// TestDelegatedTransport_NoOps verifies that no-op methods don't panic and
// return the expected zero values.
func TestDelegatedTransport_NoOps(t *testing.T) {
	a := &Agent{}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)

	// Phase 1 no-ops return zero values.
	if err := tr.RateLimitGate(ts); err != nil {
		t.Fatalf("RateLimitGate: %v", err)
	}
	unlock := tr.AcquireTurnLock(ts)
	unlock() // should not panic
	dec := tr.IncrementProcessing(ts)
	dec() // should not panic
	unreg := tr.RegisterTurn(ts)
	unreg() // should not panic

	// Phase 2 no-ops.
	if err := tr.LoadAndRepairSession(ts); err != nil {
		t.Fatalf("LoadAndRepairSession: %v", err)
	}
	tr.BuildSystemAndTools(ts) // no panic
	tr.InjectNudges(ts)        // no panic

	// Phase 4 no-ops.
	if err := tr.SaveSession(ts); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	tr.LogConversationSent(ts) // no-op: delegated path logs per-message in OnText
	tr.UpdateSessionMeta(ts)   // no panic (stub)
	tr.RunCompaction(ts)       // no panic (stub)
}

// TestDelegatedTransport_ResolveModelEffort verifies it reads agent-level model.
func TestDelegatedTransport_ResolveModelEffort(t *testing.T) {
	a := &Agent{Model: "anthropic/claude-opus-4-6"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	tr.ResolveModelEffort(ts)

	if ts.TurnModel != "anthropic/claude-opus-4-6" {
		t.Errorf("TurnModel = %q, want %q", ts.TurnModel, "anthropic/claude-opus-4-6")
	}
}

// TestDelegatedTransport_ComposePrompt verifies it produces a non-empty prompt
// and updates lastMessageTime.
func TestDelegatedTransport_ComposePrompt(t *testing.T) {
	a := &Agent{Model: "anthropic/claude-opus-4-6"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello world"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.TurnModel = a.Model
	ts.StartedAt = time.Now()

	if err := tr.ComposePrompt(ts); err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	if ts.Prompt == "" {
		t.Fatal("Prompt should not be empty")
	}
	// The prompt should contain the user text.
	if !strings.Contains(ts.Prompt, "hello world") {
		t.Errorf("Prompt should contain user text, got: %q", ts.Prompt)
	}
	// lastMessageTime should have been updated.
	if ts.SessionMeta.lastMessageTime.IsZero() {
		t.Error("lastMessageTime should be non-zero after ComposePrompt")
	}
}

// ---------------------------------------------------------------------------
// mockBackendDT is a lightweight backend mock for DelegatedTransport tests.
// Each method delegates to a function field when set, otherwise returns sane
// defaults. Keeps test code focused on DelegatedTransport behaviour.
// ---------------------------------------------------------------------------
type mockBackendDT struct {
	mu sync.Mutex

	sendToPaneFn  func(ctx context.Context, prompt string, handler *delegator.EventHandler) (*delegator.TurnResult, error)
	sendCommandFn func(ctx context.Context, command string) error
	waitForTurnFn func(ctx context.Context) error
	turnInFlight  bool
	sessionFile   string
	sessionEvents *delegator.SessionEvents // captured by AttachSessionEvents; Inject splices delivery callbacks back into handler
}

func (m *mockBackendDT) Start(_ context.Context, _ delegator.StartOptions) error             { return nil }
func (m *mockBackendDT) IsRunning() bool                                                   { return true }
func (m *mockBackendDT) Restart(_ context.Context) error                                   { return nil }
func (m *mockBackendDT) SetPermissionPromptFunc(_ delegator.PermissionPromptFunc)            {}
func (m *mockBackendDT) SetOnPromptsCleared(_ func())                                      {}
func (m *mockBackendDT) RegisterPromptCancelListener(_ string, _ func(string))             {}
func (m *mockBackendDT) SetOnSessionReady(_ func(string))                                  {}
func (m *mockBackendDT) SetTypingFunc(_ func(bool))                                        {}
func (m *mockBackendDT) AttachSessionEvents(events *delegator.SessionEvents) {
	m.mu.Lock()
	m.sessionEvents = events
	m.mu.Unlock()
}
func (m *mockBackendDT) SendKeystroke(_ context.Context, _ string) error                   { return nil }
func (m *mockBackendDT) SendSpecialKey(_ context.Context, _ string) error                  { return nil }
func (m *mockBackendDT) Interrupt(_ context.Context) error                                 { return nil }
func (m *mockBackendDT) SessionID() string                                                 { return "" }
func (m *mockBackendDT) WaitReady(_ context.Context) error                                 { return nil }
func (m *mockBackendDT) Close() error                                                      { return nil }

func (m *mockBackendDT) SessionFilePath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionFile
}

func (m *mockBackendDT) IsTurnInFlight() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turnInFlight
}

func (m *mockBackendDT) SendToPane(ctx context.Context, prompt string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
	if m.sendToPaneFn != nil {
		return m.sendToPaneFn(ctx, prompt, handler)
	}
	return &delegator.TurnResult{Text: "ok"}, nil
}

func (m *mockBackendDT) SendCommand(ctx context.Context, command string) error {
	if m.sendCommandFn != nil {
		return m.sendCommandFn(ctx, command)
	}
	return nil
}

// Inject mirrors production routing so tests written against the new
// canonical entry point exercise the same code paths as the legacy mocks.
// Production passes inj.Turn (TurnEvents) post-TODO #747; we synthesise
// an EventHandler so the SendToPane mock surface — which still uses
// EventHandler — keeps working without per-test churn. Delivery callbacks
// (OnText etc.) come from the SessionEvents previously installed via
// AttachSessionEvents — that's how production routes them, so test
// callbacks that fire handler.OnText exercise the new path.
func (m *mockBackendDT) Inject(ctx context.Context, inj delegator.Inject) error {
	m.mu.Lock()
	se := m.sessionEvents
	m.mu.Unlock()
	handler := inj.Handler
	if handler == nil {
		handler = &delegator.EventHandler{}
	}
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

func (m *mockBackendDT) WaitForTurn(ctx context.Context) error {
	if m.waitForTurnFn != nil {
		return m.waitForTurnFn(ctx)
	}
	return nil
}

// newMockDelegatedManager creates a DelegatedManager pre-loaded with a mock
// backend so tests can call RunInference without real CC infrastructure.
func newMockDelegatedManager(t *testing.T, be delegator.Delegator) *DelegatedManager {
	t.Helper()
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return be, nil },
	}
	// Pre-register the backend so Get() returns it immediately.
	_, err := mgr.Get(context.Background(), "test/s")
	if err != nil {
		t.Fatalf("pre-register backend: %v", err)
	}
	return mgr
}

// ---------------------------------------------------------------------------
// InjectNudges tests
// ---------------------------------------------------------------------------

// TestDelegatedTransport_InjectNudges_WithNudger verifies that nudge reminders
// from both CheckTurnInterval and CheckRegex are prepended to the prompt.
func TestDelegatedTransport_InjectNudges_WithNudger(t *testing.T) {
	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{Text: "interval-reminder", Trigger: nudge.Trigger{Type: "every_n_turns", N: 1}},
			{Text: "regex-reminder", Trigger: nudge.Trigger{Type: "regex", Pattern: "hello"}},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 2)

	a := &Agent{Model: "test-model", Nudger: sched}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello world"}, nil)
	ts.Prompt = "original prompt"

	tr.InjectNudges(ts)

	if !strings.Contains(ts.Prompt, "interval-reminder") {
		t.Errorf("expected interval-reminder in prompt, got: %q", ts.Prompt)
	}
	if !strings.Contains(ts.Prompt, "regex-reminder") {
		t.Errorf("expected regex-reminder in prompt, got: %q", ts.Prompt)
	}
	// The original prompt should still be at the end.
	if !strings.HasSuffix(ts.Prompt, "original prompt") {
		t.Errorf("original prompt should be at the end, got: %q", ts.Prompt)
	}
}

// TestDelegatedTransport_InjectNudges_NilNudger verifies that InjectNudges is
// a no-op when the agent has no Nudger.
func TestDelegatedTransport_InjectNudges_NilNudger(t *testing.T) {
	a := &Agent{Model: "test-model"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello"}, nil)
	ts.Prompt = "unchanged"

	tr.InjectNudges(ts)

	if ts.Prompt != "unchanged" {
		t.Errorf("prompt should be unchanged with nil nudger, got: %q", ts.Prompt)
	}
}

// TestDelegatedTransport_InjectNudges_EmptyTexts verifies that InjectNudges
// returns early when Texts is empty (no user message to evaluate regex against).
func TestDelegatedTransport_InjectNudges_EmptyTexts(t *testing.T) {
	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{Text: "should-not-appear", Trigger: nudge.Trigger{Type: "every_n_turns", N: 1}},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 2)
	a := &Agent{Model: "test-model", Nudger: sched}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", nil, nil)
	ts.Texts = []string{} // explicitly empty
	ts.Prompt = "original"

	tr.InjectNudges(ts)

	if ts.Prompt != "original" {
		t.Errorf("prompt should be unchanged with empty texts, got: %q", ts.Prompt)
	}
}

// TestDelegatedTransport_InjectNudges_NoMatchingNudges verifies that a nudger
// with rules that don't fire leaves the prompt unchanged.
func TestDelegatedTransport_InjectNudges_NoMatchingNudges(t *testing.T) {
	// Regex that won't match the user message, turn interval too high to fire.
	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{Text: "nope", Trigger: nudge.Trigger{Type: "regex", Pattern: "zzzzz_no_match"}},
			{Text: "also-nope", Trigger: nudge.Trigger{Type: "every_n_turns", N: 999}},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 2)
	a := &Agent{Model: "test-model", Nudger: sched}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello"}, nil)
	ts.Prompt = "original"

	tr.InjectNudges(ts)

	if ts.Prompt != "original" {
		t.Errorf("prompt should be unchanged when no nudges fire, got: %q", ts.Prompt)
	}
}

// ---------------------------------------------------------------------------
// RunInference tests
// ---------------------------------------------------------------------------

// TestDelegatedTransport_RunInference_Success verifies the happy path: Get
// returns a backend, SendToPane is called, and the OnTurnComplete handler
// populates FinalText/FinalUsage and closes CompletionChan.
func TestDelegatedTransport_RunInference_Success(t *testing.T) {
	be := &mockBackendDT{
		sessionFile: "/tmp/test-session.jsonl",
		sendToPaneFn: func(_ context.Context, prompt string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			if !strings.Contains(prompt, "hello") {
				t.Errorf("prompt should contain 'hello', got: %q", prompt)
			}
			// Simulate the watcher calling OnTurnComplete asynchronously.
			if handler != nil && handler.OnTurnComplete != nil {
				handler.OnTurnComplete(&delegator.TurnResult{
					Text:  "response text",
					Model: "claude-sonnet-4-20250514",
					Usage: &delegator.TurnUsage{
						InputTokens:  1000,
						OutputTokens: 200,
					},
				})
			}
			return nil, nil
		},
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello"}, nil)
	ts.Prompt = "hello"
	ts.StartedAt = time.Now()

	err := tr.RunInference(ts)
	if err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	// CompletionChan should be closed (non-blocking read).
	select {
	case <-ts.CompletionChan:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("CompletionChan was not closed")
	}

	if ts.Backend == nil {
		t.Error("Backend should be set")
	}
	if ts.FinalText != "response text" {
		t.Errorf("FinalText = %q, want %q", ts.FinalText, "response text")
	}
	if ts.FinalModel != "claude-sonnet-4-20250514" {
		t.Errorf("FinalModel = %q, want %q", ts.FinalModel, "claude-sonnet-4-20250514")
	}
	if ts.FinalUsage == nil {
		t.Fatal("FinalUsage should not be nil")
	}
	if ts.FinalUsage.InputTokens != 1000 {
		t.Errorf("FinalUsage.InputTokens = %d, want 1000", ts.FinalUsage.InputTokens)
	}
	if ts.FinalUsage.OutputTokens != 200 {
		t.Errorf("FinalUsage.OutputTokens = %d, want 200", ts.FinalUsage.OutputTokens)
	}
	if ts.sessionFilePath != "/tmp/test-session.jsonl" {
		t.Errorf("sessionFilePath = %q, want %q", ts.sessionFilePath, "/tmp/test-session.jsonl")
	}
}

// TestDelegatedTransport_RunInference_GetError verifies that an error from
// DelegatedManager.Get propagates back to the caller.
func TestDelegatedTransport_RunInference_GetError(t *testing.T) {
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) {
			return nil, errors.New("backend unavailable")
		},
	}
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"

	err := tr.RunInference(ts)
	if err == nil {
		t.Fatal("expected error from RunInference")
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("error should mention 'backend unavailable', got: %v", err)
	}
}

// TestDelegatedTransport_RunInference_TurnInFlight verifies the follow-up path:
// when IsTurnInFlight is true, SendCommand is called with the prompt and
// CompletionChan is closed immediately without creating a new turn pipeline.
// The backend's SendCommand owns rearm-cascade wiring so the queued response
// reaches the original handler — that wiring is asserted in ccstream_test.go,
// not here.
func TestDelegatedTransport_RunInference_TurnInFlight(t *testing.T) {
	var capturedCmd string
	be := &mockBackendDT{
		turnInFlight: true,
		sendCommandFn: func(_ context.Context, command string) error {
			capturedCmd = command
			return nil
		},
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"follow-up"}, nil)
	ts.Prompt = "follow-up text"

	err := tr.RunInference(ts)
	if err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	if capturedCmd != "follow-up text" {
		t.Errorf("SendCommand command = %q, want %q", capturedCmd, "follow-up text")
	}

	// CompletionChan should be closed.
	select {
	case <-ts.CompletionChan:
		// good
	case <-time.After(time.Second):
		t.Fatal("CompletionChan was not closed for in-flight follow-up")
	}
}

// TestDelegatedTransport_RunInference_TurnInFlightSendCommandError verifies
// that when IsTurnInFlight + SendCommand fails, the error propagates and
// CompletionChan is still closed (no leak).
func TestDelegatedTransport_RunInference_TurnInFlightSendCommandError(t *testing.T) {
	be := &mockBackendDT{
		turnInFlight: true,
		sendCommandFn: func(_ context.Context, _ string) error {
			return errors.New("send failed")
		},
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"follow-up"}, nil)
	ts.Prompt = "follow-up"

	err := tr.RunInference(ts)
	if err == nil || !strings.Contains(err.Error(), "send failed") {
		t.Fatalf("expected 'send failed' error, got: %v", err)
	}

	// CompletionChan should still be closed even on error.
	select {
	case <-ts.CompletionChan:
		// good
	case <-time.After(time.Second):
		t.Fatal("CompletionChan should be closed even on SendCommand error")
	}
}

// TestDelegatedTransport_RunInference_WaitForPermission verifies that
// RunInference blocks while a permission prompt is outstanding and proceeds
// once it is cleared.
func TestDelegatedTransport_RunInference_WaitForPermission(t *testing.T) {
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			if handler != nil && handler.OnTurnComplete != nil {
				handler.OnTurnComplete(&delegator.TurnResult{Text: "done"})
			}
			return nil, nil
		},
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}

	// Set permission pending before RunInference.
	mgr.SetPermissionPending("test/s", true)

	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()

	done := make(chan error, 1)
	go func() {
		done <- tr.RunInference(ts)
	}()

	// Give RunInference time to block on WaitForPermission.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("RunInference should be blocked waiting for permission")
	default:
		// expected -- still blocked
	}

	// Clear permission.
	mgr.SetPermissionPending("test/s", false)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunInference after permission cleared: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunInference did not complete after permission was cleared")
	}
}

// TestDelegatedTransport_RunInference_PostToolNudgeWired verifies that the
// PostToolNudgeFunc drives CheckAfterTools: on the N-th tool call the
// every_n_tools rule fires and the returned reminder is formatted with the
// nudge header. This is the delegated equivalent of the API transport's
// mid-loop CheckAfterTools call after each tool batch.
func TestDelegatedTransport_RunInference_PostToolNudgeWired(t *testing.T) {
	var capturedNudgeFunc func(string, string, bool) []string
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			capturedNudgeFunc = handler.PostToolNudgeFunc
			if handler.OnTurnComplete != nil {
				handler.OnTurnComplete(&delegator.TurnResult{Text: "ok"})
			}
			return nil, nil
		},
	}

	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{Text: "tool-batch-reminder", Trigger: nudge.Trigger{Type: "every_n_tools", N: 3}},
			{Text: "error-reminder", Trigger: nudge.Trigger{Type: "after_error"}},
		},
	}
	sched := nudge.NewScheduler(rs, 1, 2)
	sched.StartTurn("hi")

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr, Nudger: sched}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	if capturedNudgeFunc == nil {
		t.Fatal("PostToolNudgeFunc should be wired into the EventHandler")
	}

	// Two successful tools — no reminder yet (every_n_tools N=3, after_error needs isError).
	if got := capturedNudgeFunc("Read", "", false); len(got) != 0 {
		t.Errorf("PostToolNudgeFunc after tool 1 = %v, want empty", got)
	}
	if got := capturedNudgeFunc("Edit", "", false); len(got) != 0 {
		t.Errorf("PostToolNudgeFunc after tool 2 = %v, want empty", got)
	}
	// Third tool — every_n_tools should fire.
	got := capturedNudgeFunc("Bash", "", false)
	if len(got) == 0 {
		t.Fatal("PostToolNudgeFunc after tool 3 should return a reminder")
	}
	found := false
	for _, r := range got {
		if strings.Contains(r, "tool-batch-reminder") {
			found = true
		}
		if !strings.HasPrefix(r, nudgeHeader) {
			t.Errorf("reminder missing nudge header: %q", r)
		}
	}
	if !found {
		t.Errorf("expected tool-batch-reminder in %v", got)
	}
}

// TestDelegatedTransport_RunInference_PostToolNudgeNilNudger verifies the
// callback is safe when the agent has no Nudger — returns nil without panic
// so hook_response dispatch keeps working for agents that don't nudge.
func TestDelegatedTransport_RunInference_PostToolNudgeNilNudger(t *testing.T) {
	var capturedNudgeFunc func(string, string, bool) []string
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			capturedNudgeFunc = handler.PostToolNudgeFunc
			if handler.OnTurnComplete != nil {
				handler.OnTurnComplete(&delegator.TurnResult{Text: "ok"})
			}
			return nil, nil
		},
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	if capturedNudgeFunc == nil {
		t.Fatal("PostToolNudgeFunc should be wired even without Nudger")
	}
	if got := capturedNudgeFunc("Read", "", false); got != nil {
		t.Errorf("PostToolNudgeFunc with nil Nudger = %v, want nil", got)
	}
}

// TestDelegatedTransport_RunInference_PreAnswerGateFiresOnce verifies that
// PreAnswerNudgeFunc returns the verification prompt on the first call and
// "" thereafter — the closure-local firedPreAnswer flag must break the loop
// so ccstream doesn't re-dispatch indefinitely when the scheduler keeps
// reporting a pending pre_answer rule.
func TestDelegatedTransport_RunInference_PreAnswerGateFiresOnce(t *testing.T) {
	var capturedPreAnswerFunc func(*delegator.TurnResult) string
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			capturedPreAnswerFunc = handler.PreAnswerNudgeFunc
			if handler.OnTurnComplete != nil {
				handler.OnTurnComplete(&delegator.TurnResult{Text: "final"})
			}
			return nil, nil
		},
	}

	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{Text: "verify-your-answer", Trigger: nudge.Trigger{Type: "pre_answer"}},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 2)
	sched.StartTurn("hi")

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{
		Model:                  "test-model",
		DelegatedManager:       mgr,
		Nudger:                 sched,
		NudgePreAnswerGate:     true,
		NudgePreAnswerMinTools: 0,
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	if capturedPreAnswerFunc == nil {
		t.Fatal("PreAnswerNudgeFunc should be wired into the EventHandler")
	}

	// First call: round-1 result arrives with no tools — gate fires.
	firstRound := &delegator.TurnResult{
		Text:  "original answer",
		Usage: &delegator.TurnUsage{InputTokens: 100, OutputTokens: 50},
	}
	followUp := capturedPreAnswerFunc(firstRound)
	if followUp == "" {
		t.Fatal("pre-answer gate should fire on first call")
	}
	if !strings.Contains(followUp, "verify-your-answer") {
		t.Errorf("follow-up should contain reminder text, got: %q", followUp)
	}
	if !strings.Contains(followUp, NoResponseSentinel) {
		t.Errorf("follow-up should instruct model to emit %q on no-op, got: %q", NoResponseSentinel, followUp)
	}

	// Second call: gate should self-suppress.
	if again := capturedPreAnswerFunc(&delegator.TurnResult{Text: "revised"}); again != "" {
		t.Errorf("pre-answer gate should fire only once, got second follow-up: %q", again)
	}
}

// TestDelegatedTransport_RunInference_PreAnswerGateDisabled verifies that
// PreAnswerNudgeFunc returns "" when the agent hasn't enabled the gate, even
// if a pre_answer rule exists — the gate config flag is the master switch.
func TestDelegatedTransport_RunInference_PreAnswerGateDisabled(t *testing.T) {
	var capturedPreAnswerFunc func(*delegator.TurnResult) string
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			capturedPreAnswerFunc = handler.PreAnswerNudgeFunc
			if handler.OnTurnComplete != nil {
				handler.OnTurnComplete(&delegator.TurnResult{Text: "ok"})
			}
			return nil, nil
		},
	}

	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{Text: "verify", Trigger: nudge.Trigger{Type: "pre_answer"}},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 2)
	sched.StartTurn("hi")

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{
		Model:              "test-model",
		DelegatedManager:   mgr,
		Nudger:             sched,
		NudgePreAnswerGate: false, // explicitly disabled
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	if got := capturedPreAnswerFunc(&delegator.TurnResult{Text: "x"}); got != "" {
		t.Errorf("pre-answer gate should be a no-op when disabled, got: %q", got)
	}
}

// TestDelegatedTransport_RunInference_PreAnswerAccumulatesUsage verifies that
// when the gate runs a second round, round-1 usage captured inside the gate
// callback gets folded into the final ts.FinalUsage. Without this, the API
// log would under-report the true token spend of the user's turn since
// ccstream's beginTurn resets lastUsage between rounds.
func TestDelegatedTransport_RunInference_PreAnswerAccumulatesUsage(t *testing.T) {
	var capturedHandler *delegator.EventHandler
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			capturedHandler = handler
			return nil, nil
		},
	}

	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{Text: "verify", Trigger: nudge.Trigger{Type: "pre_answer"}},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 2)
	sched.StartTurn("hi")

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{
		Model:                  "test-model",
		DelegatedManager:       mgr,
		Nudger:                 sched,
		NudgePreAnswerGate:     true,
		NudgePreAnswerMinTools: 0,
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	// Simulate ccstream: round 1 → PreAnswerNudgeFunc (fires), round 2 → OnTurnComplete.
	round1 := &delegator.TurnResult{
		Text:  "original",
		Usage: &delegator.TurnUsage{InputTokens: 100, OutputTokens: 40, CacheReadInputTokens: 10},
	}
	if followUp := capturedHandler.PreAnswerNudgeFunc(round1); followUp == "" {
		t.Fatal("gate should have fired on round 1")
	}
	capturedHandler.OnTurnComplete(&delegator.TurnResult{
		Text:  "revised",
		Usage: &delegator.TurnUsage{InputTokens: 20, OutputTokens: 15},
	})

	if ts.FinalText != "revised" {
		t.Errorf("FinalText = %q, want %q", ts.FinalText, "revised")
	}
	if ts.FinalUsage == nil {
		t.Fatal("FinalUsage should not be nil")
	}
	// 100 + 20 = 120 input, 40 + 15 = 55 output, 10 + 0 cache read.
	if ts.FinalUsage.InputTokens != 120 {
		t.Errorf("FinalUsage.InputTokens = %d, want 120 (accumulated)", ts.FinalUsage.InputTokens)
	}
	if ts.FinalUsage.OutputTokens != 55 {
		t.Errorf("FinalUsage.OutputTokens = %d, want 55 (accumulated)", ts.FinalUsage.OutputTokens)
	}
	if ts.FinalUsage.CacheReadInputTokens != 10 {
		t.Errorf("FinalUsage.CacheReadInputTokens = %d, want 10 (accumulated)", ts.FinalUsage.CacheReadInputTokens)
	}
}

// TestDelegatedTransport_RunInference_PreAnswerSentinelRestoresOriginal
// verifies that when the second round echoes NoResponseSentinel ("my
// original answer stands"), FinalText is replaced with round-1's text so
// the platform delivers the answer the agent originally committed to rather
// than a raw sentinel literal.
func TestDelegatedTransport_RunInference_PreAnswerSentinelRestoresOriginal(t *testing.T) {
	var capturedHandler *delegator.EventHandler
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			capturedHandler = handler
			return nil, nil
		},
	}

	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{Text: "verify", Trigger: nudge.Trigger{Type: "pre_answer"}},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 2)
	sched.StartTurn("hi")

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{
		Model:                  "test-model",
		DelegatedManager:       mgr,
		Nudger:                 sched,
		NudgePreAnswerGate:     true,
		NudgePreAnswerMinTools: 0,
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	round1 := &delegator.TurnResult{
		Text:  "original answer that stands",
		Usage: &delegator.TurnUsage{InputTokens: 10, OutputTokens: 5},
	}
	if capturedHandler.PreAnswerNudgeFunc(round1) == "" {
		t.Fatal("gate should fire")
	}
	capturedHandler.OnTurnComplete(&delegator.TurnResult{
		Text:  NoResponseSentinel,
		Usage: &delegator.TurnUsage{InputTokens: 5, OutputTokens: 1},
	})

	if ts.FinalText != "original answer that stands" {
		t.Errorf("FinalText = %q, want original answer (sentinel should restore round-1)", ts.FinalText)
	}
}

// TestDelegatedTransport_RunInference_NilTurnResult verifies that a nil
// TurnResult from the watcher doesn't panic (edge case: watcher error).
func TestDelegatedTransport_RunInference_NilTurnResult(t *testing.T) {
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			if handler != nil && handler.OnTurnComplete != nil {
				handler.OnTurnComplete(nil)
			}
			return nil, nil
		},
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	select {
	case <-ts.CompletionChan:
	case <-time.After(time.Second):
		t.Fatal("CompletionChan should be closed even with nil TurnResult")
	}

	if ts.FinalText != "" {
		t.Errorf("FinalText should be empty for nil result, got: %q", ts.FinalText)
	}
	if ts.FinalUsage != nil {
		t.Error("FinalUsage should be nil for nil result")
	}
}

// ---------------------------------------------------------------------------
// OnText conversation logging
// ---------------------------------------------------------------------------

// TestDelegatedTransport_OnText_LogsEachMessage verifies that the OnText
// callback in RunInference logs each intermediate text to the conversation DB
// individually, rather than accumulating a single concatenated row at turn end.
// This is the fix for conversation.db missing per-message rows (Issue 2) and
// concatenating separate messages into one row (Issue 3).
func TestDelegatedTransport_OnText_LogsEachMessage(t *testing.T) {
	// Set up a temp conversation DB so log.Conversation() actually writes.
	dir := t.TempDir()
	agentID := "test"
	err := focilog.InitPerAgentConversation([]string{agentID}, func(id string) string {
		return dir + "/" + id + ".db"
	})
	if err != nil {
		t.Fatalf("InitPerAgentConversation: %v", err)
	}
	defer focilog.CloseConversation()

	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *delegator.EventHandler) (*delegator.TurnResult, error) {
			// Simulate CC producing three intermediate text blocks
			// followed by turn completion.
			if handler != nil {
				if handler.OnText != nil {
					handler.OnText("message one")
					handler.OnText("message two")
					handler.OnText("message three")
				}
				if handler.OnTurnComplete != nil {
					handler.OnTurnComplete(&delegator.TurnResult{
						Text:  "message onemessage twomessage three",
						Model: "test-model",
					})
				}
			}
			return nil, nil
		},
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}

	// The session key must start with the agent ID so resolveConvLog routes
	// to the right DB.
	sk := agentID + "/chat/12345"
	// Wrap the test's BufferSink with loggingSink so intermediate TextBlock
	// events fire conversation-DB logging — production wires this in
	// Agent.RunTurn (per-turn) and lateDeliverySink (fallback). Tests that
	// call RunInference directly need to wire it themselves.
	meta := &TurnMetadata{UserID: "u1", Username: "dick"}
	wrappedSink := newLoggingSink(turnevent.NewBufferSink(), a, 42, meta, sk)
	ctx := turnevent.WithSink(context.Background(), wrappedSink)
	ts := NewTurnState(ctx, sk, []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()
	ts.Meta = meta
	ts.ConvChatID = 42

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	<-ts.CompletionChan

	// Now run LogConversationSent (the no-op override) — should NOT add
	// another row with the concatenated text.
	tr.LogConversationSent(ts)

	// Query the DB for sent rows.
	dbPath := dir + "/" + agentID + ".db"
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open DB: %v", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT text FROM messages WHERE direction='sent' ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var texts []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatalf("scan: %v", err)
		}
		texts = append(texts, text)
	}

	if len(texts) != 3 {
		t.Fatalf("got %d sent rows, want 3 individual messages; texts=%v", len(texts), texts)
	}
	if texts[0] != "message one" || texts[1] != "message two" || texts[2] != "message three" {
		t.Errorf("sent texts = %v, want [message one, message two, message three]", texts)
	}
}

// ---------------------------------------------------------------------------
// UpdateSessionMeta tests
// ---------------------------------------------------------------------------

// TestDelegatedTransport_UpdateSessionMeta_Tracking verifies that token counts
// and model from the turn result are persisted into sessionMeta.
func TestDelegatedTransport_UpdateSessionMeta_Tracking(t *testing.T) {
	a := &Agent{Model: "test-model"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	started := time.Now()
	ts.StartedAt = started
	ts.FinalModel = "claude-opus-4-20250514"
	ts.FinalUsage = &provider.Usage{
		InputTokens:              5000,
		OutputTokens:             1500,
		CacheReadInputTokens:     3000,
		CacheCreationInputTokens: 200,
	}

	tr.UpdateSessionMeta(ts)

	sm := ts.SessionMeta
	if sm.lastMessageTime != started {
		t.Errorf("lastMessageTime = %v, want %v", sm.lastMessageTime, started)
	}
	if sm.prevInput != 5000 {
		t.Errorf("prevInput = %d, want 5000", sm.prevInput)
	}
	if sm.prevOutput != 1500 {
		t.Errorf("prevOutput = %d, want 1500", sm.prevOutput)
	}
	if sm.prevCacheRead != 3000 {
		t.Errorf("prevCacheRead = %d, want 3000", sm.prevCacheRead)
	}
	if sm.prevCacheWrite != 200 {
		t.Errorf("prevCacheWrite = %d, want 200", sm.prevCacheWrite)
	}
	if sm.model != "claude-opus-4-20250514" {
		t.Errorf("model = %q, want %q", sm.model, "claude-opus-4-20250514")
	}
}

// TestDelegatedTransport_UpdateSessionMeta_NilUsage verifies that
// UpdateSessionMeta is a safe no-op when FinalUsage is nil.
func TestDelegatedTransport_UpdateSessionMeta_NilUsage(t *testing.T) {
	a := &Agent{Model: "test-model"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.FinalUsage = nil

	// Should not panic.
	tr.UpdateSessionMeta(ts)

	if ts.SessionMeta.prevInput != 0 {
		t.Error("prevInput should be unchanged with nil usage")
	}
}

// TestDelegatedTransport_UpdateSessionMeta_NilSessionMeta verifies that
// UpdateSessionMeta is a safe no-op when SessionMeta is nil.
func TestDelegatedTransport_UpdateSessionMeta_NilSessionMeta(t *testing.T) {
	a := &Agent{Model: "test-model"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = nil
	ts.FinalUsage = &provider.Usage{InputTokens: 100}

	// Should not panic.
	tr.UpdateSessionMeta(ts)
}

// TestDelegatedTransport_UpdateSessionMeta_EmptyModel verifies that the model
// field in sessionMeta is left unchanged when FinalModel is empty.
func TestDelegatedTransport_UpdateSessionMeta_EmptyModel(t *testing.T) {
	a := &Agent{Model: "test-model"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.SessionMeta.model = "existing-model"
	ts.FinalModel = "" // watcher didn't report a model
	ts.FinalUsage = &provider.Usage{InputTokens: 100}

	tr.UpdateSessionMeta(ts)

	if ts.SessionMeta.model != "existing-model" {
		t.Errorf("model should be unchanged, got: %q", ts.SessionMeta.model)
	}
}

// ---------------------------------------------------------------------------
// LogUsage tests
// ---------------------------------------------------------------------------

// TestDelegatedTransport_LogUsage_CostCalculation verifies that LogUsage sets
// FinalCost using the model and usage data, and uses FinalModel when available.
func TestDelegatedTransport_LogUsage_CostCalculation(t *testing.T) {
	a := &Agent{Model: "test-model"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.StartedAt = time.Now()
	ts.TurnModel = "claude-sonnet-4-20250514"
	ts.FinalModel = "claude-opus-4-20250514"
	ts.FinalUsage = &provider.Usage{
		InputTokens:  1000,
		OutputTokens: 500,
	}
	ts.sessionFilePath = "/tmp/session.jsonl"

	tr.LogUsage(ts)

	if ts.FinalCost <= 0 {
		t.Errorf("FinalCost should be positive, got: %f", ts.FinalCost)
	}
}

// TestDelegatedTransport_LogUsage_FallbackToTurnModel verifies that when
// FinalModel is empty, LogUsage falls back to TurnModel.
func TestDelegatedTransport_LogUsage_FallbackToTurnModel(t *testing.T) {
	a := &Agent{Model: "test-model"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.StartedAt = time.Now()
	ts.TurnModel = "claude-sonnet-4-20250514"
	ts.FinalModel = "" // watcher didn't provide a model
	ts.FinalUsage = &provider.Usage{
		InputTokens:  100,
		OutputTokens: 50,
	}
	ts.sessionFilePath = "/tmp/session.jsonl"

	tr.LogUsage(ts)

	// Cost should still be calculated using TurnModel.
	if ts.FinalCost <= 0 {
		t.Errorf("FinalCost should be positive using TurnModel fallback, got: %f", ts.FinalCost)
	}
}

// TestDelegatedTransport_LogUsage_NilUsage verifies that LogUsage is a safe
// no-op when FinalUsage is nil.
func TestDelegatedTransport_LogUsage_NilUsage(t *testing.T) {
	a := &Agent{Model: "test-model"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.FinalUsage = nil

	// Should not panic.
	tr.LogUsage(ts)

	if ts.FinalCost != 0 {
		t.Errorf("FinalCost should be zero with nil usage, got: %f", ts.FinalCost)
	}
}

// ---------------------------------------------------------------------------
// RunCompaction tests
// ---------------------------------------------------------------------------

// TestDelegatedTransport_RunCompaction_AboveThreshold verifies that when total
// tokens exceed the compaction threshold, /compact is sent to the backend.
func TestDelegatedTransport_RunCompaction_AboveThreshold(t *testing.T) {
	var capturedCmd string
	be := &mockBackendDT{
		sendCommandFn: func(_ context.Context, cmd string) error {
			capturedCmd = cmd
			return nil
		},
	}

	store := session.NewStore(t.TempDir())
	comp := compaction.NewCompactor(store, 0.8)

	var memoryFired, startFired, notifyFired bool
	a := &Agent{
		Model:            "test-model",
		Compactor:        comp,
		DelegatedManager: newMockDelegatedManager(t, be),
	}
	a.CompactionMemoryFunc.Add(func(_ string) { memoryFired = true })
	a.CompactionStartFunc.Add(func(_, _ string) { startFired = true })
	a.CompactionNotifyFunc.Add(func(_, _ string) { notifyFired = true })

	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.Backend = be
	ts.StartedAt = time.Now()
	// 200k context window for unknown model, 0.8 threshold = 160k.
	// Set tokens above threshold.
	ts.FinalUsage = &provider.Usage{
		InputTokens:              100000,
		CacheReadInputTokens:     70000,
		CacheCreationInputTokens: 0,
	}

	tr.RunCompaction(ts)

	if capturedCmd == "" {
		t.Fatal("expected /compact command to be sent")
	}
	if !strings.HasPrefix(capturedCmd, "/compact ") {
		t.Errorf("command should start with '/compact ', got: %q", capturedCmd)
	}
	if !memoryFired {
		t.Error("CompactionMemoryFunc should have been called")
	}
	if !startFired {
		t.Error("CompactionStartFunc should have been called")
	}
	if !notifyFired {
		t.Error("CompactionNotifyFunc should have been called")
	}
}

// TestDelegatedTransport_RunCompaction_BelowThreshold verifies that when tokens
// are below the compaction threshold, no /compact command is sent.
func TestDelegatedTransport_RunCompaction_BelowThreshold(t *testing.T) {
	cmdSent := false
	be := &mockBackendDT{
		sendCommandFn: func(_ context.Context, _ string) error {
			cmdSent = true
			return nil
		},
	}

	store := session.NewStore(t.TempDir())
	comp := compaction.NewCompactor(store, 0.8)
	a := &Agent{
		Model:            "test-model",
		Compactor:        comp,
		DelegatedManager: newMockDelegatedManager(t, be),
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.Backend = be
	ts.StartedAt = time.Now()
	// Below threshold: 200k * 0.8 = 160k, so 50k is well below.
	ts.FinalUsage = &provider.Usage{
		InputTokens: 50000,
	}

	tr.RunCompaction(ts)

	if cmdSent {
		t.Error("should not send /compact when below threshold")
	}
}

// TestDelegatedTransport_RunCompaction_NilCompactor verifies that RunCompaction
// is a safe no-op when the Compactor is nil.
func TestDelegatedTransport_RunCompaction_NilCompactor(t *testing.T) {
	a := &Agent{Model: "test-model"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.FinalUsage = &provider.Usage{InputTokens: 100000}

	// Should not panic.
	tr.RunCompaction(ts)
}

// TestDelegatedTransport_RunCompaction_NilUsage verifies that RunCompaction
// is a safe no-op when FinalUsage is nil.
func TestDelegatedTransport_RunCompaction_NilUsage(t *testing.T) {
	store := session.NewStore(t.TempDir())
	comp := compaction.NewCompactor(store, 0.8)
	a := &Agent{Model: "test-model", Compactor: comp}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.FinalUsage = nil

	// Should not panic.
	tr.RunCompaction(ts)
}

// TestDelegatedTransport_RunCompaction_NoCompactSession verifies that sessions
// marked as no_compact skip compaction even above the threshold.
func TestDelegatedTransport_RunCompaction_NoCompactSession(t *testing.T) {
	cmdSent := false
	be := &mockBackendDT{
		sendCommandFn: func(_ context.Context, _ string) error {
			cmdSent = true
			return nil
		},
	}

	store := session.NewStore(t.TempDir())
	comp := compaction.NewCompactor(store, 0.8)
	a := &Agent{
		Model:            "test-model",
		Compactor:        comp,
		DelegatedManager: newMockDelegatedManager(t, be),
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.Backend = be
	ts.StartedAt = time.Now()
	// Above threshold.
	ts.FinalUsage = &provider.Usage{
		InputTokens:          100000,
		CacheReadInputTokens: 70000,
	}

	// Mark session as no_compact.
	a.SetSessionNoCompact(ts.SessionKey, true)

	tr.RunCompaction(ts)

	if cmdSent {
		t.Error("should not send /compact for no_compact sessions")
	}
}

// TestDelegatedTransport_RunCompaction_NudgeReloadAndMetaReset verifies that
// after successful compaction, NudgeReloadFunc is called and sessionMeta
// fields are cleared.
func TestDelegatedTransport_RunCompaction_NudgeReloadAndMetaReset(t *testing.T) {
	be := &mockBackendDT{
		sendCommandFn: func(_ context.Context, _ string) error { return nil },
	}

	store := session.NewStore(t.TempDir())
	comp := compaction.NewCompactor(store, 0.8)
	nudgeReloaded := false
	a := &Agent{
		Model:            "test-model",
		Compactor:        comp,
		DelegatedManager: newMockDelegatedManager(t, be),
		NudgeReloadFunc:  func() { nudgeReloaded = true },
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.SessionMeta.prevCacheRead = 5000
	ts.SessionMeta.systemBlocks = []provider.SystemBlock{{Text: "old"}}
	ts.Backend = be
	ts.StartedAt = time.Now()
	ts.FinalUsage = &provider.Usage{
		InputTokens:          100000,
		CacheReadInputTokens: 70000,
	}

	tr.RunCompaction(ts)

	if !nudgeReloaded {
		t.Error("NudgeReloadFunc should have been called after compaction")
	}
	if ts.SessionMeta.systemBlocks != nil {
		t.Error("systemBlocks should be cleared after compaction")
	}
	if ts.SessionMeta.prevCacheRead != 0 {
		t.Errorf("prevCacheRead should be cleared after compaction, got: %d", ts.SessionMeta.prevCacheRead)
	}
}

// TestDelegatedTransport_RunCompaction_SendCommandError verifies that when
// SendCommand fails during compaction, no panic occurs and notify callbacks
// are not called.
func TestDelegatedTransport_RunCompaction_SendCommandError(t *testing.T) {
	be := &mockBackendDT{
		sendCommandFn: func(_ context.Context, _ string) error {
			return errors.New("tmux send error")
		},
	}

	store := session.NewStore(t.TempDir())
	comp := compaction.NewCompactor(store, 0.8)
	notifyCalled := false
	a := &Agent{
		Model:            "test-model",
		Compactor:        comp,
		DelegatedManager: newMockDelegatedManager(t, be),
	}
	a.CompactionNotifyFunc.Add(func(_, _ string) { notifyCalled = true })
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.Backend = be
	ts.StartedAt = time.Now()
	ts.FinalUsage = &provider.Usage{
		InputTokens:          100000,
		CacheReadInputTokens: 70000,
	}

	tr.RunCompaction(ts)

	if notifyCalled {
		t.Error("CompactionNotifyFunc should not be called when SendCommand fails")
	}
}

// TestDelegatedTransport_RunCompaction_WaitForTurnError verifies that when
// WaitForTurn times out, the notify callback is not called.
func TestDelegatedTransport_RunCompaction_WaitForTurnError(t *testing.T) {
	be := &mockBackendDT{
		sendCommandFn: func(_ context.Context, _ string) error { return nil },
		waitForTurnFn: func(_ context.Context) error {
			return errors.New("timeout waiting for compaction turn")
		},
	}

	store := session.NewStore(t.TempDir())
	comp := compaction.NewCompactor(store, 0.8)
	notifyCalled := false
	a := &Agent{
		Model:            "test-model",
		Compactor:        comp,
		DelegatedManager: newMockDelegatedManager(t, be),
	}
	a.CompactionNotifyFunc.Add(func(_, _ string) { notifyCalled = true })
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.Backend = be
	ts.StartedAt = time.Now()
	ts.FinalUsage = &provider.Usage{
		InputTokens:          100000,
		CacheReadInputTokens: 70000,
	}

	tr.RunCompaction(ts)

	if notifyCalled {
		t.Error("CompactionNotifyFunc should not be called when WaitForTurn fails")
	}
}

// TestDelegatedTransport_RunCompaction_EmptySummaryPrompt verifies that
// compaction is skipped when the summary prompt resolves to empty (path="none").
func TestDelegatedTransport_RunCompaction_EmptySummaryPrompt(t *testing.T) {
	cmdSent := false
	be := &mockBackendDT{
		sendCommandFn: func(_ context.Context, _ string) error {
			cmdSent = true
			return nil
		},
	}

	store := session.NewStore(t.TempDir())
	comp := compaction.NewCompactor(store, 0.8)
	a := &Agent{
		Model:                       "test-model",
		Compactor:                   comp,
		DelegatedManager:            newMockDelegatedManager(t, be),
		CompactionSummaryPromptPath: "none", // resolves to empty string
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.Backend = be
	ts.StartedAt = time.Now()
	ts.FinalUsage = &provider.Usage{
		InputTokens:          100000,
		CacheReadInputTokens: 70000,
	}

	tr.RunCompaction(ts)

	if cmdSent {
		t.Error("should not send /compact when summary prompt resolves to empty")
	}
}

// TestDelegatedTransport_ResolveModelEffort_SessionModel verifies that when a
// session-level model is set (via sessionMeta), it takes precedence over the
// agent-level model.
func TestDelegatedTransport_ResolveModelEffort_SessionModel(t *testing.T) {
	a := &Agent{Model: "agent-default"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	// Set session model via the meta.
	sm := a.getSessionMeta("test/s")
	sm.model = "session-override"

	tr.ResolveModelEffort(ts)

	if ts.TurnModel != "session-override" {
		t.Errorf("TurnModel = %q, want %q", ts.TurnModel, "session-override")
	}
}

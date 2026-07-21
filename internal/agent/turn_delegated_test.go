package agent

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/compaction"
	"foci/internal/convo"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/modelinfo"
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
	unreg := tr.RegisterTurn(ts)
	unreg() // should not panic (registers + unregisters a TurnDetail)

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
	// lastMessageTime is now written centrally by OrchestrateFullTurn (after
	// ComposePrompt), not by the transport — covered at the orchestrator level.
}

// ---------------------------------------------------------------------------
// mockBackendDT is a lightweight backend mock for DelegatedTransport tests.
// Each method delegates to a function field when set, otherwise returns sane
// defaults. Keeps test code focused on DelegatedTransport behaviour.
// ---------------------------------------------------------------------------
// mockHandler bundles per-turn callbacks for the delegated-backend mocks — the
// shape the production EventHandler used to have. The mock Inject synthesises
// one from the installed SessionEvents (delivery) + Inject.Turn (bookkeeping),
// so test closures that fire handler.OnText/OnTurnComplete exercise the same
// routing production uses (SendToPane is a mock-only test seam).
type mockHandler struct {
	OnText          func(text string)
	OnTextDelta     func(delta string)
	OnThinkingDelta func(delta string)
	OnToolStart     func(id, name, input string)
	OnToolEnd       func(id, name, output string, isError bool)
	OnTurnComplete  func(result *delegator.TurnResult)

	PostToolNudgeFunc  func(toolName, toolInput string, isError bool) []string
	PreAnswerNudgeFunc func(result *delegator.TurnResult) string
}

type mockBackendDT struct {
	mu sync.Mutex

	sendToPaneFn  func(ctx context.Context, prompt string, handler *mockHandler) (*delegator.TurnResult, error)
	sendCommandFn func(ctx context.Context, command string) error
	waitForTurnFn func(ctx context.Context) error
	turnInFlight  bool
	sessionFile   string
	sessionEvents *delegator.SessionEvents // captured by AttachSessionEvents; Inject splices delivery callbacks back into handler
	awaiting      bool                     // AwaitingAutonomousRun() return, guarded by mu (#1068 Phase 2 gate tests)
}

// setAwaiting toggles the AwaitingAutonomousRun() result under mu so the inbox
// worker (another goroutine) sees the change safely.
func (m *mockBackendDT) setAwaiting(v bool) {
	m.mu.Lock()
	m.awaiting = v
	m.mu.Unlock()
}

// AwaitingAutonomousRun satisfies delegator.AutonomousRunAwaiter for the inject-
// gate tests.
func (m *mockBackendDT) AwaitingAutonomousRun() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.awaiting
}

func (m *mockBackendDT) Start(_ context.Context, _ delegator.StartOptions) error  { return nil }
func (m *mockBackendDT) IsRunning() bool                                          { return true }
func (m *mockBackendDT) SetPermissionPromptFunc(_ delegator.PermissionPromptFunc) {}
func (m *mockBackendDT) SetOnPromptsCleared(_ func())                             {}
func (m *mockBackendDT) RegisterPromptCancelListener(_ string, _ func(string))    {}
func (m *mockBackendDT) SetOnSessionReady(_ func(string))                         {}
func (m *mockBackendDT) SetTypingFunc(_ func(bool))                               {}
func (m *mockBackendDT) AttachSessionEvents(events *delegator.SessionEvents) {
	m.mu.Lock()
	m.sessionEvents = events
	m.mu.Unlock()
}
func (m *mockBackendDT) SendKeystroke(_ context.Context, _ string) error  { return nil }
func (m *mockBackendDT) SendSpecialKey(_ context.Context, _ string) error { return nil }
func (m *mockBackendDT) Interrupt(_ context.Context) error                { return nil }
func (m *mockBackendDT) SessionID() string                                { return "" }
func (m *mockBackendDT) WaitReady(_ context.Context) error                { return nil }
func (m *mockBackendDT) CheckReady(_ context.Context) (bool, error)       { return true, nil }
func (m *mockBackendDT) StatusDetail() string                              { return "" }
func (m *mockBackendDT) Close() error                                     { return nil }

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

func (m *mockBackendDT) SendToPane(ctx context.Context, prompt string, handler *mockHandler) (*delegator.TurnResult, error) {
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

// Inject mirrors production routing so tests exercise the same code paths.
// Production passes inj.Turn (TurnEvents) for bookkeeping and installs delivery
// via AttachSessionEvents; we recombine the two into a mockHandler for the
// SendToPane test seam so test closures that fire handler.OnText/OnTurnComplete
// keep working without per-test churn — delivery callbacks come from the
// SessionEvents installed via AttachSessionEvents, exactly as production routes them.
func (m *mockBackendDT) ImmediateInject(ctx context.Context, inj delegator.Inject) error {
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
	case delegator.SourceSystem:
		// Mirrors production: system input never folds into an in-flight
		// turn — reject so the caller waits and retries.
		if m.IsTurnInFlight() {
			return delegator.ErrTurnInFlight
		}
		_, err := m.SendToPane(ctx, inj.Text, handler)
		return err
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
	// Bundled nudges drop the NO_RESPONSE footer — a reply to the user is
	// always required on this path.
	if strings.Contains(ts.Prompt, NoResponseSentinel) {
		t.Errorf("bundled nudges must not carry the NO_RESPONSE footer; got: %q", ts.Prompt)
	}
	// A single closing delimiter must separate the nudge region from the
	// user's prompt — exactly once, even with multiple nudges fired.
	if got := strings.Count(ts.Prompt, a.nudgeUserBoundary()); got != 1 {
		t.Errorf("expected exactly 1 end-marker between nudges and prompt, got %d; prompt: %q", got, ts.Prompt)
	}
	// TODO 1434: the region must open the <system-reminder> wrapper exactly
	// once too — not once per bundled nudge — so two triggers firing on the
	// same turn (interval + regex, as here) don't nest the tag.
	if got := strings.Count(ts.Prompt, a.nudgePreamble()); got != 1 {
		t.Errorf("expected exactly 1 nudge-preamble (system-reminder open) even with 2 nudges fired, got %d; prompt: %q", got, ts.Prompt)
	}
	if !strings.Contains(ts.Prompt, a.nudgeUserBoundary()+"\n\noriginal prompt") {
		t.Errorf("end-marker should sit directly before the original prompt, got: %q", ts.Prompt)
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

// TestDelegatedTransport_InjectNudges_ReflectionTriggerSuppressed verifies that
// a non-user trigger (reflection/keepalive/etc.) suppresses turn-interval and
// regex nudges entirely — and, because the early return precedes StartTurn, the
// every_n_turns lifetime counter is NOT advanced by system turns. (#815)
func TestDelegatedTransport_InjectNudges_ReflectionTriggerSuppressed(t *testing.T) {
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
	ts.Trigger = "reflection" // system-internal turn — no user-facing answer
	ts.Prompt = "original prompt"

	tr.InjectNudges(ts)

	if ts.Prompt != "original prompt" {
		t.Errorf("reflection turn should inject no nudges, got: %q", ts.Prompt)
	}

	// A subsequent *user* turn (Trigger="") must still fire — proves the gate
	// keys off the trigger, not a permanent disable, and that the reflection
	// turn did not consume the every_n_turns interval.
	ts2 := NewTurnState(context.Background(), "test/s", []string{"hello world"}, nil)
	ts2.Prompt = "user prompt"
	tr.InjectNudges(ts2)
	if !strings.Contains(ts2.Prompt, "interval-reminder") {
		t.Errorf("user turn after reflection should fire interval nudge, got: %q", ts2.Prompt)
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
		sendToPaneFn: func(_ context.Context, prompt string, handler *mockHandler) (*delegator.TurnResult, error) {
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

// TestDelegatedTransport_RunInference_DoubleComplete proves the per-turn
// sync.Once guard makes OnTurnComplete idempotent (P1-8). Both the normal
// result path (OnResult) and the process-exit finalize path can fire
// OnTurnComplete for the same turn; without the guard the second
// close(CompletionChan) panics and crashes the gateway. Asserts: two sequential
// completions do not panic, the channel is closed exactly once, and the first
// completion's result wins.
func TestDelegatedTransport_RunInference_DoubleComplete(t *testing.T) {
	be := &mockBackendDT{
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
			// Simulate the OnResult/finalizeExit race: both fire completion.
			handler.OnTurnComplete(&delegator.TurnResult{Text: "real answer"})
			handler.OnTurnComplete(&delegator.TurnResult{Text: "process exited"})
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
	case <-time.After(2 * time.Second):
		t.Fatal("CompletionChan was not closed")
	}
	if ts.FinalText != "real answer" {
		t.Errorf("FinalText = %q, want %q (first completion wins)", ts.FinalText, "real answer")
	}
}

// TestDelegatedTransport_RunInference_ConcurrentComplete is the -race variant of
// the double-complete guard: many goroutines fire OnTurnComplete at once and the
// sync.Once must still close CompletionChan exactly once with no panic or data
// race. (make test does not run -race; run with `go test -race`.)
func TestDelegatedTransport_RunInference_ConcurrentComplete(t *testing.T) {
	var wg sync.WaitGroup
	be := &mockBackendDT{
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					handler.OnTurnComplete(&delegator.TurnResult{Text: "x"})
				}()
			}
			wg.Wait()
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
	case <-time.After(2 * time.Second):
		t.Fatal("CompletionChan was not closed")
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

// interactiveTestCtx returns a context whose trigger is a registered platform
// trigger, so RunInference classifies the turn as real-time user input (the
// only class allowed to fold into an in-flight turn).
func interactiveTestCtx() context.Context {
	RegisterPlatformTrigger("testplat")
	return WithTrigger(context.Background(), "testplat")
}

// TestDelegatedTransport_RunInference_TurnInFlight verifies the interactive
// follow-up path: when IsTurnInFlight is true and the turn carries a platform
// (user-input) trigger, SendCommand is called with the prompt and
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
	ts := NewTurnState(interactiveTestCtx(), "test/s", []string{"follow-up"}, nil)
	ts.Trigger = "testplat"
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
// that when IsTurnInFlight + SendCommand fails (interactive fold path), the
// error propagates and CompletionChan is still closed (no leak).
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
	ts := NewTurnState(interactiveTestCtx(), "test/s", []string{"follow-up"}, nil)
	ts.Trigger = "testplat"
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

// ---------------------------------------------------------------------------
// Voice mode tests (#1445)
// ---------------------------------------------------------------------------

// voiceModeBackendDT wraps mockBackendDT with delegator.VoiceModer, recording
// Enter/Exit call counts (and, via enterSeenBySendToPane, whether Enter had
// already run by the time dispatch reached the backend) so RunInference's
// voice-mode wiring can be asserted without a real CC/Codex process.
type voiceModeBackendDT struct {
	mockBackendDT
	enterCalls int
	exitCalls  int
	enterErr   error
}

func (v *voiceModeBackendDT) EnterVoiceMode(_ context.Context) error {
	v.mu.Lock()
	v.enterCalls++
	v.mu.Unlock()
	return v.enterErr
}

func (v *voiceModeBackendDT) ExitVoiceMode(_ context.Context) error {
	v.mu.Lock()
	v.exitCalls++
	v.mu.Unlock()
	return nil
}

func (v *voiceModeBackendDT) counts() (enter, exit int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.enterCalls, v.exitCalls
}

// TestDelegatedTransport_RunInference_VoiceMode_EntersBeforeDispatchExitsAfterComplete
// verifies the full voice-mode wiring: for a trigger=="voice" turn on a
// backend implementing VoiceModer, EnterVoiceMode runs before the prompt
// reaches the backend, and ExitVoiceMode runs once OnTurnComplete fires — not
// merely once RunInference returns (buildTurnEvents' completion callback is
// the true end of the turn on the async delegated path).
func TestDelegatedTransport_RunInference_VoiceMode_EntersBeforeDispatchExitsAfterComplete(t *testing.T) {
	be := &voiceModeBackendDT{}
	var enterAtDispatch int
	be.mockBackendDT.sendToPaneFn = func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
		enterAtDispatch, _ = be.counts()
		if handler != nil && handler.OnTurnComplete != nil {
			handler.OnTurnComplete(&delegator.TurnResult{Text: "done"})
		}
		return nil, nil
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello"}, nil)
	ts.Trigger = "voice"
	ts.Prompt = "hello"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	if enterAtDispatch != 1 {
		t.Errorf("EnterVoiceMode call count at dispatch time = %d, want 1 (must run before the prompt reaches the backend)", enterAtDispatch)
	}
	enter, exit := be.counts()
	if enter != 1 {
		t.Errorf("EnterVoiceMode calls = %d, want 1", enter)
	}
	if exit != 1 {
		t.Errorf("ExitVoiceMode calls = %d, want 1 (fired from OnTurnComplete)", exit)
	}
}

// TestDelegatedTransport_RunInference_VoiceMode_SkippedForNonVoiceTrigger
// verifies a normal (non-voice) turn never touches VoiceModer even when the
// backend implements it — the low-effort switch is scoped to voice-originated
// turns only.
func TestDelegatedTransport_RunInference_VoiceMode_SkippedForNonVoiceTrigger(t *testing.T) {
	be := &voiceModeBackendDT{}
	be.mockBackendDT.sendToPaneFn = func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
		if handler != nil && handler.OnTurnComplete != nil {
			handler.OnTurnComplete(&delegator.TurnResult{Text: "done"})
		}
		return nil, nil
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello"}, nil)
	ts.Prompt = "hello"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	enter, exit := be.counts()
	if enter != 0 || exit != 0 {
		t.Errorf("VoiceMode calls for a non-voice trigger = enter=%d exit=%d, want 0/0", enter, exit)
	}
}

// TestDelegatedTransport_RunInference_VoiceMode_SkippedOnFold verifies that a
// voice-triggered follow-up that folds into an already in-flight turn does
// NOT enter/exit voice mode — the fold path never reaches the begin-turn
// dispatch that voice mode is scoped to (entering here would toggle effort
// mid-flight for a turn that already started, possibly under a different
// trigger).
func TestDelegatedTransport_RunInference_VoiceMode_SkippedOnFold(t *testing.T) {
	be := &voiceModeBackendDT{}
	be.mockBackendDT.turnInFlight = true
	be.mockBackendDT.sendCommandFn = func(_ context.Context, _ string) error { return nil }

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"follow-up"}, nil)
	ts.Trigger = "voice"
	ts.Prompt = "follow-up"

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	enter, exit := be.counts()
	if enter != 0 || exit != 0 {
		t.Errorf("VoiceMode calls on a folded follow-up = enter=%d exit=%d, want 0/0", enter, exit)
	}
}

// TestDelegatedTransport_RunInference_VoiceMode_ExitsOnDispatchError verifies
// that when EnterVoiceMode succeeded but the begin-turn dispatch itself then
// fails (so CC never begins a turn and OnTurnComplete will never fire),
// RunInference still calls ExitVoiceMode before returning — otherwise the
// session would be stuck at low effort until a later turn happened to
// restore it.
func TestDelegatedTransport_RunInference_VoiceMode_ExitsOnDispatchError(t *testing.T) {
	be := &voiceModeBackendDT{}
	be.mockBackendDT.sendToPaneFn = func(_ context.Context, _ string, _ *mockHandler) (*delegator.TurnResult, error) {
		return nil, errors.New("dispatch failed")
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello"}, nil)
	ts.Trigger = "voice"
	ts.Prompt = "hello"
	ts.StartedAt = time.Now()

	err := tr.RunInference(ts)
	if err == nil || !strings.Contains(err.Error(), "dispatch failed") {
		t.Fatalf("expected 'dispatch failed' error, got: %v", err)
	}

	enter, exit := be.counts()
	if enter != 1 {
		t.Errorf("EnterVoiceMode calls = %d, want 1", enter)
	}
	if exit != 1 {
		t.Errorf("ExitVoiceMode calls = %d, want 1 (dispatch failed, so OnTurnComplete never fires — RunInference must undo Enter itself)", exit)
	}
}

// TestDelegatedTransport_RunInference_VoiceMode_BackendWithoutInterface
// verifies a voice-triggered turn on a backend that doesn't implement
// VoiceModer (plain mockBackendDT) runs normally with no panic — VoiceModer
// is an optional capability, same pattern as every other backend interface.
func TestDelegatedTransport_RunInference_VoiceMode_BackendWithoutInterface(t *testing.T) {
	be := &mockBackendDT{
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
			if handler != nil && handler.OnTurnComplete != nil {
				handler.OnTurnComplete(&delegator.TurnResult{Text: "done"})
			}
			return nil, nil
		},
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hello"}, nil)
	ts.Trigger = "voice"
	ts.Prompt = "hello"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	if ts.FinalText != "done" {
		t.Errorf("FinalText = %q, want %q", ts.FinalText, "done")
	}
}

// questionBackendDT is a mockBackendDT that reports a pending AskUserQuestion
// and records the answer routed to it.
type questionBackendDT struct {
	mockBackendDT
	pendingQuestion string
	answeredReqID   string
	answeredText    string
}

func (q *questionBackendDT) HasPendingQuestion() string { return q.pendingQuestion }
func (q *questionBackendDT) RespondToQuestion(reqID, text string) error {
	q.answeredReqID, q.answeredText = reqID, text
	return nil
}
func (q *questionBackendDT) CancelQuestion(string) error { return nil }

// elicitationBackendDT is the elicitation twin of questionBackendDT.
type elicitationBackendDT struct {
	mockBackendDT
	pendingElicitation string
	answeredReqID      string
	answeredText       string
}

func (e *elicitationBackendDT) HasPendingElicitation() string { return e.pendingElicitation }
func (e *elicitationBackendDT) RespondToElicitation(reqID, text string) error {
	e.answeredReqID, e.answeredText = reqID, text
	return nil
}

// TestAnswerPendingBackendPrompt verifies the shared typed-answer capture:
// trimmed first-batch text answers a pending question (or elicitation field),
// empty/whitespace text falls through, and a backend with nothing pending is
// untouched.
func TestAnswerPendingBackendPrompt(t *testing.T) {
	q := &questionBackendDT{pendingQuestion: "req-q"}
	if !answerPendingBackendPrompt(log.NewComponentLogger("delegated"), q, "test/s", []string{"  yes please  "}) {
		t.Fatal("pending question + text: want consumed")
	}
	if q.answeredReqID != "req-q" || q.answeredText != "yes please" {
		t.Errorf("answer routed as (%q, %q), want (req-q, trimmed text)", q.answeredReqID, q.answeredText)
	}

	e := &elicitationBackendDT{pendingElicitation: "req-e"}
	if !answerPendingBackendPrompt(log.NewComponentLogger("delegated"), e, "test/s", []string{"field value"}) {
		t.Fatal("pending elicitation + text: want consumed")
	}
	if e.answeredReqID != "req-e" || e.answeredText != "field value" {
		t.Errorf("answer routed as (%q, %q), want (req-e, field value)", e.answeredReqID, e.answeredText)
	}

	q2 := &questionBackendDT{pendingQuestion: "req-q"}
	if answerPendingBackendPrompt(log.NewComponentLogger("delegated"), q2, "test/s", []string{"   "}) {
		t.Error("whitespace-only text must fall through to a normal turn")
	}
	if answerPendingBackendPrompt(log.NewComponentLogger("delegated"), q2, "test/s", nil) {
		t.Error("empty batch must fall through to a normal turn")
	}
	if answerPendingBackendPrompt(log.NewComponentLogger("delegated"), &questionBackendDT{}, "test/s", []string{"hello"}) {
		t.Error("no pending prompt: text must not be consumed")
	}
}

// TestDelegatedTransport_RunInference_SystemTurnSkipsQuestionIntercept
// verifies a system turn's text is never consumed as the answer to a pending
// AskUserQuestion — the intercept is foldable-input-only, so the system prompt
// proceeds to the normal (SourceSystem) dispatch path instead.
func TestDelegatedTransport_RunInference_SystemTurnSkipsQuestionIntercept(t *testing.T) {
	var sentToPane string
	be := &questionBackendDT{pendingQuestion: "req-q"}
	be.sendToPaneFn = func(_ context.Context, prompt string, handler *mockHandler) (*delegator.TurnResult, error) {
		sentToPane = prompt
		if handler != nil && handler.OnTurnComplete != nil {
			handler.OnTurnComplete(&delegator.TurnResult{Text: "done"})
		}
		return nil, nil
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"[keepalive]"}, nil)
	ts.Prompt = "[keepalive]"

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	if be.answeredReqID != "" {
		t.Errorf("system text was consumed as question answer (req=%q)", be.answeredReqID)
	}
	if sentToPane != "[keepalive]" {
		t.Errorf("system turn prompt = %q, want dispatched normally", sentToPane)
	}
}

// TestDelegatedTransport_RunInference_QueuedMessageNeverFolds verifies the
// per-message queue choice at the transport: an interactive (platform) turn
// whose sender marked it SteerNever does NOT fold into an in-flight turn via
// SendCommand — it dispatches like a system turn, waiting for the in-flight
// turn to complete and then beginning a fresh tracked turn.
func TestDelegatedTransport_RunInference_QueuedMessageNeverFolds(t *testing.T) {
	var sentToPane string
	var foldAttempted bool
	be := &mockBackendDT{
		turnInFlight: true,
		sendCommandFn: func(_ context.Context, _ string) error {
			foldAttempted = true
			return nil
		},
	}
	be.waitForTurnFn = func(_ context.Context) error {
		be.mu.Lock()
		be.turnInFlight = false
		be.mu.Unlock()
		return nil
	}
	be.sendToPaneFn = func(_ context.Context, prompt string, handler *mockHandler) (*delegator.TurnResult, error) {
		sentToPane = prompt
		if handler != nil && handler.OnTurnComplete != nil {
			handler.OnTurnComplete(&delegator.TurnResult{Text: "done"})
		}
		return nil, nil
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ctx := WithSteerPreference(interactiveTestCtx(), SteerNever)
	ts := NewTurnState(ctx, "test/s", []string{"queued message"}, nil)
	ts.Trigger = "testplat"
	ts.Prompt = "queued message"

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	if foldAttempted {
		t.Error("explicitly-queued message folded into in-flight turn via SendCommand")
	}
	if sentToPane != "queued message" {
		t.Errorf("fresh turn prompt = %q, want %q", sentToPane, "queued message")
	}
	select {
	case <-ts.CompletionChan:
	case <-time.After(time.Second):
		t.Fatal("CompletionChan was not closed for queued message turn")
	}
}

// TestDelegatedTransport_RunInference_SystemTurnNeverFolds verifies the system
// dispatch path: a turn with a system trigger (here: none, e.g. foci send /
// notifications) arriving while a turn is in flight does NOT fold via
// SendCommand. It waits (WaitForTurn) for the in-flight turn to complete and
// then begins a fresh turn via the exclusive SourceSystem begin.
func TestDelegatedTransport_RunInference_SystemTurnNeverFolds(t *testing.T) {
	var sentToPane string
	var foldAttempted bool
	be := &mockBackendDT{
		turnInFlight: true,
		sendCommandFn: func(_ context.Context, _ string) error {
			foldAttempted = true
			return nil
		},
	}
	be.waitForTurnFn = func(_ context.Context) error {
		// Simulate the in-flight turn completing while the system turn waits.
		be.mu.Lock()
		be.turnInFlight = false
		be.mu.Unlock()
		return nil
	}
	be.sendToPaneFn = func(_ context.Context, prompt string, handler *mockHandler) (*delegator.TurnResult, error) {
		sentToPane = prompt
		if handler != nil && handler.OnTurnComplete != nil {
			handler.OnTurnComplete(&delegator.TurnResult{Text: "done"})
		}
		return nil, nil
	}

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"[keepalive]"}, nil)
	ts.Prompt = "[keepalive]"

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	if foldAttempted {
		t.Error("system turn folded into in-flight turn via SendCommand — must never steer")
	}
	if sentToPane != "[keepalive]" {
		t.Errorf("fresh turn prompt = %q, want %q", sentToPane, "[keepalive]")
	}
	select {
	case <-ts.CompletionChan:
		// good — fresh tracked turn completed
	case <-time.After(time.Second):
		t.Fatal("CompletionChan was not closed for system turn")
	}
}

// TestDelegatedTransport_RunInference_WaitForPermission verifies that
// RunInference blocks while a permission prompt is outstanding and proceeds
// once it is cleared.
func TestDelegatedTransport_RunInference_WaitForPermission(t *testing.T) {
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
		t.Fatal("PostToolNudgeFunc should be wired into the handler")
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
		if !strings.HasPrefix(r, a.nudgePreamble()) {
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
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
	sched.Configure(nudge.Settings{Cooldown: 5, MaxPerBatch: 2, PreAnswerGate: true})
	sched.StartTurn("hi")

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{
		Model:                  "test-model",
		DelegatedManager:       mgr,
		Nudger:                 sched,
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	if capturedPreAnswerFunc == nil {
		t.Fatal("PreAnswerNudgeFunc should be wired into the handler")
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

// TestDelegatedTransport_RunInference_PreAnswerGateSuppressedOnReflection is the
// #815 regression test: the pre-answer gate must NOT fire on a reflection turn.
// Reproduces the observed 2026-06-05 22:14 firing — a reflection pass wrote
// memory files (tool_count≥min) then hit end_turn, and the gate injected
// "verify before answering" on a turn with no user-facing answer. With the
// trigger gate, PreAnswerNudgeFunc returns "" for any non-user trigger even
// when the gate is enabled and the tool threshold is met.
func TestDelegatedTransport_RunInference_PreAnswerGateSuppressedOnReflection(t *testing.T) {
	var capturedPreAnswerFunc func(*delegator.TurnResult) string
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
	sched.StartTurn("reflection prompt")

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{
		Model:                  "test-model",
		DelegatedManager:       mgr,
		Nudger:                 sched,
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"reflection prompt"}, nil)
	ts.Trigger = "reflection" // system-internal turn — the #815 condition
	ts.Prompt = "reflection prompt"
	ts.StartedAt = time.Now()

	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	if capturedPreAnswerFunc == nil {
		t.Fatal("PreAnswerNudgeFunc should be wired into the handler")
	}

	firstRound := &delegator.TurnResult{Text: "memory written"}
	if followUp := capturedPreAnswerFunc(firstRound); followUp != "" {
		t.Errorf("pre-answer gate must NOT fire on a reflection turn, got: %q", followUp)
	}
}

// TestDelegatedTransport_RunInference_PreAnswerGateDisabled verifies that
// PreAnswerNudgeFunc returns "" when the agent hasn't enabled the gate, even
// if a pre_answer rule exists — the gate config flag is the master switch.
func TestDelegatedTransport_RunInference_PreAnswerGateDisabled(t *testing.T) {
	var capturedPreAnswerFunc func(*delegator.TurnResult) string
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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

// TestDelegatedTransport_RunInference_PreAnswerKeepsRoundsSeparate verifies the
// post-fix behaviour: when the gate runs a second round, round-1 usage is
// captured into ts.PriorCallUsages as a distinct record and FinalUsage stays =
// round 2 ONLY (the true current context size). The rounds are NOT folded —
// folding cumulative cache_read across rounds double-counts the same context
// as a size signal and tripped spurious compactions. Turn-total cost (the sum
// across rounds) is preserved separately by LogUsage via ts.FinalCost.
func TestDelegatedTransport_RunInference_PreAnswerKeepsRoundsSeparate(t *testing.T) {
	var capturedHandler *mockHandler
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
	sched.Configure(nudge.Settings{Cooldown: 5, MaxPerBatch: 2, PreAnswerGate: true})
	sched.StartTurn("hi")

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{
		Model:                  "test-model",
		DelegatedManager:       mgr,
		Nudger:                 sched,
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
	// FinalUsage = round 2 ONLY (un-folded): 20 input, 15 output, 0 cache read.
	if ts.FinalUsage.InputTokens != 20 {
		t.Errorf("FinalUsage.InputTokens = %d, want 20 (round 2 only, not folded)", ts.FinalUsage.InputTokens)
	}
	if ts.FinalUsage.OutputTokens != 15 {
		t.Errorf("FinalUsage.OutputTokens = %d, want 15 (round 2 only, not folded)", ts.FinalUsage.OutputTokens)
	}
	if ts.FinalUsage.CacheReadInputTokens != 0 {
		t.Errorf("FinalUsage.CacheReadInputTokens = %d, want 0 (round 2 only, not folded)", ts.FinalUsage.CacheReadInputTokens)
	}
	// Round-1 usage is preserved as a distinct record in PriorCallUsages.
	if len(ts.PriorCallUsages) != 1 {
		t.Fatalf("PriorCallUsages len = %d, want 1 (round-1 stashed)", len(ts.PriorCallUsages))
	}
	if got := ts.PriorCallUsages[0]; got.InputTokens != 100 || got.OutputTokens != 40 || got.CacheReadInputTokens != 10 {
		t.Errorf("PriorCallUsages[0] = {in:%d out:%d cr:%d}, want {100 40 10} (round 1, un-summed)",
			got.InputTokens, got.OutputTokens, got.CacheReadInputTokens)
	}
}

// TestDelegatedTransport_GatedTurn_TwoRowsNoSpuriousCompaction is the end-to-end
// regression for the 16:00 cache_read double-count bug. A gate-fired delegated
// turn (two terminal calls, each reading the full ~55k context) must:
//  1. write TWO api.db ledger rows, each with its OWN un-summed cache_read,
//  2. record turn-total cost = sum of the two per-call costs in ts.FinalCost,
//  3. NOT trigger compaction — the compaction trigger sees only round-2's size
//     (≈56k < 60k threshold) even though round-1 + round-2 cache_read sums to
//     >threshold. Pre-fix, the fold made the trigger read the doubled ~111k and
//     compact on every gated turn.
func TestDelegatedTransport_GatedTurn_TwoRowsNoSpuriousCompaction(t *testing.T) {
	// Real api.db so we can count the rows written by LogUsage.
	dbPath := filepath.Join(t.TempDir(), "api.db")
	if err := log.InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	t.Cleanup(func() { log.CloseAPIDB() })

	const model = "claude-sonnet-4-5"
	store := session.NewStore(t.TempDir())
	// Threshold 0.3 of a 200k window = 60k. The 16:00 scenario exactly: each
	// round read ~55k (sum ~111k > 60k) but the real context (round 2) is < 60k.
	comp := compaction.NewCompactor(store, 0.3)
	cmdSent := false
	be := &mockBackendDT{
		sendCommandFn: func(_ context.Context, _ string) error { cmdSent = true; return nil },
	}
	a := &Agent{
		Model:            model,
		Compactor:        comp,
		DelegatedManager: newMockDelegatedManager(t, be),
	}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}

	ts := NewTurnState(context.Background(), "test/gated", []string{"hi"}, nil)
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.Backend = be
	ts.StartedAt = time.Now()
	ts.FinalModel = model
	ts.sessionFilePath = "/tmp/session.jsonl"

	// Round 1: full ~55k context read (the gate's first terminal call).
	round1 := &provider.Usage{InputTokens: 246, OutputTokens: 200, CacheReadInputTokens: 55405}
	// Round 2 (final): re-read the same ~55k context after the nudge.
	ts.PriorCallUsages = []*provider.Usage{round1}
	ts.FinalUsage = &provider.Usage{InputTokens: 2, OutputTokens: 150, CacheReadInputTokens: 55566}

	tr.LogUsage(ts)

	// (1) Two ledger rows, each with its OWN un-summed cache_read.
	rows := log.ReadAPIDBLog()
	if len(rows) != 2 {
		t.Fatalf("api.db rows = %d, want 2 (one per terminal call)", len(rows))
	}
	// Rows are chronological: prior round(s) first, final last.
	if rows[0].CacheRead != 55405 {
		t.Errorf("row[0].CacheRead = %d, want 55405 (round-1, un-summed)", rows[0].CacheRead)
	}
	if rows[1].CacheRead != 55566 {
		t.Errorf("row[1].CacheRead = %d, want 55566 (round-2, un-summed)", rows[1].CacheRead)
	}
	for i, r := range rows {
		if r.CacheRead == 110971 {
			t.Errorf("row[%d] has the folded cache_read 110971 — fold was reintroduced", i)
		}
		if r.CallType != "delegated_turn" {
			t.Errorf("row[%d].CallType = %q, want delegated_turn", i, r.CallType)
		}
	}

	// (2) Turn-total cost = sum of the two per-call costs.
	cost1 := modelinfo.Cost(model, round1.InputTokens, round1.OutputTokens, round1.CacheReadInputTokens, round1.CacheCreationInputTokens)
	cost2 := modelinfo.Cost(model, 2, 150, 55566, 0)
	wantCost := cost1 + cost2
	if diff := ts.FinalCost - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("FinalCost = %.8f, want %.8f (sum of per-call costs)", ts.FinalCost, wantCost)
	}
	if gotRowSum := rows[0].CostUSD + rows[1].CostUSD; gotRowSum-ts.FinalCost > 1e-9 || ts.FinalCost-gotRowSum > 1e-9 {
		t.Errorf("sum of row costs %.8f != FinalCost %.8f", gotRowSum, ts.FinalCost)
	}

	// (3) Compaction trigger sees ONLY round-2 size (input+cR+cW = 2+55566 ≈
	// 55568 < 60k) → no compaction, despite round1+round2 cache_read summing to
	// >60k. This is the spurious-compaction bug, fixed.
	tr.RunCompaction(ts)
	if cmdSent {
		t.Error("compaction fired on a gated turn whose real (round-2) size is below threshold — the fold bug is back")
	}
}

func TestDelegatedTransport_RunCompaction_DeferredWhileSubagentInFlight(t *testing.T) {
	// Over-threshold context MUST compact — unless background work (an Agent-tool
	// subagent / run_in_background Bash / the autonomous run its completion
	// triggers) is in flight, in which case compaction is deferred so /compact
	// doesn't rewrite the transcript out from under work that reports back into
	// it. The threshold re-fires every turn, so this is a defer, not a skip.
	const model = "claude-sonnet-4-5"
	store := session.NewStore(t.TempDir())
	comp := compaction.NewCompactor(store, 0.3) // 30% of ~200k = ~60k threshold

	// 150k context is well over the ~60k threshold, so size alone would compact.
	newTS := func(a *Agent, be *mockBackendDT) *TurnState {
		ts := NewTurnState(context.Background(), "test/compact-gate", []string{"hi"}, nil)
		ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
		ts.Backend = be
		ts.FinalModel = model
		ts.FinalUsage = &provider.Usage{InputTokens: 2, OutputTokens: 150, CacheReadInputTokens: 150000}
		return ts
	}

	t.Run("in-flight defers", func(t *testing.T) {
		cmdSent := false
		be := &mockBackendDT{sendCommandFn: func(_ context.Context, _ string) error { cmdSent = true; return nil }}
		be.setAwaiting(true) // subagent pending
		a := &Agent{Model: model, Compactor: comp, DelegatedManager: newMockDelegatedManager(t, be)}
		tr := &DelegatedTransport{sharedTurnOps{agent: a}}

		tr.RunCompaction(newTS(a, be))
		if cmdSent {
			t.Error("compaction fired while a subagent was in flight — it must be deferred until background work drains")
		}
	})

	t.Run("idle compacts", func(t *testing.T) {
		// Control: same over-threshold size, no background work → compaction runs.
		cmdSent := false
		be := &mockBackendDT{sendCommandFn: func(_ context.Context, _ string) error { cmdSent = true; return nil }}
		be.setAwaiting(false)
		a := &Agent{Model: model, Compactor: comp, DelegatedManager: newMockDelegatedManager(t, be)}
		tr := &DelegatedTransport{sharedTurnOps{agent: a}}

		tr.RunCompaction(newTS(a, be))
		if !cmdSent {
			t.Error("compaction did NOT fire on an over-threshold idle session — the gate is blocking the normal path")
		}
	})
}

// TestDelegatedTransport_RunInference_PreAnswerSentinelRestoresOriginal
// verifies that when the second round echoes NoResponseSentinel ("my
// original answer stands"), FinalText is replaced with round-1's text so
// the platform delivers the answer the agent originally committed to rather
// than a raw sentinel literal.
func TestDelegatedTransport_RunInference_PreAnswerSentinelRestoresOriginal(t *testing.T) {
	var capturedHandler *mockHandler
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
	sched.Configure(nudge.Settings{Cooldown: 5, MaxPerBatch: 2, PreAnswerGate: true})
	sched.StartTurn("hi")

	mgr := newMockDelegatedManager(t, be)
	a := &Agent{
		Model:                  "test-model",
		DelegatedManager:       mgr,
		Nudger:                 sched,
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
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
	// Set up a temp conversation DB so convo.Record() actually writes.
	dir := t.TempDir()
	agentID := "test"
	err := convo.InitPerAgent([]string{agentID}, func(id string) string {
		return dir + "/" + id + ".db"
	})
	if err != nil {
		t.Fatalf("InitPerAgentConversation: %v", err)
	}
	defer convo.Close()

	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
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
	mgr.AttachDelivery = a.AttachDelivery
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}

	// The session key must start with the agent ID so resolveConvLog routes
	// to the right DB.
	sk := agentID + "/c12345"
	// Wrap the test's BufferSink with loggingSink so intermediate TextBlock
	// events fire conversation-DB logging. SessionEvents now binds to the session
	// router (not the ctx sink), so — mirroring production, where Agent.RunTurn /
	// the orchestrator register the per-turn sink — a test driving RunInference
	// directly registers its sink on the router itself.
	meta := &TurnMetadata{UserID: "u1", Username: "dick"}
	wrappedSink := newLoggingSink(turnevent.NewBufferSink(), a, 42, meta, sk)
	a.sessionRouter(sk).Register(wrappedSink)
	defer a.sessionRouter(sk).Clear()
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

// captureTextSink records intermediate TextBlock texts routed to it.
type captureTextSink struct{ got *[]string }

func (c captureTextSink) Emit(_ context.Context, ev turnevent.Event) {
	if tb, ok := ev.(turnevent.TextBlock); ok {
		*c.got = append(*c.got, tb.Text)
	}
}
func (captureTextSink) DeliversToPlatform() bool { return true }

// TestDelegatedTransport_SinkLessTurnBindsRouterNotNopSink is the #1068 Phase 1
// regression. A sink-less (system) turn used to bind the session's shared
// SessionEvents to NopSink — SinkFromContext(ctx) of a no-sink ctx — for the
// session lifetime, so a concurrent autonomous run's OnText vanished. Now
// SessionEvents binds to the session ROUTER, so the backend's OnText reaches
// whatever the router forwards to. Proven here: a sink registered on the router
// receives the backend's OnText even though the turn's own ctx carries no sink.
func TestDelegatedTransport_SinkLessTurnBindsRouterNotNopSink(t *testing.T) {
	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
			if handler != nil {
				if handler.OnText != nil {
					handler.OnText("one")
					handler.OnText("two")
					handler.OnText("three")
				}
				if handler.OnTurnComplete != nil {
					handler.OnTurnComplete(&delegator.TurnResult{Text: "onetwothree", Model: "test-model"})
				}
			}
			return nil, nil
		},
	}
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	mgr.AttachDelivery = a.AttachDelivery
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}

	sk := "test/c999"
	var got []string
	a.sessionRouter(sk).Register(captureTextSink{got: &got})
	defer a.sessionRouter(sk).Clear()

	// context.Background carries NO sink → SinkFromContext == NopSink: exactly the
	// sink-less system turn that used to poison the binding.
	ts := NewTurnState(context.Background(), sk, []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()
	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	<-ts.CompletionChan

	if len(got) != 3 {
		t.Fatalf("router sink got %d texts, want 3 — SessionEvents must bind the router, not the ctx's NopSink; got=%v", len(got), got)
	}
}

// TestDelegatedTransport_ThinkingBufferResetPerTurn is the Phase 2 review flag-5
// regression. Thinking deltas accumulate into a session-scoped buffer (installed
// by AttachDelivery). A between-turns autonomous run streams thinking into that
// same buffer but has no turn of its own to drain it, so without a per-turn reset
// its thinking would be misattributed to the NEXT turn's conversation-DB entry.
// RunInference discards the buffer at turn start; this proves the next turn logs
// only its own thinking.
func TestDelegatedTransport_ThinkingBufferResetPerTurn(t *testing.T) {
	dir := t.TempDir()
	agentID := "test"
	if err := convo.InitPerAgent([]string{agentID}, func(id string) string {
		return dir + "/" + id + ".db"
	}); err != nil {
		t.Fatalf("InitPerAgent: %v", err)
	}
	defer convo.Close()

	be := &mockBackendDT{
		sessionFile: "/tmp/session.jsonl",
		sendToPaneFn: func(_ context.Context, _ string, handler *mockHandler) (*delegator.TurnResult, error) {
			if handler != nil {
				if handler.OnThinkingDelta != nil {
					handler.OnThinkingDelta("TURN-THINK")
				}
				if handler.OnTurnComplete != nil {
					handler.OnTurnComplete(&delegator.TurnResult{Text: "done", Model: "test-model"})
				}
			}
			return nil, nil
		},
	}
	mgr := newMockDelegatedManager(t, be)
	a := &Agent{Model: "test-model", DelegatedManager: mgr}
	mgr.AttachDelivery = a.AttachDelivery
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}

	sk := agentID + "/c777"
	meta := &TurnMetadata{UserID: "u1", Username: "dick"}

	// Simulate a between-turns autonomous run: build the session delivery, then
	// stream a thinking delta with no turn to drain it.
	a.AttachDelivery(be, sk)
	be.sessionEvents.OnThinkingDelta("STALE-AUTONOMOUS")

	ts := NewTurnState(context.Background(), sk, []string{"hi"}, nil)
	ts.Prompt = "hi"
	ts.StartedAt = time.Now()
	ts.Meta = meta
	ts.ConvChatID = 77
	if err := tr.RunInference(ts); err != nil {
		t.Fatalf("RunInference: %v", err)
	}
	<-ts.CompletionChan

	db, err := sql.Open("sqlite", dir+"/"+agentID+".db")
	if err != nil {
		t.Fatalf("open DB: %v", err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT text FROM messages WHERE content_type='thinking' ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		texts = append(texts, s)
	}
	if len(texts) != 1 || texts[0] != "TURN-THINK" {
		t.Fatalf("thinking log = %v, want exactly [TURN-THINK] — the stale autonomous delta must not leak into this turn's entry", texts)
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
	// lastMessageTime moved to OrchestrateFullTurn (central write); UpdateSessionMeta
	// only tracks tokens/cost now.
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
	a.CompactionNotifyFunc.Add(func(_, _, _ string) { notifyFired = true })

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
	a.CompactionNotifyFunc.Add(func(_, _, _ string) { notifyCalled = true })
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
	a.CompactionNotifyFunc.Add(func(_, _, _ string) { notifyCalled = true })
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
	// An empty compaction-summary.md override in the agent's prompts dir makes
	// compactionSummaryPrompt() resolve to "" (file read + TrimSpace), the
	// file-only successor to the old compaction_summary_prompt = "none" disable.
	emptyPromptDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(emptyPromptDir, "compaction-summary.md"), []byte(""), 0o644); err != nil {
		t.Fatalf("write empty prompt override: %v", err)
	}
	a := &Agent{
		Model:            "test-model",
		Compactor:        comp,
		DelegatedManager: newMockDelegatedManager(t, be),
		PromptSearchDirs: []string{emptyPromptDir},
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

// TestDisplayUsage_SumsDeltasButNotCacheRead locks the summable-vs-cumulative
// rule: input/output/cache_write sum across the gate's two rounds, but
// cache_read is the last call only (cumulative — summing double-counts the same
// context, the bug behind the spurious compactions).
func TestDisplayUsage_SumsDeltasButNotCacheRead(t *testing.T) {
	// nil usage -> nil
	if (&TurnState{}).DisplayUsage() != nil {
		t.Fatal("DisplayUsage() with nil FinalUsage should return nil")
	}

	// Non-gated turn: no PriorCallUsages -> FinalUsage values unchanged.
	plain := &TurnState{FinalUsage: &provider.Usage{
		InputTokens: 2, OutputTokens: 100, CacheReadInputTokens: 55566, CacheCreationInputTokens: 484,
	}}
	if got := plain.DisplayUsage(); got.InputTokens != 2 || got.OutputTokens != 100 ||
		got.CacheReadInputTokens != 55566 || got.CacheCreationInputTokens != 484 {
		t.Fatalf("non-gated DisplayUsage mismatch: %+v", got)
	}

	// Gated turn: round-1 in PriorCallUsages, round-2 in FinalUsage (the real
	// 16:00 scenario shape). in/out/cw sum; cache_read = round-2 only.
	gated := &TurnState{
		FinalUsage:      &provider.Usage{InputTokens: 2, OutputTokens: 6875, CacheReadInputTokens: 55566, CacheCreationInputTokens: 484},
		PriorCallUsages: []*provider.Usage{{InputTokens: 246, OutputTokens: 3038, CacheReadInputTokens: 55405, CacheCreationInputTokens: 161}},
	}
	got := gated.DisplayUsage()
	if got.InputTokens != 248 { // 2 + 246
		t.Errorf("InputTokens = %d, want 248 (summed)", got.InputTokens)
	}
	if got.OutputTokens != 9913 { // 6875 + 3038
		t.Errorf("OutputTokens = %d, want 9913 (summed)", got.OutputTokens)
	}
	if got.CacheCreationInputTokens != 645 { // 484 + 161
		t.Errorf("CacheCreationInputTokens = %d, want 645 (summed)", got.CacheCreationInputTokens)
	}
	if got.CacheReadInputTokens != 55566 { // round-2 only, NOT 110971
		t.Errorf("CacheReadInputTokens = %d, want 55566 (last call only, never summed)", got.CacheReadInputTokens)
	}
}

// TestDelegatedUpdateSessionMeta_SyntheticModelNotRecorded: CC reports
// "<synthetic>" on results served without an API call; recording it as the
// session's live model locks branches into an unlaunchable `--model` (the
// 2026-07-13 keepalive breakage). A real model must still be recorded.
func TestDelegatedUpdateSessionMeta_SyntheticModelNotRecorded(t *testing.T) {
	a := &Agent{Model: "claude-fable-5"}
	tr := &DelegatedTransport{sharedTurnOps{agent: a}}

	ts := NewTurnState(context.Background(), "bot/c100", []string{"hi"}, nil)
	ts.SessionMeta = &sessionMeta{model: "claude-opus-4-8"}
	ts.FinalUsage = &provider.Usage{}

	ts.FinalModel = SyntheticModel
	tr.UpdateSessionMeta(ts)
	if ts.SessionMeta.model != "claude-opus-4-8" {
		t.Errorf("model = %q after synthetic result, want claude-opus-4-8 kept", ts.SessionMeta.model)
	}

	ts.FinalModel = "claude-fable-5"
	tr.UpdateSessionMeta(ts)
	if ts.SessionMeta.model != "claude-fable-5" {
		t.Errorf("model = %q after real result, want claude-fable-5 recorded", ts.SessionMeta.model)
	}
}

// TestSessionModelFiltersSyntheticPollution: a session meta polluted with the
// sentinel before the write-guard existed must resolve to the agent default,
// both directly and via a child session's root fallback.
func TestSessionModelFiltersSyntheticPollution(t *testing.T) {
	a := &Agent{Model: "claude-fable-5"}
	sm := a.getSessionMeta("bot/c100")
	a.metaMu.Lock()
	sm.model = SyntheticModel
	a.metaMu.Unlock()

	if got := a.SessionModel("bot/c100"); got != "claude-fable-5" {
		t.Errorf("SessionModel(root) = %q, want agent default", got)
	}
	if got := a.SessionModel("bot/c100/b1"); got != "claude-fable-5" {
		t.Errorf("SessionModel(child of polluted root) = %q, want agent default", got)
	}
}

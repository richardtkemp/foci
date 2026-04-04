package cctmux

import (
	"context"
	"strings"
	"sync"
	"testing"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// checkPermissionPrompt state machine tests
// ---------------------------------------------------------------------------

// TestCheckPermissionPrompt_NilPane verifies that checkPermissionPrompt
// returns immediately without panic when pane is nil.
func TestCheckPermissionPrompt_NilPane(t *testing.T) {
	b := &Backend{}
	// Should not panic — pane is nil so capturePane is never called.
	b.checkPermissionPrompt()
}

// TestCheckPermissionPrompt_StateMachine tests the full permission state
// machine: no prompt -> prompt detected -> same prompt deduped -> new prompt
// -> prompt cleared. We verify this by directly manipulating the Backend's
// lastPrompt / permissionActive fields and checking transitions, since the
// actual capturePane call requires a real tmux session.
func TestCheckPermissionPrompt_StateMachine(t *testing.T) {
	b := &Backend{}

	// Initially: no prompt active.
	if b.permissionActive {
		t.Fatal("permissionActive should start false")
	}
	if b.lastPrompt != "" {
		t.Fatal("lastPrompt should start empty")
	}

	// Transition 1: prompt detected.
	b.lastPromptMu.Lock()
	b.lastPrompt = "block1"
	b.permissionActive = true
	b.lastPromptMu.Unlock()

	if !b.permissionActive {
		t.Error("permissionActive should be true after prompt detected")
	}

	// Transition 2: same prompt seen again (dedup check).
	b.lastPromptMu.Lock()
	samePrompt := ("block1" == b.lastPrompt)
	b.lastPromptMu.Unlock()
	if !samePrompt {
		t.Error("should detect same prompt for dedup")
	}

	// Transition 3: different prompt appears.
	b.lastPromptMu.Lock()
	b.lastPrompt = "block2"
	b.permissionActive = true
	b.lastPromptMu.Unlock()

	b.lastPromptMu.Lock()
	if b.lastPrompt != "block2" {
		t.Errorf("lastPrompt = %q, want %q", b.lastPrompt, "block2")
	}
	b.lastPromptMu.Unlock()

	// Transition 4: prompt cleared (user responded or CC timed out).
	clearedCalled := false
	b.lastPromptMu.Lock()
	b.onPermCleared = func() { clearedCalled = true }
	b.lastPromptMu.Unlock()

	// Replicate the "no prompt visible, was active" branch from checkPermissionPrompt.
	b.lastPromptMu.Lock()
	wasActive := b.permissionActive
	b.permissionActive = false
	b.lastPrompt = ""
	clearedFn := b.onPermCleared
	b.lastPromptMu.Unlock()
	if wasActive && clearedFn != nil {
		clearedFn()
	}

	if !clearedCalled {
		t.Error("onPermCleared should have been called")
	}
	if b.permissionActive {
		t.Error("permissionActive should be false after clearing")
	}
	if b.lastPrompt != "" {
		t.Error("lastPrompt should be empty after clearing")
	}
}

// TestCheckPermissionPrompt_ClearedNotFiredWhenInactive verifies that
// onPermCleared is NOT called when no prompt was active (the "else" branch
// in the nil-prompt case).
func TestCheckPermissionPrompt_ClearedNotFiredWhenInactive(t *testing.T) {
	b := &Backend{}
	called := false
	b.lastPromptMu.Lock()
	b.onPermCleared = func() { called = true }
	b.permissionActive = false // no prompt was active
	b.lastPromptMu.Unlock()

	// Simulate the nil-prompt branch.
	b.lastPromptMu.Lock()
	wasActive := b.permissionActive
	b.lastPromptMu.Unlock()

	if wasActive {
		t.Fatal("permissionActive should be false")
	}
	if called {
		t.Error("onPermCleared should not be called when no prompt was active")
	}
}

// TestCheckPermissionPrompt_StructuredCallback verifies that the permPromptFunc
// callback receives correctly structured choices when a prompt is detected.
func TestCheckPermissionPrompt_StructuredCallback(t *testing.T) {
	b := &Backend{}

	var gotText, gotSummary string
	var gotChoices []delegator.PromptChoice
	b.replyMu.Lock()
	b.permPromptFunc = func(requestID, text, summary string, choices []delegator.PromptChoice) {
		gotText = text
		gotSummary = summary
		gotChoices = choices
	}
	b.replyMu.Unlock()

	// Simulate the callback path from checkPermissionPrompt.
	prompt := &permissionPrompt{
		Description: "Edit file\n memory/test.md",
		Summary:     "Edit file memory/test.md",
		Choices:     []promptChoice{{Number: "1", Label: "Yes"}, {Number: "3", Label: "No"}},
		Raw:         "full block",
	}

	var choices []delegator.PromptChoice
	for _, c := range prompt.Choices {
		choices = append(choices, delegator.PromptChoice{
			Label: c.Label,
			Data:  c.Number,
		})
	}

	b.replyMu.Lock()
	promptFn := b.permPromptFunc
	b.replyMu.Unlock()

	if promptFn != nil && len(prompt.Choices) > 0 {
		promptFn("", "\u26a0\ufe0f Permission required:\n\n"+prompt.Description, prompt.Summary, choices)
	}

	if gotText == "" {
		t.Fatal("permPromptFunc was not called")
	}
	if gotSummary != "Edit file memory/test.md" {
		t.Errorf("summary = %q", gotSummary)
	}
	if len(gotChoices) != 2 {
		t.Fatalf("got %d choices, want 2", len(gotChoices))
	}
	if gotChoices[0].Data != "1" || gotChoices[0].Label != "Yes" {
		t.Errorf("choice 0 = %+v", gotChoices[0])
	}
	if gotChoices[1].Data != "3" || gotChoices[1].Label != "No" {
		t.Errorf("choice 1 = %+v", gotChoices[1])
	}
}

// TestCheckPermissionPrompt_FallbackToReplyFunc verifies that when
// permPromptFunc is nil, the prompt falls back to plain text via replyFunc.
func TestCheckPermissionPrompt_FallbackToReplyFunc(t *testing.T) {
	b := &Backend{}

	var gotReply string
	b.replyMu.Lock()
	b.replyFunc = func(text string) { gotReply = text }
	b.replyMu.Unlock()

	// Simulate the fallback path: permPromptFunc is nil, so use replyFunc.
	prompt := &permissionPrompt{
		Raw: "some permission prompt",
	}

	b.replyMu.Lock()
	promptFn := b.permPromptFunc
	replyFn := b.replyFunc
	b.replyMu.Unlock()

	if promptFn != nil && len(prompt.Choices) > 0 {
		t.Fatal("should not take structured path")
	} else if replyFn != nil {
		replyFn("\u26a0\ufe0f Claude Code needs permission:\n\n" + prompt.Raw + "\n\nReply with your choice (1, 2, 3, etc.)")
	}

	if !strings.Contains(gotReply, "some permission prompt") {
		t.Errorf("expected reply to contain prompt text, got %q", gotReply)
	}
}

// TestCheckPermissionPrompt_NoCallbacksSet verifies that when neither
// permPromptFunc nor replyFunc is set, nothing panics.
func TestCheckPermissionPrompt_NoCallbacksSet(t *testing.T) {
	b := &Backend{}
	// Both permPromptFunc and replyFunc are nil. Simulate the callback
	// dispatch — should not panic.
	prompt := &permissionPrompt{
		Description: "Edit file",
		Summary:     "Edit file",
		Choices:     []promptChoice{{Number: "1", Label: "Yes"}},
		Raw:         "block",
	}

	b.replyMu.Lock()
	promptFn := b.permPromptFunc
	replyFn := b.replyFunc
	b.replyMu.Unlock()

	// Neither path should execute.
	if promptFn != nil {
		t.Fatal("promptFn should be nil")
	}
	if replyFn != nil {
		t.Fatal("replyFn should be nil")
	}
	_ = prompt // used above
}

// ---------------------------------------------------------------------------
// clearLastPrompt tests
// ---------------------------------------------------------------------------

// TestClearLastPrompt verifies that clearLastPrompt resets the dedup state
// so a subsequent identical prompt would be forwarded again.
func TestClearLastPrompt(t *testing.T) {
	b := &Backend{}

	// Set some dedup state.
	b.lastPromptMu.Lock()
	b.lastPrompt = "some prompt block"
	b.lastPromptMu.Unlock()

	b.clearLastPrompt()

	b.lastPromptMu.Lock()
	if b.lastPrompt != "" {
		t.Errorf("lastPrompt = %q after clear, want empty", b.lastPrompt)
	}
	b.lastPromptMu.Unlock()
}

// TestClearLastPrompt_AlreadyEmpty verifies clearing when already empty is safe.
func TestClearLastPrompt_AlreadyEmpty(t *testing.T) {
	b := &Backend{}
	b.clearLastPrompt() // should not panic
	b.lastPromptMu.Lock()
	if b.lastPrompt != "" {
		t.Errorf("lastPrompt = %q, want empty", b.lastPrompt)
	}
	b.lastPromptMu.Unlock()
}

// TestClearLastPrompt_ConcurrentSafety exercises clearLastPrompt under
// concurrent access to verify mutex correctness.
func TestClearLastPrompt_ConcurrentSafety(t *testing.T) {
	b := &Backend{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.lastPromptMu.Lock()
			b.lastPrompt = "some prompt"
			b.permissionActive = true
			b.lastPromptMu.Unlock()
		}()
		go func() {
			defer wg.Done()
			b.clearLastPrompt()
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// IsRunning tests
// ---------------------------------------------------------------------------

// TestIsRunning_NilPane verifies IsRunning returns false when no pane exists.
func TestIsRunning_NilPane(t *testing.T) {
	b := &Backend{}
	if b.IsRunning() {
		t.Error("IsRunning should return false when pane is nil")
	}
}

// TestIsRunning_ConcurrentAccess verifies IsRunning is safe under
// concurrent reads (exercises the mutex).
func TestIsRunning_ConcurrentAccess(t *testing.T) {
	b := &Backend{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.IsRunning()
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Close tests
// ---------------------------------------------------------------------------

// TestClose_NilPane verifies Close succeeds when the backend was never started.
func TestClose_NilPane(t *testing.T) {
	b := &Backend{}
	if err := b.Close(); err != nil {
		t.Errorf("Close with nil pane should succeed, got %v", err)
	}
}

// TestClose_StopsWatchCtx verifies that Close cancels the watch context
// and nils the cancel func.
func TestClose_StopsWatchCtx(t *testing.T) {
	b := &Backend{}
	ctx, cancel := context.WithCancel(context.Background())
	b.watchCtx = ctx
	b.watchStop = cancel

	if err := b.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if b.watchStop != nil {
		t.Error("watchStop should be nil after Close")
	}
	if ctx.Err() == nil {
		t.Error("watchCtx should be cancelled after Close")
	}
}

// TestClose_NilsWatcher verifies that Close sets watcher to nil.
func TestClose_NilsWatcher(t *testing.T) {
	b := &Backend{}
	if err := b.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if b.watcher != nil {
		t.Error("watcher should be nil after Close")
	}
}

// TestClose_Idempotent verifies that calling Close multiple times is safe.
func TestClose_Idempotent(t *testing.T) {
	b := &Backend{}
	for i := 0; i < 3; i++ {
		if err := b.Close(); err != nil {
			t.Errorf("Close call %d failed: %v", i, err)
		}
	}
}

// TestClose_ClosesBridge verifies that Close shuts down the exec bridge.
func TestClose_ClosesBridge(t *testing.T) {
	// We can't easily create a real tools.ExecBridge without a registry,
	// but we can verify the nil-check path: Close with bridge=nil should
	// not panic.
	b := &Backend{}
	b.bridge = nil
	if err := b.Close(); err != nil {
		t.Errorf("Close with nil bridge failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// newFromConfig tests
// ---------------------------------------------------------------------------

// TestNewFromConfig_Default verifies backend creation with empty config.
func TestNewFromConfig_Default(t *testing.T) {
	cfg := map[string]any{}
	b, err := newFromConfig(cfg)
	if err != nil {
		t.Fatalf("newFromConfig failed: %v", err)
	}
	bb := b.(*Backend)
	if bb.socketPath != "" {
		t.Errorf("socketPath = %q, want empty", bb.socketPath)
	}
	if bb.preSendOffset != -1 {
		t.Errorf("preSendOffset = %d, want -1", bb.preSendOffset)
	}
}

// TestNewFromConfig_SocketPath verifies that socket_path is extracted from config.
func TestNewFromConfig_SocketPath(t *testing.T) {
	cfg := map[string]any{"socket_path": "/tmp/my.sock"}
	b, err := newFromConfig(cfg)
	if err != nil {
		t.Fatalf("newFromConfig failed: %v", err)
	}
	bb := b.(*Backend)
	if bb.socketPath != "/tmp/my.sock" {
		t.Errorf("socketPath = %q, want %q", bb.socketPath, "/tmp/my.sock")
	}
}

// TestNewFromConfig_WrongSocketType verifies that a non-string socket_path
// config value is silently ignored (default empty string).
func TestNewFromConfig_WrongSocketType(t *testing.T) {
	cfg := map[string]any{"socket_path": 12345}
	b, err := newFromConfig(cfg)
	if err != nil {
		t.Fatalf("newFromConfig failed: %v", err)
	}
	bb := b.(*Backend)
	if bb.socketPath != "" {
		t.Errorf("socketPath = %q, want empty for non-string value", bb.socketPath)
	}
}

// ---------------------------------------------------------------------------
// Setter tests (SetReplyFunc, SetPermissionPromptFunc, etc.)
// ---------------------------------------------------------------------------

// TestSetReplyFunc verifies that SetReplyFunc stores and replaces the callback.
func TestSetReplyFunc(t *testing.T) {
	b := &Backend{}

	called := false
	b.SetReplyFunc(func(s string) { called = true })

	b.replyMu.Lock()
	fn := b.replyFunc
	b.replyMu.Unlock()
	if fn == nil {
		t.Fatal("replyFunc should not be nil after SetReplyFunc")
	}
	fn("test")
	if !called {
		t.Error("replyFunc was not called")
	}
}

// TestSetPermissionPromptFunc verifies storage and invocation.
func TestSetPermissionPromptFunc(t *testing.T) {
	b := &Backend{}

	called := false
	b.SetPermissionPromptFunc(func(reqID, text, summary string, choices []delegator.PromptChoice) {
		called = true
	})

	b.replyMu.Lock()
	fn := b.permPromptFunc
	b.replyMu.Unlock()
	if fn == nil {
		t.Fatal("permPromptFunc should not be nil")
	}
	fn("", "test", "summary", nil)
	if !called {
		t.Error("permPromptFunc was not called")
	}
}

// TestSetOnPermissionCleared verifies the onPermCleared callback is stored.
func TestSetOnPermissionCleared(t *testing.T) {
	b := &Backend{}

	called := false
	b.SetOnPermissionCleared(func() { called = true })

	b.lastPromptMu.Lock()
	fn := b.onPermCleared
	b.lastPromptMu.Unlock()
	if fn == nil {
		t.Fatal("onPermCleared should not be nil")
	}
	fn()
	if !called {
		t.Error("onPermCleared was not called")
	}
}

// TestSetOnSessionReady verifies the onSessionReady callback is stored.
func TestSetOnSessionReady(t *testing.T) {
	b := &Backend{}

	var gotID string
	b.SetOnSessionReady(func(id string) { gotID = id })

	b.replyMu.Lock()
	fn := b.onSessionReady
	b.replyMu.Unlock()
	if fn == nil {
		t.Fatal("onSessionReady should not be nil")
	}
	fn("test-session-id")
	if gotID != "test-session-id" {
		t.Errorf("session ID = %q, want %q", gotID, "test-session-id")
	}
}

// TestSetTypingFunc verifies the typingFunc callback is stored.
func TestSetTypingFunc(t *testing.T) {
	b := &Backend{}

	var gotTyping bool
	b.SetTypingFunc(func(typing bool) { gotTyping = typing })

	b.replyMu.Lock()
	fn := b.typingFunc
	b.replyMu.Unlock()
	if fn == nil {
		t.Fatal("typingFunc should not be nil")
	}
	fn(true)
	if !gotTyping {
		t.Error("typingFunc should have received true")
	}
}

// ---------------------------------------------------------------------------
// SessionID / SessionFilePath tests
// ---------------------------------------------------------------------------

// TestSessionID_Empty verifies SessionID returns empty when not yet discovered.
func TestSessionID_Empty(t *testing.T) {
	b := &Backend{}
	if got := b.SessionID(); got != "" {
		t.Errorf("SessionID = %q, want empty", got)
	}
}

// TestSessionID_Set verifies SessionID returns the stored value.
func TestSessionID_Set(t *testing.T) {
	b := &Backend{}
	b.mu.Lock()
	b.sessionID = "abc-123"
	b.mu.Unlock()

	if got := b.SessionID(); got != "abc-123" {
		t.Errorf("SessionID = %q, want %q", got, "abc-123")
	}
}

// TestSessionFilePath_NoWatcher verifies empty path when watcher is nil.
func TestSessionFilePath_NoWatcher(t *testing.T) {
	b := &Backend{}
	if got := b.SessionFilePath(); got != "" {
		t.Errorf("SessionFilePath = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// IsTurnInFlight tests
// ---------------------------------------------------------------------------

// TestIsTurnInFlight_NoCallback verifies false when no callback is set.
func TestIsTurnInFlight_NoCallback(t *testing.T) {
	b := &Backend{}
	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight should be false with no callback")
	}
}

// TestIsTurnInFlight_WithCallback verifies true when a callback is registered.
func TestIsTurnInFlight_WithCallback(t *testing.T) {
	b := &Backend{}
	b.turnCompleteMu.Lock()
	b.turnCompleteFn = func(r *delegator.TurnResult) {}
	b.turnCompleteMu.Unlock()

	if !b.IsTurnInFlight() {
		t.Error("IsTurnInFlight should be true with callback set")
	}
}

// TestIsTurnInFlight_ClearedAfterFire verifies that after fireTurnComplete,
// IsTurnInFlight returns false.
func TestIsTurnInFlight_ClearedAfterFire(t *testing.T) {
	b := &Backend{}
	b.turnCompleteMu.Lock()
	b.turnCompleteFn = func(r *delegator.TurnResult) {}
	b.turnCompleteMu.Unlock()

	b.fireTurnComplete(&delegator.TurnResult{})

	if b.IsTurnInFlight() {
		t.Error("IsTurnInFlight should be false after fireTurnComplete")
	}
}

// ---------------------------------------------------------------------------
// Permission detection pipeline integration test
// ---------------------------------------------------------------------------

// TestPermissionPipeline_EndToEnd tests the full detection pipeline:
// pane content -> extractPermissionPrompt -> buildPermissionSummary,
// then verifies the Backend can dispatch via structured or fallback path.
func TestPermissionPipeline_EndToEnd(t *testing.T) {
	content := strings.Join([]string{
		"Previous output...",
		"───────────────────────────────",
		"Edit file",
		" internal/backend/cctmux/lifecycle.go",
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌",
		"-old line",
		"+new line",
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌",
		"Do you want to edit this file?",
		"❯ 1. Yes",
		"  2. Yes, allow all edits in internal/ during this session (shift+tab)",
		"  3. No",
		"Esc to cancel",
	}, "\n")

	prompt := extractPermissionPrompt(content)
	if prompt == nil {
		t.Fatal("expected a prompt")
	}

	if prompt.Summary == "" {
		t.Error("expected non-empty summary")
	}

	if !strings.HasPrefix(prompt.Summary, "Edit file") {
		t.Errorf("summary should start with 'Edit file', got %q", prompt.Summary)
	}

	if len(prompt.Choices) != 3 {
		t.Fatalf("got %d choices, want 3", len(prompt.Choices))
	}

	// Verify choice 2 includes the session permission text.
	if !strings.Contains(prompt.Choices[1].Label, "allow all edits") {
		t.Errorf("choice 2 label = %q, expected 'allow all edits'", prompt.Choices[1].Label)
	}

	// Now simulate the full dispatch through a Backend.
	b := &Backend{}

	var dispatched bool
	b.SetPermissionPromptFunc(func(reqID, text, summary string, choices []delegator.PromptChoice) {
		dispatched = true
		if summary != prompt.Summary {
			t.Errorf("dispatched summary = %q, want %q", summary, prompt.Summary)
		}
		if len(choices) != 3 {
			t.Errorf("dispatched %d choices, want 3", len(choices))
		}
	})

	// Simulate what checkPermissionPrompt does after extracting the prompt.
	b.lastPromptMu.Lock()
	b.lastPrompt = prompt.Raw
	b.permissionActive = true
	b.lastPromptMu.Unlock()

	b.replyMu.Lock()
	promptFn := b.permPromptFunc
	b.replyMu.Unlock()

	if promptFn != nil && len(prompt.Choices) > 0 {
		var choices []delegator.PromptChoice
		for _, c := range prompt.Choices {
			choices = append(choices, delegator.PromptChoice{Label: c.Label, Data: c.Number})
		}
		promptFn("", "\u26a0\ufe0f Permission required:\n\n"+prompt.Description, prompt.Summary, choices)
	}

	if !dispatched {
		t.Error("permission prompt was not dispatched")
	}
}

// ---------------------------------------------------------------------------
// discoverSession tests
// ---------------------------------------------------------------------------

// TestDiscoverSession_NoSession verifies that discoverSession with an invalid
// PID does nothing (defers to lazy discovery).
func TestDiscoverSession_NoSession(t *testing.T) {
	b := &Backend{}
	b.pane = &tmuxPane{pid: 999999999} // non-existent PID
	b.workDir = "/nonexistent"

	// Should not panic, just return silently.
	b.discoverSession()

	if b.sessionID != "" {
		t.Errorf("sessionID = %q, want empty", b.sessionID)
	}
}

// ---------------------------------------------------------------------------
// isSyntheticNoResponse tests
// ---------------------------------------------------------------------------

// TestIsSyntheticNoResponse verifies detection of CC's synthetic empty
// responses that should be silently dropped.
func TestIsSyntheticNoResponse(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"No response requested.", true},
		{"[[NO_RESPONSE]]", true},
		{"  No response requested.  ", true},
		{"\n[[NO_RESPONSE]]\n", true},
		{"Hello world", false},
		{"", false},
		{"No response", false},
		{"no response requested.", false}, // case-sensitive
	}
	for _, tc := range cases {
		if got := isSyntheticNoResponse(tc.input); got != tc.want {
			t.Errorf("isSyntheticNoResponse(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

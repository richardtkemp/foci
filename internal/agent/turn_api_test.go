package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/nudge"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// TestAPITransport_IncrementProcessing verifies the atomic counter goes up and back down.
func TestAPITransport_IncrementProcessing(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	if a.IsProcessing() {
		t.Fatal("should not be processing before IncrementProcessing")
	}

	dec := tr.IncrementProcessing(ts)
	if !a.IsProcessing() {
		t.Fatal("should be processing after IncrementProcessing")
	}
	if got := atomic.LoadInt32(&a.processing); got != 1 {
		t.Fatalf("processing = %d, want 1", got)
	}

	dec()
	if a.IsProcessing() {
		t.Fatal("should not be processing after decrement")
	}
}

// TestAPITransport_RegisterTurn verifies turn detail registration and cleanup.
func TestAPITransport_RegisterTurn(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Trigger = "telegram"

	unreg := tr.RegisterTurn(ts)

	if ts.TurnDetail == nil {
		t.Fatal("TurnDetail should be set")
	}
	if ts.TurnDetail.SessionKey != "test/s" {
		t.Errorf("TurnDetail.SessionKey = %q, want %q", ts.TurnDetail.SessionKey, "test/s")
	}
	if ts.TurnDetail.Trigger != "telegram" {
		t.Errorf("TurnDetail.Trigger = %q, want %q", ts.TurnDetail.Trigger, "telegram")
	}

	details := a.ProcessingDetails()
	if len(details) != 1 {
		t.Fatalf("ProcessingDetails len = %d, want 1", len(details))
	}

	unreg()
	details = a.ProcessingDetails()
	if len(details) != 0 {
		t.Fatalf("ProcessingDetails len = %d after unreg, want 0", len(details))
	}
}

// TestAPITransport_AcquireTurnLock verifies serialization and unlock.
func TestAPITransport_AcquireTurnLock(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
	ts.Trigger = "telegram"

	unlock := tr.AcquireTurnLock(ts)

	// Lock is held — a second goroutine should block.
	blocked := make(chan struct{})
	go func() {
		ts2 := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)
		ts2.Trigger = "keepalive"
		unlock2 := tr.AcquireTurnLock(ts2)
		close(blocked)
		unlock2()
	}()

	select {
	case <-blocked:
		t.Fatal("second lock should block while first is held")
	case <-time.After(50 * time.Millisecond):
		// expected
	}

	unlock()

	select {
	case <-blocked:
		// expected — second lock acquired after unlock
	case <-time.After(5 * time.Second):
		t.Fatal("second lock should acquire after first unlocks")
	}
}

// TestAPITransport_ResolveModelEffort verifies model/effort/thinking/speed resolution.
func TestAPITransport_ResolveModelEffort(t *testing.T) {
	a := &Agent{
		Model: "anthropic/claude-sonnet-4-20250514",
	}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	tr.ResolveModelEffort(ts)

	if ts.TurnModel != "anthropic/claude-sonnet-4-20250514" {
		t.Errorf("TurnModel = %q, want agent default", ts.TurnModel)
	}
	// No per-model defaults configured, so effort/thinking/speed stay empty.
	if ts.TurnEffort != "" {
		t.Errorf("TurnEffort = %q, want empty", ts.TurnEffort)
	}
}

// TestAPITransport_ResolveModelEffort_WithDefaults verifies model defaults apply.
func TestAPITransport_ResolveModelEffort_WithDefaults(t *testing.T) {
	a := &Agent{
		Model: "anthropic/claude-sonnet-4-20250514",
		ModelDefaultsFn: func(model string) config.ModelDefaults {
			return config.ModelDefaults{Effort: "high", Thinking: "adaptive", Speed: ""}
		},
	}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "test/s", []string{"hi"}, nil)

	tr.ResolveModelEffort(ts)

	if ts.TurnEffort != "high" {
		t.Errorf("TurnEffort = %q, want %q", ts.TurnEffort, "high")
	}
	if ts.TurnThinking != "adaptive" {
		t.Errorf("TurnThinking = %q, want %q", ts.TurnThinking, "adaptive")
	}
}

// ---------------------------------------------------------------------------
// ComposePrompt tests
// ---------------------------------------------------------------------------

// testAgentForCompose builds a minimal Agent suitable for ComposePrompt tests.
// It sets up a real session store (in a temp dir) and an empty bootstrap so
// prepareUserMessage can run without nil panics.
func testAgentForCompose(t *testing.T) (*Agent, *session.Store) {
	t.Helper()
	dir := t.TempDir()
	store := session.NewStore(dir)
	bs := workspace.NewBootstrap(t.TempDir(), []string{}) // empty workspace → empty system blocks

	a := &Agent{
		Sessions:  store,
		Bootstrap: bs,
		Model:     "anthropic/claude-sonnet-4-20250514",
	}
	return a, store
}

// TestComposePrompt_Basic verifies that ComposePrompt appends a user message
// with the expected role and text content to both Messages and NewMessages.
func TestComposePrompt_Basic(t *testing.T) {
	a, _ := testAgentForCompose(t)
	tr := &APITransport{sharedTurnOps{agent: a}}

	ctx := WithTrigger(context.Background(), "telegram")
	ctx = WithTurnMetadata(ctx, &TurnMetadata{UserID: "42", Username: "alice"})
	ts := NewTurnState(ctx, "bot/c100/1000000000", []string{"hello world"}, nil)
	ts.Meta = &TurnMetadata{UserID: "42", Username: "alice"}
	ts.TurnModel = "anthropic/claude-sonnet-4-20250514"

	if err := tr.ComposePrompt(ts); err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	if len(ts.Messages) != 1 {
		t.Fatalf("Messages len = %d, want 1", len(ts.Messages))
	}
	if ts.Messages[0].Role != "user" {
		t.Errorf("Messages[0].Role = %q, want %q", ts.Messages[0].Role, "user")
	}
	if len(ts.NewMessages) != 1 {
		t.Fatalf("NewMessages len = %d, want 1", len(ts.NewMessages))
	}

	// The user text should appear in the content blocks.
	text := provider.TextOf(ts.UserMsg.Content)
	if !strings.Contains(text, "hello world") {
		t.Errorf("UserMsg text missing user input; got %q", text)
	}
}

// TestComposePrompt_DuplicateSuppressedWithThinking verifies that duplicate
// messages are suppressed when thinking is enabled with effort above "low".
// This is the "thinking + high effort" case where duplicating hurts more than helps.
func TestComposePrompt_DuplicateSuppressedWithThinking(t *testing.T) {
	a, _ := testAgentForCompose(t)
	a.DuplicateMessages = true
	tr := &APITransport{sharedTurnOps{agent: a}}

	ctx := WithTrigger(context.Background(), "telegram")
	ctx = WithTurnMetadata(ctx, &TurnMetadata{})
	ts := NewTurnState(ctx, "bot/c100/1000000000", []string{"test"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.TurnModel = "anthropic/claude-sonnet-4-20250514"
	ts.TurnThinking = "adaptive"
	ts.TurnEffort = "high"

	if err := tr.ComposePrompt(ts); err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	if ts.EffectiveDuplicate {
		t.Error("EffectiveDuplicate should be false when thinking=adaptive + effort=high")
	}
}

// TestComposePrompt_DuplicateAllowedWithLowEffort verifies that duplicate
// messages remain enabled when effort is "low", even with thinking on.
func TestComposePrompt_DuplicateAllowedWithLowEffort(t *testing.T) {
	a, _ := testAgentForCompose(t)
	a.DuplicateMessages = true
	tr := &APITransport{sharedTurnOps{agent: a}}

	ctx := WithTrigger(context.Background(), "telegram")
	ctx = WithTurnMetadata(ctx, &TurnMetadata{})
	ts := NewTurnState(ctx, "bot/c100/1000000000", []string{"test"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.TurnModel = "anthropic/claude-sonnet-4-20250514"
	ts.TurnThinking = "adaptive"
	ts.TurnEffort = "low"

	if err := tr.ComposePrompt(ts); err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	if !ts.EffectiveDuplicate {
		t.Error("EffectiveDuplicate should be true when effort=low")
	}
}

// TestComposePrompt_DuplicateAllowedWithThinkingOff verifies that duplicate
// messages remain enabled when thinking is off, regardless of effort.
func TestComposePrompt_DuplicateAllowedWithThinkingOff(t *testing.T) {
	a, _ := testAgentForCompose(t)
	a.DuplicateMessages = true
	tr := &APITransport{sharedTurnOps{agent: a}}

	ctx := WithTrigger(context.Background(), "telegram")
	ctx = WithTurnMetadata(ctx, &TurnMetadata{})
	ts := NewTurnState(ctx, "bot/c100/1000000000", []string{"test"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.TurnModel = "anthropic/claude-sonnet-4-20250514"
	ts.TurnThinking = "off"
	ts.TurnEffort = "high"

	if err := tr.ComposePrompt(ts); err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	if !ts.EffectiveDuplicate {
		t.Error("EffectiveDuplicate should be true when thinking=off")
	}
}

// TestComposePrompt_Orientation verifies that branch orientation text is
// included in the user message content blocks when ConsumeOrientation returns
// a non-empty string.
func TestComposePrompt_Orientation(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	bs := workspace.NewBootstrap(t.TempDir(), []string{})

	// Set up a root session with some history, then create a branch with orientation.
	rootKey := "bot/c100/1000000000"
	if err := store.TestAppend(rootKey, provider.Message{Role: "user", Content: provider.TextContent("msg1")}); err != nil {
		t.Fatalf("setup root: %v", err)
	}
	if err := store.TestAppend(rootKey, provider.Message{Role: "assistant", Content: provider.TextContent("reply1")}); err != nil {
		t.Fatalf("setup root: %v", err)
	}

	// OrientationTemplate is the raw template; the branch creation resolves
	// any placeholders. A literal string (no placeholders) is stored as-is.
	branchKey, err := store.CreateBranchWithOptions(rootKey, session.BranchOptions{
		OrientationTemplate: "You are branching to discuss cats.",
	})
	if err != nil {
		t.Fatalf("CreateBranchWithOptions: %v", err)
	}

	a := &Agent{
		Sessions:  store,
		Bootstrap: bs,
		Model:     "anthropic/claude-sonnet-4-20250514",
	}
	tr := &APITransport{sharedTurnOps{agent: a}}

	ctx := WithTrigger(context.Background(), "telegram")
	ctx = WithTurnMetadata(ctx, &TurnMetadata{})
	ts := NewTurnState(ctx, branchKey, []string{"tell me about cats"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.TurnModel = "anthropic/claude-sonnet-4-20250514"

	if err := tr.ComposePrompt(ts); err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	text := provider.TextOf(ts.UserMsg.Content)
	if !strings.Contains(text, "cats") {
		t.Errorf("orientation text not found in user message; got %q", text)
	}
}

// TestComposePrompt_AppendsToExistingMessages verifies that ComposePrompt
// appends to an already-populated Messages slice (e.g. from LoadAndRepairSession).
func TestComposePrompt_AppendsToExistingMessages(t *testing.T) {
	a, _ := testAgentForCompose(t)
	tr := &APITransport{sharedTurnOps{agent: a}}

	ctx := WithTrigger(context.Background(), "telegram")
	ctx = WithTurnMetadata(ctx, &TurnMetadata{})
	ts := NewTurnState(ctx, "bot/c100/1000000000", []string{"hello"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.TurnModel = "anthropic/claude-sonnet-4-20250514"

	// Pre-populate with existing session history.
	ts.Messages = []provider.Message{
		{Role: "user", Content: provider.TextContent("old message")},
		{Role: "assistant", Content: provider.TextContent("old reply")},
	}

	if err := tr.ComposePrompt(ts); err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	if len(ts.Messages) != 3 {
		t.Fatalf("Messages len = %d, want 3", len(ts.Messages))
	}
	if ts.Messages[2].Role != "user" {
		t.Errorf("Messages[2].Role = %q, want %q", ts.Messages[2].Role, "user")
	}
}

// ---------------------------------------------------------------------------
// LoadAndRepairSession tests
// ---------------------------------------------------------------------------

// TestLoadAndRepairSession_EmptySession verifies LoadAndRepairSession handles
// a session that doesn't exist yet (returns empty message slice, no error).
func TestLoadAndRepairSession_EmptySession(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	a := &Agent{Sessions: store}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)

	if err := tr.LoadAndRepairSession(ts); err != nil {
		t.Fatalf("LoadAndRepairSession: %v", err)
	}
	if len(ts.Messages) != 0 {
		t.Fatalf("Messages len = %d, want 0", len(ts.Messages))
	}
}

// TestLoadAndRepairSession_LoadsExisting verifies that LoadAndRepairSession
// correctly loads messages from an existing session file.
func TestLoadAndRepairSession_LoadsExisting(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	key := "bot/c100/1000000000"
	if err := store.TestAppend(key, provider.Message{Role: "user", Content: provider.TextContent("q1")}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := store.TestAppend(key, provider.Message{Role: "assistant", Content: provider.TextContent("a1")}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	a := &Agent{Sessions: store}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), key, []string{"hi"}, nil)

	if err := tr.LoadAndRepairSession(ts); err != nil {
		t.Fatalf("LoadAndRepairSession: %v", err)
	}
	if len(ts.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(ts.Messages))
	}
	if ts.Messages[0].Role != "user" {
		t.Errorf("Messages[0].Role = %q, want %q", ts.Messages[0].Role, "user")
	}
}

// TestLoadAndRepairSession_RepairsInterruptedToolCalls verifies that
// interrupted tool calls (assistant with tool_use and no following tool_result)
// are repaired with synthetic results.
func TestLoadAndRepairSession_RepairsInterruptedToolCalls(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	key := "bot/c100/1000000000"

	// Set up a session with an interrupted tool call (assistant with tool_use, no tool_result).
	if err := store.TestAppend(key, provider.Message{Role: "user", Content: provider.TextContent("do something")}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := store.TestAppend(key, provider.Message{
		Role: "assistant",
		Content: []provider.ContentBlock{
			{Type: "text", Text: "Let me check."},
			{Type: "tool_use", ID: "toolu_123", Name: "bash"},
		},
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	a := &Agent{Sessions: store}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), key, []string{"hi"}, nil)

	if err := tr.LoadAndRepairSession(ts); err != nil {
		t.Fatalf("LoadAndRepairSession: %v", err)
	}

	// Should have original 2 messages + repair pair (tool_result user + ack assistant).
	if len(ts.Messages) != 4 {
		t.Fatalf("Messages len = %d, want 4 (2 original + 2 repair)", len(ts.Messages))
	}
	// Repair message should be user with tool_result.
	if ts.Messages[2].Role != "user" {
		t.Errorf("Messages[2].Role = %q, want %q", ts.Messages[2].Role, "user")
	}
	if ts.Messages[3].Role != "assistant" {
		t.Errorf("Messages[3].Role = %q, want %q", ts.Messages[3].Role, "assistant")
	}
}

// TestLoadAndRepairSession_RepairsMissingAssistant verifies that consecutive
// user messages get a synthetic assistant message inserted between them.
func TestLoadAndRepairSession_RepairsMissingAssistant(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	key := "bot/c100/1000000000"

	// Two consecutive user messages (corrupt session).
	if err := store.TestAppend(key, provider.Message{Role: "user", Content: provider.TextContent("q1")}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := store.TestAppend(key, provider.Message{Role: "user", Content: provider.TextContent("q2")}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	a := &Agent{Sessions: store}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), key, []string{"hi"}, nil)

	if err := tr.LoadAndRepairSession(ts); err != nil {
		t.Fatalf("LoadAndRepairSession: %v", err)
	}

	// Should have 3 messages: user, synthetic assistant, user.
	if len(ts.Messages) != 3 {
		t.Fatalf("Messages len = %d, want 3", len(ts.Messages))
	}
	if ts.Messages[1].Role != "assistant" {
		t.Errorf("Messages[1].Role = %q, want %q (synthetic assistant)", ts.Messages[1].Role, "assistant")
	}
}

// ---------------------------------------------------------------------------
// BuildSystemAndTools tests
// ---------------------------------------------------------------------------

// TestBuildSystemAndTools_BasicBlocks verifies that BuildSystemAndTools
// populates ts.System from the bootstrap and ts.ToolDefs from the registry.
func TestBuildSystemAndTools_BasicBlocks(t *testing.T) {
	bsDir := t.TempDir()
	bs := workspace.NewBootstrap(bsDir, []string{})

	reg := tools.NewRegistry()
	a := &Agent{
		Bootstrap:        bs,
		EnvironmentBlock: "env: test",
		Tools:            reg,
	}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)

	tr.BuildSystemAndTools(ts)

	// EnvironmentBlock should appear in system blocks.
	found := false
	for _, block := range ts.System {
		if strings.Contains(block.Text, "env: test") {
			found = true
			break
		}
	}
	if !found {
		t.Error("EnvironmentBlock not found in system blocks")
	}

	// ToolDefs should be empty since no tools are registered.
	if len(ts.ToolDefs) != 0 {
		t.Errorf("ToolDefs len = %d, want 0 (no tools registered)", len(ts.ToolDefs))
	}
}

// TestBuildSystemAndTools_ServerToolsMerge verifies that ServerTools are
// appended to ToolDefs from the registry.
func TestBuildSystemAndTools_ServerToolsMerge(t *testing.T) {
	bsDir := t.TempDir()
	bs := workspace.NewBootstrap(bsDir, []string{})
	reg := tools.NewRegistry()

	serverTool := provider.NewCustomTool("web_search", "Search the web", nil)
	a := &Agent{
		Bootstrap:   bs,
		Tools:       reg,
		ServerTools: []provider.ToolDef{serverTool},
	}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)

	tr.BuildSystemAndTools(ts)

	if len(ts.ToolDefs) != 1 {
		t.Fatalf("ToolDefs len = %d, want 1 (server tool)", len(ts.ToolDefs))
	}
}

// TestBuildSystemAndTools_ExtraSystemBlocks verifies that ExtraSystemBlocks
// are included in the system prompt output.
func TestBuildSystemAndTools_ExtraSystemBlocks(t *testing.T) {
	bsDir := t.TempDir()
	bs := workspace.NewBootstrap(bsDir, []string{})
	reg := tools.NewRegistry()

	a := &Agent{
		Bootstrap:     bs,
		Tools:         reg,
		CacheStrategy: "auto",
		ExtraSystemBlocks: []provider.SystemBlock{
			{Type: "text", Text: "extra skill block"},
		},
	}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)

	tr.BuildSystemAndTools(ts)

	found := false
	for _, block := range ts.System {
		if strings.Contains(block.Text, "extra skill block") {
			found = true
			break
		}
	}
	if !found {
		t.Error("ExtraSystemBlocks not found in system blocks")
	}
}

// ---------------------------------------------------------------------------
// InjectNudges tests
// ---------------------------------------------------------------------------

// TestInjectNudges_NilNudger verifies InjectNudges is a no-op when Nudger is nil.
func TestInjectNudges_NilNudger(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)
	ts.UserMsg = provider.Message{
		Role:    "user",
		Content: provider.TextContent("original"),
	}
	ts.Messages = []provider.Message{ts.UserMsg}
	ts.NewMessages = []provider.Message{ts.UserMsg}

	tr.InjectNudges(ts)

	// Content should be unchanged.
	if len(ts.UserMsg.Content) != 1 {
		t.Errorf("UserMsg.Content len = %d, want 1 (unchanged)", len(ts.UserMsg.Content))
	}
}

// TestInjectNudges_TurnIntervalFires verifies that nudge blocks are prepended
// to the user message when an every_n_turns rule fires.
func TestInjectNudges_TurnIntervalFires(t *testing.T) {
	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{
				Text:    "Remember to be concise.",
				Trigger: nudge.Trigger{Type: "every_n_turns", N: 1},
			},
		},
	}
	scheduler := nudge.NewScheduler(rs, 5, 3)

	a := &Agent{Nudger: scheduler}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hello"}, nil)
	ts.UserMsg = provider.Message{
		Role:    "user",
		Content: provider.TextContent("hello"),
	}
	ts.Messages = []provider.Message{ts.UserMsg}
	ts.NewMessages = []provider.Message{ts.UserMsg}

	tr.InjectNudges(ts)

	// Nudge should be prepended as a new content block before the user text.
	if len(ts.UserMsg.Content) < 2 {
		t.Fatalf("UserMsg.Content len = %d, want >= 2 (nudge + original)", len(ts.UserMsg.Content))
	}
	if !strings.Contains(ts.UserMsg.Content[0].Text, "Remember to be concise.") {
		t.Errorf("first content block should contain nudge text; got %q", ts.UserMsg.Content[0].Text)
	}
	// Verify Messages and NewMessages were updated too.
	lastMsg := ts.Messages[len(ts.Messages)-1]
	if len(lastMsg.Content) != len(ts.UserMsg.Content) {
		t.Error("Messages not updated to match nudge injection")
	}
}

// TestInjectNudges_RegexFires verifies that regex nudge triggers fire when
// the user message matches the pattern.
func TestInjectNudges_RegexFires(t *testing.T) {
	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{
				Text:    "Watch out for code quality.",
				Trigger: nudge.Trigger{Type: "regex", Pattern: "(?i)refactor"},
			},
		},
	}
	scheduler := nudge.NewScheduler(rs, 5, 3)

	a := &Agent{Nudger: scheduler}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"please refactor this"}, nil)
	ts.UserMsg = provider.Message{
		Role:    "user",
		Content: provider.TextContent("please refactor this"),
	}
	ts.Messages = []provider.Message{ts.UserMsg}
	ts.NewMessages = []provider.Message{ts.UserMsg}

	tr.InjectNudges(ts)

	if len(ts.UserMsg.Content) < 2 {
		t.Fatalf("UserMsg.Content len = %d, want >= 2 (nudge + original)", len(ts.UserMsg.Content))
	}
	if !strings.Contains(ts.UserMsg.Content[0].Text, "Watch out for code quality.") {
		t.Errorf("regex nudge not found in content; got %q", ts.UserMsg.Content[0].Text)
	}
}

// TestInjectNudges_NoMatch verifies that nudge blocks are NOT prepended when
// no triggers fire (e.g. regex doesn't match).
func TestInjectNudges_NoMatch(t *testing.T) {
	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{
				Text:    "Watch out for code quality.",
				Trigger: nudge.Trigger{Type: "regex", Pattern: "(?i)refactor"},
			},
		},
	}
	scheduler := nudge.NewScheduler(rs, 5, 3)

	a := &Agent{Nudger: scheduler}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hello world"}, nil)
	ts.UserMsg = provider.Message{
		Role:    "user",
		Content: provider.TextContent("hello world"),
	}
	ts.Messages = []provider.Message{ts.UserMsg}
	ts.NewMessages = []provider.Message{ts.UserMsg}

	tr.InjectNudges(ts)

	// No nudge should be injected.
	if len(ts.UserMsg.Content) != 1 {
		t.Errorf("UserMsg.Content len = %d, want 1 (no nudge)", len(ts.UserMsg.Content))
	}
}

// ---------------------------------------------------------------------------
// SaveSession tests
// ---------------------------------------------------------------------------

// TestSaveSession_PersistsMessages verifies that SaveSession writes new
// messages to the session store and nils NewMessages to prevent double-save.
func TestSaveSession_PersistsMessages(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	a := &Agent{Sessions: store}
	tr := &APITransport{sharedTurnOps{agent: a}}

	key := "bot/c100/1000000000"
	ts := NewTurnState(context.Background(), key, []string{"hi"}, nil)
	ts.Messages = []provider.Message{
		{Role: "user", Content: provider.TextContent("q1")},
		{Role: "assistant", Content: provider.TextContent("a1")},
	}
	ts.NewMessages = []provider.Message{
		{Role: "user", Content: provider.TextContent("q1")},
		{Role: "assistant", Content: provider.TextContent("a1")},
	}

	if err := tr.SaveSession(ts); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	// NewMessages should be nil after save.
	if ts.NewMessages != nil {
		t.Error("NewMessages should be nil after SaveSession")
	}

	// Verify messages are persisted.
	loaded, err := store.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded messages = %d, want 2", len(loaded))
	}
}

// TestSaveSession_EmptyNewMessages verifies that SaveSession is a no-op
// when there are no new messages to save.
func TestSaveSession_EmptyNewMessages(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	a := &Agent{Sessions: store}
	tr := &APITransport{sharedTurnOps{agent: a}}

	key := "bot/c100/1000000000"
	ts := NewTurnState(ctx(t), key, []string{"hi"}, nil)
	ts.NewMessages = nil

	if err := tr.SaveSession(ts); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	// No file should be created.
	loaded, err := store.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 0 {
		t.Errorf("loaded messages = %d, want 0 (nothing saved)", len(loaded))
	}
}

// TestSaveSession_NilNewMessages verifies that SaveSession handles nil
// NewMessages without error.
func TestSaveSession_NilNewMessages(t *testing.T) {
	a := &Agent{Sessions: session.NewStore(t.TempDir())}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)
	ts.NewMessages = nil

	if err := tr.SaveSession(ts); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
}

// ---------------------------------------------------------------------------
// UpdateSessionMeta tests
// ---------------------------------------------------------------------------

// TestUpdateSessionMeta_Updates verifies that UpdateSessionMeta correctly
// copies final usage and cost data into the session metadata.
func TestUpdateSessionMeta_Updates(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}

	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)
	ts.SessionMeta = &sessionMeta{}
	ts.StartedAt = time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	ts.FinalCost = 0.0042
	ts.FinalUsage = &provider.Usage{
		InputTokens:              1000,
		OutputTokens:             500,
		CacheCreationInputTokens: 200,
	}

	tr.UpdateSessionMeta(ts)

	if ts.SessionMeta.lastMessageTime != ts.StartedAt {
		t.Errorf("lastMessageTime = %v, want %v", ts.SessionMeta.lastMessageTime, ts.StartedAt)
	}
	if ts.SessionMeta.prevCost != 0.0042 {
		t.Errorf("prevCost = %f, want 0.0042", ts.SessionMeta.prevCost)
	}
	if ts.SessionMeta.prevInput != 1000 {
		t.Errorf("prevInput = %d, want 1000", ts.SessionMeta.prevInput)
	}
	if ts.SessionMeta.prevOutput != 500 {
		t.Errorf("prevOutput = %d, want 500", ts.SessionMeta.prevOutput)
	}
	if ts.SessionMeta.prevCacheWrite != 200 {
		t.Errorf("prevCacheWrite = %d, want 200", ts.SessionMeta.prevCacheWrite)
	}
}

// TestUpdateSessionMeta_NilSessionMeta verifies UpdateSessionMeta is a no-op
// when SessionMeta is nil.
func TestUpdateSessionMeta_NilSessionMeta(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}

	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)
	ts.SessionMeta = nil
	ts.FinalUsage = &provider.Usage{InputTokens: 100}

	// Should not panic.
	tr.UpdateSessionMeta(ts)
}

// TestUpdateSessionMeta_NilFinalUsage verifies UpdateSessionMeta is a no-op
// when FinalUsage is nil.
func TestUpdateSessionMeta_NilFinalUsage(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}

	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)
	ts.SessionMeta = &sessionMeta{prevCost: 0.01}
	ts.FinalUsage = nil

	tr.UpdateSessionMeta(ts)

	// prevCost should be unchanged.
	if ts.SessionMeta.prevCost != 0.01 {
		t.Errorf("prevCost = %f, want 0.01 (unchanged)", ts.SessionMeta.prevCost)
	}
}

// ---------------------------------------------------------------------------
// RunCompaction tests
// ---------------------------------------------------------------------------

// TestRunCompaction_NilFinalUsage verifies RunCompaction is a no-op when
// FinalUsage is nil (no API response received, e.g. error path).
func TestRunCompaction_NilFinalUsage(t *testing.T) {
	a := &Agent{Sessions: session.NewStore(t.TempDir())}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)
	ts.FinalUsage = nil

	// Should not panic (Compactor is nil, and we bail early on nil FinalUsage).
	tr.RunCompaction(ts)
}

// TestRunCompaction_WithFinalUsage verifies RunCompaction calls maybeCompact
// when FinalUsage is populated. We can't easily verify the compact call itself
// (it requires a Compactor), but we verify it doesn't panic with nil Compactor.
func TestRunCompaction_WithFinalUsage(t *testing.T) {
	a := &Agent{Sessions: session.NewStore(t.TempDir())}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)
	ts.SessionMeta = &sessionMeta{}
	ts.FinalUsage = &provider.Usage{InputTokens: 5000, OutputTokens: 1000}
	ts.Messages = []provider.Message{
		{Role: "user", Content: provider.TextContent("q")},
		{Role: "assistant", Content: provider.TextContent("a")},
	}
	ts.System = []provider.SystemBlock{{Type: "text", Text: "system prompt"}}

	// Compactor is nil so maybeCompact will be a no-op, but we verify no panic.
	tr.RunCompaction(ts)
}

// ---------------------------------------------------------------------------
// LogUsage tests
// ---------------------------------------------------------------------------

// TestLogUsage_NoOp verifies that LogUsage is a no-op for APITransport
// (API path logs usage per-call inside RunInference via logAPIResponse).
func TestLogUsage_NoOp(t *testing.T) {
	a := &Agent{}
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)

	// Should not panic.
	tr.LogUsage(ts)
}

// ---------------------------------------------------------------------------
// toolDisplayNote tests
// ---------------------------------------------------------------------------

// TestToolDisplayNote_Modes verifies the tool display note text for each mode.
func TestToolDisplayNote_Modes(t *testing.T) {
	tests := []struct {
		mode     string
		contains string
	}{
		{"full", "tool_results=visible"},
		{"preview", "tool_results=preview"},
		{"off", "tool_results=hidden"},
		{"", "tool_results=hidden"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			got := toolDisplayNote(tt.mode)
			if !strings.Contains(got, tt.contains) {
				t.Errorf("toolDisplayNote(%q) = %q, want to contain %q", tt.mode, got, tt.contains)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RateLimitGate tests
// ---------------------------------------------------------------------------

// TestRateLimitGate_NotLimited verifies RateLimitGate returns nil when
// the endpoint is not rate-limited.
func TestRateLimitGate_NotLimited(t *testing.T) {
	a := &Agent{Endpoint: "api.anthropic.com"}
	tr := &APITransport{sharedTurnOps{agent: a}}

	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"hi"}, nil)
	ts.Trigger = "telegram"
	// Ensure session meta exists with no endpoint override.
	a.getSessionMeta(ts.SessionKey)

	err := tr.RateLimitGate(ts)
	if err != nil {
		t.Fatalf("RateLimitGate: unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// RunInference tests — aspects testable without a real API
// ---------------------------------------------------------------------------

// NOTE: RunInference is the largest method (~270 lines) and deeply coupled to
// provider.Send, tool execution, and streaming. Full integration testing would
// require a mock provider.Client that returns scripted responses. The tests
// below cover the paths that are feasible to exercise without a full mock:
// max loop handling, context cancellation, NO_RESPONSE handling, pause_turn,
// batched partial messages, and API errors.

type mockClient struct {
	sendFn func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error)
}

func (m *mockClient) SendMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	return m.sendFn(ctx, req)
}
func (m *mockClient) CountTokens(ctx context.Context, req *provider.MessageRequest) (int, error) {
	return 0, nil
}
func (m *mockClient) IsCachingAvailable() bool { return false }

// ctx is a test helper that returns a background context. Avoids repeating
// context.Background() in every test.
func ctx(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// newInferenceTS builds a minimal TurnState for RunInference tests.
func newInferenceTS(t *testing.T, a *Agent, client provider.Client) *TurnState {
	t.Helper()
	ts := NewTurnState(context.Background(), "bot/c100/1000000000", []string{"go"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.TurnModel = "anthropic/test-model"
	ts.TurnClient = client
	ts.Messages = []provider.Message{{Role: "user", Content: provider.TextContent("go")}}
	ts.NewMessages = []provider.Message{{Role: "user", Content: provider.TextContent("go")}}
	ts.System = []provider.SystemBlock{{Type: "text", Text: "sys"}}
	ts.StartedAt = time.Now()
	return ts
}

// newInferenceAgent builds a minimal Agent for RunInference tests.
func newInferenceAgent(t *testing.T, client provider.Client) *Agent {
	t.Helper()
	return &Agent{
		Client:          client,
		Sessions:        session.NewStore(t.TempDir()),
		Tools:           tools.NewRegistry(),
		Bootstrap:       workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:           "anthropic/test-model",
		MaxToolLoops:    10,
		MaxOutputTokens: 1024,
	}
}

// TestRunInference_MaxLoopReached verifies that when every API response is
// tool_use, RunInference stops after MaxToolLoops iterations and sets
// FinalText to the max-depth message.
func TestRunInference_MaxLoopReached(t *testing.T) {
	callCount := 0
	client := &mockClient{
		sendFn: func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			callCount++
			return &provider.MessageResponse{
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "working..."},
					{Type: "tool_use", ID: "toolu_" + strings.Repeat("x", callCount), Name: "bash", Input: json.RawMessage(`{"command":"echo hi"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
			}, nil
		},
	}

	a := newInferenceAgent(t, client)
	a.MaxToolLoops = 2
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := newInferenceTS(t, a, client)

	err := tr.RunInference(ts)
	if err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	if ts.FinalText != "Max tool call depth reached." {
		t.Errorf("FinalText = %q, want %q", ts.FinalText, "Max tool call depth reached.")
	}

	// CompletionChan should be closed.
	select {
	case <-ts.CompletionChan:
		// expected
	default:
		t.Error("CompletionChan should be closed")
	}
}

// TestRunInference_ContextCancelled verifies that RunInference returns the
// context error when the context is cancelled before the API call.
func TestRunInference_ContextCancelled(t *testing.T) {
	client := &mockClient{
		sendFn: func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			return nil, ctx.Err()
		},
	}

	a := newInferenceAgent(t, client)
	tr := &APITransport{sharedTurnOps{agent: a}}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	ts := NewTurnState(cancelCtx, "bot/c100/1000000000", []string{"hi"}, nil)
	ts.Meta = &TurnMetadata{}
	ts.SessionMeta = a.getSessionMeta(ts.SessionKey)
	ts.TurnModel = "anthropic/test-model"
	ts.TurnClient = client
	ts.Messages = []provider.Message{{Role: "user", Content: provider.TextContent("hi")}}
	ts.NewMessages = []provider.Message{{Role: "user", Content: provider.TextContent("hi")}}
	ts.System = []provider.SystemBlock{{Type: "text", Text: "sys"}}
	ts.StartedAt = time.Now()

	err := tr.RunInference(ts)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestRunInference_SimpleEndToEnd verifies the happy path where the API
// returns a text response with stop_reason="end_turn" on the first call.
func TestRunInference_SimpleEndToEnd(t *testing.T) {
	client := &mockClient{
		sendFn: func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			return &provider.MessageResponse{
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "text", Text: "Hello! How can I help?"},
				},
				StopReason: "end_turn",
				Usage: provider.Usage{
					InputTokens:  500,
					OutputTokens: 20,
				},
			}, nil
		},
	}

	a := newInferenceAgent(t, client)
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := newInferenceTS(t, a, client)

	err := tr.RunInference(ts)
	if err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	if ts.FinalText != "Hello! How can I help?" {
		t.Errorf("FinalText = %q, want %q", ts.FinalText, "Hello! How can I help?")
	}
	if ts.FinalUsage == nil {
		t.Fatal("FinalUsage should not be nil")
	}
	if ts.FinalUsage.InputTokens != 500 {
		t.Errorf("FinalUsage.InputTokens = %d, want 500", ts.FinalUsage.InputTokens)
	}

	// CompletionChan should be closed.
	select {
	case <-ts.CompletionChan:
		// expected
	default:
		t.Error("CompletionChan should be closed")
	}

	// Assistant message should be appended to Messages.
	found := false
	for _, m := range ts.Messages {
		if m.Role == "assistant" {
			found = true
			break
		}
	}
	if !found {
		t.Error("assistant message not found in Messages")
	}
}

// TestRunInference_NoResponseSentinelPassedThrough verifies that the
// [[NO_RESPONSE]] sentinel passes through RunInference — filtering is
// handled downstream by platform.IsSilent (bot SendText/SendTextToChat and Finalize).
func TestRunInference_NoResponseSentinelPassedThrough(t *testing.T) {
	client := &mockClient{
		sendFn: func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			return &provider.MessageResponse{
				Role:       "assistant",
				Content:    []provider.ContentBlock{{Type: "text", Text: "[[NO_RESPONSE]]"}},
				StopReason: "end_turn",
				Usage:      provider.Usage{InputTokens: 100, OutputTokens: 5},
			}, nil
		},
	}

	a := newInferenceAgent(t, client)
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := newInferenceTS(t, a, client)

	err := tr.RunInference(ts)
	if err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	if ts.FinalText != "[[NO_RESPONSE]]" {
		t.Errorf("FinalText = %q, want sentinel to pass through", ts.FinalText)
	}
}

// TestRunInference_PauseTurnContinues verifies that a "pause_turn" stop_reason
// causes the loop to continue to the next iteration.
func TestRunInference_PauseTurnContinues(t *testing.T) {
	callCount := 0
	client := &mockClient{
		sendFn: func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			callCount++
			if callCount == 1 {
				return &provider.MessageResponse{
					Role:       "assistant",
					Content:    []provider.ContentBlock{{Type: "text", Text: "thinking..."}},
					StopReason: "pause_turn",
					Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
				}, nil
			}
			return &provider.MessageResponse{
				Role:       "assistant",
				Content:    []provider.ContentBlock{{Type: "text", Text: "done!"}},
				StopReason: "end_turn",
				Usage:      provider.Usage{InputTokens: 200, OutputTokens: 20},
			}, nil
		},
	}

	a := newInferenceAgent(t, client)
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := newInferenceTS(t, a, client)

	err := tr.RunInference(ts)
	if err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	if callCount != 2 {
		t.Errorf("API called %d times, want 2 (pause_turn + end_turn)", callCount)
	}
	if ts.FinalText != "done!" {
		t.Errorf("FinalText = %q, want %q", ts.FinalText, "done!")
	}
}

// TestRunInference_BatchPartialMessages verifies that when
// BatchPartialAssistantMessages is true, intermediate text is accumulated
// and concatenated into FinalText.
func TestRunInference_BatchPartialMessages(t *testing.T) {
	callCount := 0
	client := &mockClient{
		sendFn: func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			callCount++
			if callCount == 1 {
				return &provider.MessageResponse{
					Role: "assistant",
					Content: []provider.ContentBlock{
						{Type: "text", Text: "Part 1."},
						{Type: "tool_use", ID: "toolu_abc", Name: "bash", Input: json.RawMessage(`{"command":"echo"}`)},
					},
					StopReason: "tool_use",
					Usage:      provider.Usage{InputTokens: 100, OutputTokens: 20},
				}, nil
			}
			return &provider.MessageResponse{
				Role:       "assistant",
				Content:    []provider.ContentBlock{{Type: "text", Text: "Part 2."}},
				StopReason: "end_turn",
				Usage:      provider.Usage{InputTokens: 200, OutputTokens: 30},
			}, nil
		},
	}

	a := newInferenceAgent(t, client)
	a.BatchPartialAssistantMessages = true
	a.BatchPartialJoiner = "\n---\n"
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := newInferenceTS(t, a, client)

	err := tr.RunInference(ts)
	if err != nil {
		t.Fatalf("RunInference: %v", err)
	}

	// FinalText should be "Part 1.\n---\nPart 2." (batched with joiner).
	if !strings.Contains(ts.FinalText, "Part 1.") || !strings.Contains(ts.FinalText, "Part 2.") {
		t.Errorf("FinalText = %q, want both parts concatenated", ts.FinalText)
	}
	if !strings.Contains(ts.FinalText, "\n---\n") {
		t.Errorf("FinalText = %q, want joiner between parts", ts.FinalText)
	}
}

// TestRunInference_DefaultMaxLoops verifies that MaxToolLoops defaults to 25
// when set to 0.
func TestRunInference_DefaultMaxLoops(t *testing.T) {
	callCount := 0
	client := &mockClient{
		sendFn: func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			callCount++
			return &provider.MessageResponse{
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "toolu_" + strings.Repeat("a", callCount), Name: "test"},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 50, OutputTokens: 10},
			}, nil
		},
	}

	a := newInferenceAgent(t, client)
	a.MaxToolLoops = 0 // should default to 25
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := newInferenceTS(t, a, client)

	_ = tr.RunInference(ts)

	if callCount != 25 {
		t.Errorf("API called %d times, want 25 (default MaxToolLoops)", callCount)
	}
	if ts.FinalText != "Max tool call depth reached." {
		t.Errorf("FinalText = %q, want max-depth message", ts.FinalText)
	}
}

// TestRunInference_MaxLoopToolResultInjection verifies that when the max loop
// is reached on a tool_use response, error tool_results are injected into
// NewMessages for all pending tool calls.
func TestRunInference_MaxLoopToolResultInjection(t *testing.T) {
	client := &mockClient{
		sendFn: func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			return &provider.MessageResponse{
				Role: "assistant",
				Content: []provider.ContentBlock{
					{Type: "tool_use", ID: "toolu_aaa", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
					{Type: "tool_use", ID: "toolu_bbb", Name: "bash", Input: json.RawMessage(`{"command":"pwd"}`)},
				},
				StopReason: "tool_use",
				Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
			}, nil
		},
	}

	a := newInferenceAgent(t, client)
	a.MaxToolLoops = 1
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := newInferenceTS(t, a, client)

	_ = tr.RunInference(ts)

	// NewMessages should contain the error tool_result message.
	found := false
	for _, msg := range ts.NewMessages {
		for _, block := range msg.Content {
			if block.Type == "tool_result" && block.IsError {
				found = true
				if !strings.Contains(block.Content, "max tool loop depth") {
					t.Errorf("tool_result content = %q, want max-depth error message", block.Content)
				}
			}
		}
	}
	if !found {
		t.Error("expected error tool_result in NewMessages for max-loop case")
	}
}

// TestRunInference_APIError verifies that RunInference handles API errors
// correctly, appending an error assistant message to NewMessages.
// Uses 400 (Bad Request) to avoid provider retry backoff delays.
func TestRunInference_APIError(t *testing.T) {
	client := &mockClient{
		sendFn: func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
			return nil, &provider.APIError{
				StatusCode: 400,
				Body:       "bad request",
			}
		},
	}

	a := newInferenceAgent(t, client)
	a.Endpoint = "api.anthropic.com"
	tr := &APITransport{sharedTurnOps{agent: a}}
	ts := newInferenceTS(t, a, client)

	err := tr.RunInference(ts)
	if err == nil {
		t.Fatal("expected error from RunInference")
	}

	// Error assistant message should be in NewMessages.
	foundErrMsg := false
	for _, msg := range ts.NewMessages {
		if msg.Role == "assistant" {
			text := provider.TextOf(msg.Content)
			if strings.Contains(text, "API error") {
				foundErrMsg = true
			}
		}
	}
	if !foundErrMsg {
		t.Error("expected error assistant message in NewMessages")
	}
}

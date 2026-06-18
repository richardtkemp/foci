package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/timeutil"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// renderDefaultMeta renders the default statusline template (which reproduces
// the historical [meta]/[state] header) for a bare agent with no stores, so the
// [state] line drops and only the [meta] line remains.
func renderDefaultMeta(now time.Time, model, platform, manaStr string, manaGood bool, sm *sessionMeta) string {
	a := &Agent{}
	return a.renderStatusline(context.Background(), DefaultStatuslineTemplate, statuslineInputs{
		now: now, model: model, platform: platform, manaStr: manaStr, manaGood: manaGood, sm: sm, agent: a,
	})
}

func TestBuildMetaPrefix(t *testing.T) {
	// Proves the default statusline template emits correct timestamp, gap, model,
	// platform, and cost fields for both the first message and subsequent messages
	// with prior-turn data (replacing the old buildMetaPrefix; #831).
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)

	// First message — no previous turn data
	sm := &sessionMeta{}
	prefix := renderDefaultMeta(now, "claude-haiku-4-5", "api", "", false, sm)
	if !strings.Contains(prefix, "time=2026-02-21T05:30:00Z") {
		t.Errorf("missing timestamp in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "gap=none") {
		t.Errorf("first message should have gap=none: %q", prefix)
	}
	if !strings.Contains(prefix, "model=claude-haiku-4-5") {
		t.Errorf("missing model in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "via=api") {
		t.Errorf("missing platform in prefix: %q", prefix)
	}
	if strings.Contains(prefix, "prev_cost") {
		t.Errorf("first message should not have prev_cost: %q", prefix)
	}

	// Subsequent message — has previous turn data
	sm.lastMessageTime = now.Add(-3*time.Hour - 12*time.Minute)
	sm.prevCost = 0.043
	sm.prevInput = 2400
	sm.prevOutput = 312
	sm.prevCacheRead = 18000
	sm.prevCacheWrite = 200

	prefix = renderDefaultMeta(now, "claude-haiku-4-5", "telegram", "", false, sm)
	if !strings.Contains(prefix, "gap=3h12m") {
		t.Errorf("missing gap in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "model=claude-haiku-4-5") {
		t.Errorf("missing model in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "via=telegram") {
		t.Errorf("missing platform in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "prev_cost=$0.043") {
		t.Errorf("missing prev_cost in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "prev_tokens=in:2400/out:312/cR:18000/cW:200") {
		t.Errorf("missing prev_tokens in prefix: %q", prefix)
	}
}

func TestMetadataInjectedInMessage(t *testing.T) {
	// Proves that every outgoing user message has a [meta] prefix containing at least
	// the model name, and that the original user text is preserved after the prefix.
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	ag.hmTest(context.Background(), "test/imeta/1000000000", "Hello")

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// The user message should have the meta prefix
	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if !strings.Contains(text, "[meta]") {
		t.Errorf("user message missing [meta] prefix: %q", text)
	}
	if !strings.Contains(text, "model=claude-haiku-4-5") {
		t.Errorf("user message missing model in [meta]: %q", text)
	}
	if !strings.Contains(text, "Hello") {
		t.Errorf("user message missing original text: %q", text)
	}
}

// TestMetadataUsesReceivedAtNotWallClock proves that when the platform boundary
// attaches a receipt time to ctx via WithReceivedAt, the [meta] header timestamp
// reflects that value instead of the wall clock. This is the regression guard
// for the bug where queued/steered Telegram messages were being stamped at
// injection time (when the turn was drained) rather than when the user sent
// the message — producing misleading gap calculations and time= values.
func TestMetadataUsesReceivedAtNotWallClock(t *testing.T) {
	var receivedReq *provider.MessageRequest
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	ag := &Agent{
		Client:    client,
		Sessions:  session.NewStore(t.TempDir()),
		Tools:     tools.NewRegistry(),
		Bootstrap: workspace.NewBootstrap(t.TempDir(), []string{}),
		Model:     "claude-haiku-4-5",
	}

	receivedAt := time.Date(2026, 4, 11, 13, 49, 0, 0, time.UTC)
	ctx := WithReceivedAt(context.Background(), receivedAt)
	if _, err := ag.hmTest(ctx, "test/ireceivedat/1000000000", "Dick's message"); err != nil {
		t.Fatalf("hmTest: %v", err)
	}
	if receivedReq == nil {
		t.Fatal("no request received")
	}

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	wantStamp := "time=" + timeutil.Format(receivedAt)
	if !strings.Contains(text, wantStamp) {
		t.Errorf("meta header missing receipt timestamp %q: %q", wantStamp, text)
	}
}

func TestBuildMetaPrefix_Mana(t *testing.T) {
	// Proves that mana status is omitted when empty, and shown with a red/green indicator
	// based on the goodMana flag, both on first and subsequent messages.
	now := time.Date(2026, 2, 21, 12, 0, 0, 0, time.UTC)

	// Without mana
	sm := &sessionMeta{}
	prefix := renderDefaultMeta(now, "claude-haiku-4-5", "api", "", false, sm)
	if strings.Contains(prefix, "mana=") {
		t.Errorf("should not contain mana when empty: %q", prefix)
	}

	// With mana, not good (first message) — red indicator
	prefix = renderDefaultMeta(now, "claude-haiku-4-5", "api", "75%", false, sm)
	if !strings.Contains(prefix, "mana=75% 🔴") {
		t.Errorf("should contain mana=75%% with red indicator: %q", prefix)
	}

	// With mana, good — green indicator
	prefix = renderDefaultMeta(now, "claude-haiku-4-5", "api", "75%", true, sm)
	if !strings.Contains(prefix, "mana=75% 🟢") {
		t.Errorf("should contain mana=75%% with green indicator: %q", prefix)
	}

	// With mana (subsequent message with cost data)
	sm.prevCost = 0.01
	sm.prevInput = 100
	prefix = renderDefaultMeta(now, "claude-haiku-4-5", "api", "50%", false, sm)
	if !strings.Contains(prefix, "mana=50% 🔴") {
		t.Errorf("should contain mana=50%% with red indicator in subsequent message: %q", prefix)
	}
}

func TestTriggerToPlatform(t *testing.T) {
	// Maps trigger labels to expected platform values for the [meta] header.
	// Register platform triggers that would normally be registered by platform init().
	RegisterPlatformTrigger("telegram")
	RegisterPlatformTrigger("discord")
	t.Cleanup(func() {
		platformTriggers.Delete("telegram")
		platformTriggers.Delete("discord")
	})

	tests := []struct {
		trigger  string
		platform string
	}{
		{"telegram", "telegram"},
		{"discord", "discord"},
		{"voice", "voice"},
		{"android", "android"},
		{"user", "api"},
		{"", "api"},
		{"keepalive", "cron"},
		{"wake", "cron"},
		{"cron", "cron"},
		{"restart", "cron"},
		{"first_run", "cron"},
		{"async_notify", "async"},
		{"tmux_watch", "tmux"},
		{"scheduled_wake", "cron"},
		{"proactive_warning", "cron"},
		{"session_end_memory", "cron"},
	}
	for _, tt := range tests {
		got := triggerToPlatform(tt.trigger)
		if got != tt.platform {
			t.Errorf("triggerToPlatform(%q) = %q, want %q", tt.trigger, got, tt.platform)
		}
	}
}

func TestMetaPlatformFromTrigger(t *testing.T) {
	// Verifies that platform= appears in the [meta] header with the correct
	// value derived from the context trigger.
	RegisterPlatformTrigger("telegram")
	t.Cleanup(func() { platformTriggers.Delete("telegram") })

	for _, tt := range []struct {
		trigger  string
		wantPlat string
	}{
		{"telegram", "telegram"},
		{"user", "api"},
		{"keepalive", "cron"},
	} {
		var receivedReq *provider.MessageRequest
		client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
			receivedReq = req
			return &provider.MessageResponse{
				ID: "msg_test", Type: "message", Role: "assistant",
				Content: provider.TextContent("ok"), StopReason: "end_turn",
				Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
			}
		})
		store := session.NewStore(t.TempDir())
		bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
		ag := &Agent{
			Client: client, Sessions: store, Tools: tools.NewRegistry(),
			Bootstrap: bootstrap, Model: "claude-haiku-4-5",
		}

		ctx := WithTrigger(context.Background(), tt.trigger)
		sk := "test/plat_" + tt.trigger + "/1000000000"
		ag.hmTest(ctx, sk, "Hello")

		lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
		text := provider.TextOf(lastMsg.Content)
		want := "via=" + tt.wantPlat
		if !strings.Contains(text, want) {
			t.Errorf("trigger=%q: expected %q in meta, got: %q", tt.trigger, want, text)
		}
	}
}

func TestRegisterPlatformTrigger(t *testing.T) {
	// Proves that RegisterPlatformTrigger causes both triggerToPlatform to
	// identity-map the trigger and isUserTrigger to return true.
	RegisterPlatformTrigger("test_plat")
	t.Cleanup(func() { platformTriggers.Delete("test_plat") })

	if got := triggerToPlatform("test_plat"); got != "test_plat" {
		t.Errorf("triggerToPlatform(\"test_plat\") = %q, want \"test_plat\"", got)
	}
	if !isUserTrigger("test_plat") {
		t.Error("isUserTrigger(\"test_plat\") = false, want true")
	}

	// Unregistered trigger should still fall through to defaults.
	if got := triggerToPlatform("unknown_sys"); got != "cron" {
		t.Errorf("triggerToPlatform(\"unknown_sys\") = %q, want \"cron\"", got)
	}
	if isUserTrigger("unknown_sys") {
		t.Error("isUserTrigger(\"unknown_sys\") = true, want false")
	}
}

func TestDuplicateMessages(t *testing.T) {
	// Proves that DuplicateMessages=true causes the user text to appear twice in the
	// outgoing message and in the saved session, while [meta] prefix appears only once.
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:            client,
		Sessions:          store,
		Tools:             tools.NewRegistry(),
		Bootstrap:         bootstrap,
		Model:             "claude-haiku-4-5",
		DuplicateMessages: true,
	}

	ag.hmTest(context.Background(), "test/idup/1000000000", "Do the thing")

	// The user message text should contain the instruction twice
	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("expected user text duplicated (2 occurrences), got %d in: %q", count, text)
	}

	// Meta prefix should only appear once
	if count := strings.Count(text, "[meta]"); count != 1 {
		t.Errorf("expected [meta] once, got %d", count)
	}

	// Saved session should also have the duplicated text (for cache coherence)
	saved, _ := store.Load("test/idup/1000000000")
	savedText := provider.TextOf(saved[0].Content)
	if count := strings.Count(savedText, "Do the thing"); count != 2 {
		t.Errorf("saved session should have duplicated text, got %d occurrences", count)
	}
}

func TestDuplicateMessagesDisabled(t *testing.T) {
	// Proves that DuplicateMessages=false (the default) sends each user text exactly once.
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:            client,
		Sessions:          store,
		Tools:             tools.NewRegistry(),
		Bootstrap:         bootstrap,
		Model:             "claude-haiku-4-5",
		DuplicateMessages: false,
	}

	ag.hmTest(context.Background(), "test/inodup/1000000000", "Do the thing")

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("expected user text once (no duplication), got %d in: %q", count, text)
	}
}

func TestDuplicateMessagesSkippedForWake(t *testing.T) {
	// Proves that duplication only applies to human-typed triggers (telegram, user, voice)
	// and is suppressed for automated/system triggers (wake, keepalive, proactive_warning, etc.).
	RegisterPlatformTrigger("telegram")
	t.Cleanup(func() { platformTriggers.Delete("telegram") })

	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:            client,
		Sessions:          store,
		Tools:             tools.NewRegistry(),
		Bootstrap:         bootstrap,
		Model:             "claude-haiku-4-5",
		DuplicateMessages: true,
	}

	// Wake trigger should NOT duplicate
	wakeCtx := WithTrigger(context.Background(), "wake")
	ag.hmTest(wakeCtx, "test/iwake/1000000000", "Do the thing")

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("wake trigger should not duplicate: expected 1 occurrence, got %d", count)
	}

	// Keepalive trigger should NOT duplicate
	kaCtx := WithTrigger(context.Background(), "keepalive")
	ag.hmTest(kaCtx, "test/ika/1000000000", "Check stuff")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Check stuff"); count != 1 {
		t.Errorf("keepalive trigger should not duplicate: expected 1 occurrence, got %d", count)
	}

	// User trigger SHOULD duplicate
	userCtx := WithTrigger(context.Background(), "user")
	ag.hmTest(userCtx, "test/iuser/1000000000", "Do the thing")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("user trigger should duplicate: expected 2 occurrences, got %d", count)
	}

	// Telegram trigger SHOULD duplicate (human-typed messages)
	tgCtx := WithTrigger(context.Background(), "telegram")
	ag.hmTest(tgCtx, "test/itg/1000000000", "Say something")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Say something"); count != 2 {
		t.Errorf("telegram trigger should duplicate: expected 2 occurrences, got %d", count)
	}

	// Voice trigger SHOULD duplicate (human-spoken messages)
	voiceCtx := WithTrigger(context.Background(), "voice")
	ag.hmTest(voiceCtx, "test/ivoice/1000000000", "Tell me")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Tell me"); count != 2 {
		t.Errorf("voice trigger should duplicate: expected 2 occurrences, got %d", count)
	}

	// System triggers should NOT duplicate
	for _, sysT := range []string{"proactive_warning", "async_notify", "session_notify", "scheduled_wake", "restart", "first_run"} {
		sysCtx := WithTrigger(context.Background(), sysT)
		ag.hmTest(sysCtx, "test/isys"+sysT+"/1000000000", "System msg")

		lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
		text = provider.TextOf(lastMsg.Content)
		if count := strings.Count(text, "System msg"); count != 1 {
			t.Errorf("%s trigger should not duplicate: expected 1 occurrence, got %d", sysT, count)
		}
	}
}

// Verifies that duplicate_messages is suppressed when extended thinking
// is enabled with effort above "low", since thinking already produces
// high-quality first responses.
func TestDuplicateMessagesSuppressedWithThinking(t *testing.T) {
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:            client,
		Sessions:          store,
		Tools:             tools.NewRegistry(),
		Bootstrap:         bootstrap,
		Model:             "claude-haiku-4-5",
		DuplicateMessages: true,
	}

	// With thinking+effort>low, duplication should be suppressed
	ag.SetSessionThinking("test/ithink/1000000000", "enabled")
	ag.SetSessionEffort("test/ithink/1000000000", "high")
	ag.hmTest(context.Background(), "test/ithink/1000000000", "Do the thing")

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("thinking+high effort should suppress duplication: expected 1, got %d", count)
	}

	// With effort=low, duplication should NOT be suppressed
	ag.SetSessionThinking("test/ilow/1000000000", "enabled")
	ag.SetSessionEffort("test/ilow/1000000000", "low")
	ag.hmTest(context.Background(), "test/ilow/1000000000", "Do the thing")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("thinking+low effort should allow duplication: expected 2, got %d", count)
	}

	// With thinking but default (empty) effort, duplication should still be suppressed
	ag.SetSessionThinking("test/idefaulteffort/1000000000", "adaptive")
	ag.hmTest(context.Background(), "test/idefaulteffort/1000000000", "Do the thing")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("thinking+default effort should suppress duplication: expected 1, got %d", count)
	}

	// With no thinking, duplication should NOT be suppressed
	ag.SetSessionEffort("test/inothink/1000000000", "high")
	ag.hmTest(context.Background(), "test/inothink/1000000000", "Do the thing")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("no thinking should allow duplication: expected 2, got %d", count)
	}
}

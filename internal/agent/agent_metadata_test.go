package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestBuildMetaPrefix(t *testing.T) {
	now := time.Date(2026, 2, 21, 5, 30, 0, 0, time.UTC)

	// First message — no previous turn data
	sm := &sessionMeta{}
	prefix := buildMetaPrefix(now, "claude-haiku-4-5", "api", "", false, sm)
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

	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "telegram", "", false, sm)
	if !strings.Contains(prefix, "gap=3h12m") {
		t.Errorf("missing gap in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "model=claude-haiku-4-5") {
		t.Errorf("missing model in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "via=telegram") {
		t.Errorf("missing platform in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "prev_cost=$0.0430") {
		t.Errorf("missing prev_cost in prefix: %q", prefix)
	}
	if !strings.Contains(prefix, "prev_tokens=in:2400/out:312/cR:18000/cW:200") {
		t.Errorf("missing prev_tokens in prefix: %q", prefix)
	}
}

func TestMetadataInjectedInMessage(t *testing.T) {
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

	ag.HandleMessage(context.Background(), "test/imeta/1000000000", "Hello")

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

func TestBuildMetaPrefix_Mana(t *testing.T) {
	now := time.Date(2026, 2, 21, 12, 0, 0, 0, time.UTC)

	// Without mana
	sm := &sessionMeta{}
	prefix := buildMetaPrefix(now, "claude-haiku-4-5", "api", "", false, sm)
	if strings.Contains(prefix, "mana=") {
		t.Errorf("should not contain mana when empty: %q", prefix)
	}

	// With mana, not good (first message) — red indicator
	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "api", "75%", false, sm)
	if !strings.Contains(prefix, "mana=75% 🔴") {
		t.Errorf("should contain mana=75%% with red indicator: %q", prefix)
	}

	// With mana, good — green indicator
	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "api", "75%", true, sm)
	if !strings.Contains(prefix, "mana=75% 🟢") {
		t.Errorf("should contain mana=75%% with green indicator: %q", prefix)
	}

	// With mana (subsequent message with cost data)
	sm.prevCost = 0.01
	sm.prevInput = 100
	prefix = buildMetaPrefix(now, "claude-haiku-4-5", "api", "50%", false, sm)
	if !strings.Contains(prefix, "mana=50% 🔴") {
		t.Errorf("should contain mana=50%% with red indicator in subsequent message: %q", prefix)
	}
}

func TestTriggerToPlatform(t *testing.T) {
	// Maps trigger labels to expected platform values for the [meta] header.
	tests := []struct {
		trigger  string
		platform string
	}{
		{"telegram", "telegram"},
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
		ag.HandleMessage(ctx, sk, "Hello")

		lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
		text := provider.TextOf(lastMsg.Content)
		want := "via=" + tt.wantPlat
		if !strings.Contains(text, want) {
			t.Errorf("trigger=%q: expected %q in meta, got: %q", tt.trigger, want, text)
		}
	}
}

func TestDuplicateMessages(t *testing.T) {
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

	ag.HandleMessage(context.Background(), "test/idup/1000000000", "Do the thing")

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

	ag.HandleMessage(context.Background(), "test/inodup/1000000000", "Do the thing")

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("expected user text once (no duplication), got %d in: %q", count, text)
	}
}

func TestDuplicateMessagesSkippedForWake(t *testing.T) {
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
	ag.HandleMessage(wakeCtx, "test/iwake/1000000000", "Do the thing")

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("wake trigger should not duplicate: expected 1 occurrence, got %d", count)
	}

	// Keepalive trigger should NOT duplicate
	kaCtx := WithTrigger(context.Background(), "keepalive")
	ag.HandleMessage(kaCtx, "test/ika/1000000000", "Check stuff")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Check stuff"); count != 1 {
		t.Errorf("keepalive trigger should not duplicate: expected 1 occurrence, got %d", count)
	}

	// User trigger SHOULD duplicate
	userCtx := WithTrigger(context.Background(), "user")
	ag.HandleMessage(userCtx, "test/iuser/1000000000", "Do the thing")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("user trigger should duplicate: expected 2 occurrences, got %d", count)
	}

	// Telegram trigger SHOULD duplicate (human-typed messages)
	tgCtx := WithTrigger(context.Background(), "telegram")
	ag.HandleMessage(tgCtx, "test/itg/1000000000", "Say something")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Say something"); count != 2 {
		t.Errorf("telegram trigger should duplicate: expected 2 occurrences, got %d", count)
	}

	// Voice trigger SHOULD duplicate (human-spoken messages)
	voiceCtx := WithTrigger(context.Background(), "voice")
	ag.HandleMessage(voiceCtx, "test/ivoice/1000000000", "Tell me")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Tell me"); count != 2 {
		t.Errorf("voice trigger should duplicate: expected 2 occurrences, got %d", count)
	}

	// System triggers should NOT duplicate
	for _, sysT := range []string{"proactive_warning", "async_notify", "session_notify", "scheduled_wake", "restart", "first_run"} {
		sysCtx := WithTrigger(context.Background(), sysT)
		ag.HandleMessage(sysCtx, "test/isys"+sysT+"/1000000000", "System msg")

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
		Thinking:          "enabled",
		Effort:            "high",
	}

	// With thinking+effort>low, duplication should be suppressed
	ag.HandleMessage(context.Background(), "test/ithink/1000000000", "Do the thing")

	lastMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	text := provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 1 {
		t.Errorf("thinking+high effort should suppress duplication: expected 1, got %d", count)
	}

	// With effort=low, duplication should NOT be suppressed
	ag.Effort = "low"
	ag.HandleMessage(context.Background(), "test/ilow/1000000000", "Do the thing")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("thinking+low effort should allow duplication: expected 2, got %d", count)
	}

	// With no thinking, duplication should NOT be suppressed
	ag.Thinking = ""
	ag.Effort = "high"
	ag.HandleMessage(context.Background(), "test/inothink/1000000000", "Do the thing")

	lastMsg = receivedReq.Messages[len(receivedReq.Messages)-1]
	text = provider.TextOf(lastMsg.Content)
	if count := strings.Count(text, "Do the thing"); count != 2 {
		t.Errorf("no thinking should allow duplication: expected 2, got %d", count)
	}
}


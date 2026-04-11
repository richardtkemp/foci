package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"foci/internal/agent/turnevent"
	"foci/internal/nudge"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestNudgeRegexPrependedToUserMessage(t *testing.T) {
	// Proves that regex nudges are prepended as ContentBlocks to the user message
	// rather than injected as standalone messages. Only one API call is made, and
	// the nudge blocks appear before the user text in the request.
	var callCount atomic.Int32
	var capturedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount.Add(1)
		capturedReq = req
		return &provider.MessageResponse{
			ID:         "msg_1",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Here is the answer to your question."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})

	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	// Create a nudge scheduler with a match rule that matches "debug".
	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{
				Text:    "match-debug-reminder",
				Trigger: nudge.Trigger{Type: "regex", Pattern: "debug"},
			},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 1)

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		Nudger:    sched,
	}

	ctx := context.Background()
	finalResp, err := ag.hmTest(ctx, "test/inudge-match/1000000000", "Please debug this issue")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Only one API call — no standalone nudge message loop.
	if got := callCount.Load(); got != 1 {
		t.Errorf("API call count = %d, want 1", got)
	}

	// Final response is the direct answer (no second call needed).
	if finalResp != "Here is the answer to your question." {
		t.Errorf("final response = %q, want direct answer", finalResp)
	}

	// Verify nudge block appears before user text in the user message.
	if capturedReq == nil {
		t.Fatal("no API request captured")
	}
	lastMsg := capturedReq.Messages[len(capturedReq.Messages)-1]
	if len(lastMsg.Content) < 2 {
		t.Fatalf("expected at least 2 content blocks, got %d", len(lastMsg.Content))
	}
	// First block: nudge
	if lastMsg.Content[0].Type != "text" || lastMsg.Content[0].Text == "" {
		t.Errorf("first block should be nudge text, got type=%q text=%q", lastMsg.Content[0].Type, lastMsg.Content[0].Text)
	}
	if !strings.Contains(lastMsg.Content[0].Text, "match-debug-reminder") {
		t.Errorf("nudge block should contain reminder text, got %q", lastMsg.Content[0].Text)
	}
	// Last block: user text (may have [meta] prefix from prepareUserMessage)
	lastBlock := lastMsg.Content[len(lastMsg.Content)-1]
	if lastBlock.Type != "text" || !strings.Contains(lastBlock.Text, "Please debug this issue") {
		t.Errorf("last block should contain user text, got type=%q text=%q", lastBlock.Type, lastBlock.Text)
	}
}

func TestNudgePreAnswerDoesNotDropReply(t *testing.T) {
	// Proves that when a pre_answer gate fires, the original answer is sent via ReplyFunc
	// before the gate injects its nudge, ensuring the initial response is never silently discarded.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			// First response: agent's answer (no tools)
			return &provider.MessageResponse{
				ID:         "msg_1",
				Type:       "message",
				Role:       "assistant",
				Content:    provider.TextContent("Original answer before gate."),
				StopReason: "end_turn",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		// Second response: after pre-answer gate injection
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Revised answer after gate."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 15},
		}
	})

	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{
				Text:    "pre-answer-check",
				Trigger: nudge.Trigger{Type: "pre_answer"},
			},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 1)

	ag := &Agent{
		Client:              client,
		Sessions:            store,
		Tools:               registry,
		Bootstrap:           bootstrap,
		Model:               "claude-haiku-4-5",
		Nudger:              sched,
		NudgePreAnswerGate:  true,
		NudgePreAnswerMinTools: 0, // fire even with 0 tool calls
	}

	var intermediateReplies []string
	recorder := turnevent.SinkFunc(func(_ context.Context, ev turnevent.Event) {
		if tb, ok := ev.(turnevent.TextBlock); ok && tb.Phase == turnevent.PhaseIntermediate {
			intermediateReplies = append(intermediateReplies, tb.Text)
		}
	})
	ctx := turnevent.WithSink(context.Background(), recorder)

	finalResp, err := ag.hmTest(ctx, "test/inudge-preanswer/1000000000", "What is the meaning of life?")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Original answer delivered as intermediate reply.
	if len(intermediateReplies) != 1 {
		t.Fatalf("expected 1 intermediate reply, got %d: %v", len(intermediateReplies), intermediateReplies)
	}
	if intermediateReplies[0] != "Original answer before gate." {
		t.Errorf("intermediate reply = %q, want original answer", intermediateReplies[0])
	}

	// Final return is the revised answer.
	if finalResp != "Revised answer after gate." {
		t.Errorf("final response = %q, want revised answer", finalResp)
	}
}

func TestNudgeRegexBatchMode(t *testing.T) {
	// Proves that with BatchPartialAssistantMessages enabled, regex nudges are
	// prepended to the user message (not standalone messages). Only one API call,
	// single direct response returned.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		callCount.Add(1)
		return &provider.MessageResponse{
			ID:         "msg_1",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Batched original."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
		}
	})

	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	rs := &nudge.RuleSet{
		Rules: []nudge.Rule{
			{
				Text:    "match-reminder",
				Trigger: nudge.Trigger{Type: "regex", Pattern: "debug"},
			},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 1)

	ag := &Agent{
		Client:                        client,
		Sessions:                      store,
		Tools:                         registry,
		Bootstrap:                     bootstrap,
		Model:                         "claude-haiku-4-5",
		Nudger:                        sched,
		BatchPartialAssistantMessages: true,
		BatchPartialJoiner:            "\n\n",
	}

	ctx := context.Background()
	finalResp, err := ag.hmTest(ctx, "test/inudge-batch/1000000000", "debug this")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// Only one API call — nudge prepended, no extra loop.
	if got := callCount.Load(); got != 1 {
		t.Errorf("API call count = %d, want 1", got)
	}

	// Final response is the direct answer.
	if finalResp != "Batched original." {
		t.Errorf("final response = %q, want %q", finalResp, "Batched original.")
	}
}

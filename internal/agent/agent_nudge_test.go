package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"foci/internal/nudge"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// TestNudgeMatchDoesNotDropReply verifies that when a match nudge fires on a
// non-tool-use response, the original reply text is delivered as an intermediate
// message before the nudge loop continues. Without the fix, the original reply
// would be silently discarded.
func TestNudgeMatchDoesNotDropReply(t *testing.T) {
	// Proves that when a match nudge fires on a non-tool-use response, the original
	// reply is delivered via ReplyFunc before the nudge loop continues, so no text is lost.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			// First response: the agent's actual answer (end_turn, no tools)
			return &provider.MessageResponse{
				ID:         "msg_1",
				Type:       "message",
				Role:       "assistant",
				Content:    provider.TextContent("Here is the answer to your question."),
				StopReason: "end_turn",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		// Second response: after match nudge injection
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Acknowledged the nudge."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 30, OutputTokens: 15},
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
				Trigger: nudge.Trigger{Type: "match", Pattern: "debug"},
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

	// Track intermediate replies.
	var intermediateReplies []string
	cb := &TurnCallbacks{
		ReplyFunc: func(text string) {
			intermediateReplies = append(intermediateReplies, text)
		},
	}
	ctx := WithTurnCallbacks(context.Background(), cb)

	finalResp, err := ag.HandleMessage(ctx, "test/inudge-match/1000000000", "Please debug this issue")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// The original answer must have been sent as an intermediate reply.
	if len(intermediateReplies) != 1 {
		t.Fatalf("expected 1 intermediate reply, got %d: %v", len(intermediateReplies), intermediateReplies)
	}
	if intermediateReplies[0] != "Here is the answer to your question." {
		t.Errorf("intermediate reply = %q, want original answer", intermediateReplies[0])
	}

	// The final return value is the nudge-response text.
	if finalResp != "Acknowledged the nudge." {
		t.Errorf("final response = %q, want nudge acknowledgement", finalResp)
	}

	// Two API calls should have been made.
	if got := callCount.Load(); got != 2 {
		t.Errorf("API call count = %d, want 2", got)
	}
}

// TestNudgePreAnswerDoesNotDropReply verifies that when a pre-answer gate fires
// on a non-tool-use response, the original reply text is delivered as an
// intermediate message before the loop continues.
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
	cb := &TurnCallbacks{
		ReplyFunc: func(text string) {
			intermediateReplies = append(intermediateReplies, text)
		},
	}
	ctx := WithTurnCallbacks(context.Background(), cb)

	finalResp, err := ag.HandleMessage(ctx, "test/inudge-preanswer/1000000000", "What is the meaning of life?")
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

// TestNudgeMatchBatchMode verifies that when BatchPartialAssistantMessages is
// enabled, a match-nudge response accumulates text rather than sending via
// ReplyFunc, and the final return contains both the original and nudge text.
func TestNudgeMatchBatchMode(t *testing.T) {
	// Proves that with BatchPartialAssistantMessages enabled, a match-nudge turn
	// accumulates all text into the final return value rather than calling ReplyFunc.
	var callCount atomic.Int32

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		n := callCount.Add(1)
		if n == 1 {
			return &provider.MessageResponse{
				ID:         "msg_1",
				Type:       "message",
				Role:       "assistant",
				Content:    provider.TextContent("Batched original."),
				StopReason: "end_turn",
				Usage:      provider.Usage{InputTokens: 20, OutputTokens: 10},
			}
		}
		return &provider.MessageResponse{
			ID:         "msg_2",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Batched nudge reply."),
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
				Text:    "match-reminder",
				Trigger: nudge.Trigger{Type: "match", Pattern: "debug"},
			},
		},
	}
	sched := nudge.NewScheduler(rs, 5, 1)

	ag := &Agent{
		Client:                      client,
		Sessions:                    store,
		Tools:                       registry,
		Bootstrap:                   bootstrap,
		Model:                       "claude-haiku-4-5",
		Nudger:                      sched,
		BatchPartialAssistantMessages: true,
		BatchPartialJoiner:          "\n\n",
	}

	// In batch mode, ReplyFunc should NOT be called.
	replyCalled := false
	cb := &TurnCallbacks{
		ReplyFunc: func(text string) {
			replyCalled = true
		},
	}
	ctx := WithTurnCallbacks(context.Background(), cb)

	finalResp, err := ag.HandleMessage(ctx, "test/inudge-batch/1000000000", "debug this")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if replyCalled {
		t.Error("ReplyFunc should not be called in batch mode")
	}

	// Final response should contain both texts joined.
	want := "Batched original.\n\nBatched nudge reply."
	if finalResp != want {
		t.Errorf("final response = %q, want %q", finalResp, want)
	}
}

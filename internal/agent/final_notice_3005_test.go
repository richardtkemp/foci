package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"foci/internal/provider"
	"foci/internal/turnevent"
)

// TestHandleMessage_MaxDepthIsFinalNotice (#3005): a turn that hits the tool
// call depth limit ends with "Max tool call depth reached.", which is foci's
// notice, not the agent's reply: its TurnComplete carries FinalNotice so the
// app shows it as a system message. The text stays the turn's FinalText for
// buffering callers (spawn, sync HTTP).
func TestHandleMessage_MaxDepthIsFinalNotice(t *testing.T) {
	t.Parallel()
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			Role: "assistant",
			Content: []provider.ContentBlock{
				{Type: "tool_use", ID: "toolu_x", Name: "bash", Input: json.RawMessage(`{"command":"echo hi"}`)},
			},
			StopReason: "tool_use",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	a := newInferenceAgent(t, client)
	a.MaxToolLoops = 1

	var mu sync.Mutex
	var got []turnevent.TurnComplete
	ctx := turnevent.WithSink(context.Background(), fnSink(func(_ context.Context, ev turnevent.Event) {
		if tc, ok := ev.(turnevent.TurnComplete); ok {
			mu.Lock()
			got = append(got, tc)
			mu.Unlock()
		}
	}))
	if _, err := a.hmTest(ctx, "test/imaxdepth", "loop forever"); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("TurnComplete events = %d, want 1", len(got))
	}
	if got[0].FinalText != "Max tool call depth reached." || !got[0].FinalNotice {
		t.Errorf("TurnComplete = {FinalText:%q FinalNotice:%v}, want the max-depth text marked as a notice", got[0].FinalText, got[0].FinalNotice)
	}
}

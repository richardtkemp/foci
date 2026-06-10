package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
)

func TestStreamMessageIdleTimeout(t *testing.T) {
	// Proves a stream that goes silent past the idle window is aborted with a
	// clear idle-timeout error, rather than hanging or being truncated by a
	// total wall-clock cap. The server sends message_start then stalls. (P2-6.)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server doesn't support flushing")
		}
		fmt.Fprintf(w, "%s\n\n", `event: message_start
data: {"type":"message_start","message":{"id":"msg_x","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`)
		flusher.Flush()
		<-r.Context().Done() // stall until the client aborts the request
	}))
	defer server.Close()

	// Short idle window so the stall trips the watchdog quickly.
	client := NewClient(StaticToken("test-key"), 300*time.Millisecond)
	client.SetBaseURL(server.URL)

	done := make(chan error, 1)
	go func() {
		_, err := client.StreamMessage(context.Background(), &MessageRequest{
			Model: "claude-haiku-4-5", MaxTokens: 256,
			Messages: []Message{{Role: "user", Content: TextContent("hi")}},
		}, &provider.StreamHandler{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected idle-timeout error, got nil")
		}
		if !strings.Contains(err.Error(), "idle timeout") {
			t.Errorf("error = %v, want stream idle timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StreamMessage did not return after the idle window — watchdog did not fire")
	}
}

func TestStreamMessageSSESuccess(t *testing.T) {
	// Proves that StreamMessage correctly reassembles text deltas from a sequence of SSE events, invokes OnTextDelta for each delta, and produces a complete MessageResponse with the right ID, stop reason, and concatenated text.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server doesn't support flushing")
		}

		events := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
			`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		}

		for _, event := range events {
			fmt.Fprintf(w, "%s\n\n", event)
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := NewClient(StaticToken("test-key"), 120*time.Second)
	client.SetBaseURL(server.URL)

	var textDeltas []string
	handler := &provider.StreamHandler{
		OnTextDelta: func(delta string) {
			textDeltas = append(textDeltas, delta)
		},
	}

	resp, err := client.StreamMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	}, handler)

	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}

	if resp.ID != "msg_test" {
		t.Errorf("resp.ID = %q, want msg_test", resp.ID)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}

	fullText := TextOf(resp.Content)
	if fullText != "Hello world" {
		t.Errorf("response text = %q, want 'Hello world'", fullText)
	}

	if len(textDeltas) != 2 {
		t.Errorf("text deltas = %d, want 2", len(textDeltas))
	}
	if len(textDeltas) >= 2 {
		if textDeltas[0] != "Hello" {
			t.Errorf("delta[0] = %q, want 'Hello'", textDeltas[0])
		}
		if textDeltas[1] != " world" {
			t.Errorf("delta[1] = %q, want ' world'", textDeltas[1])
		}
	}
}

func TestStreamMessageSSEWithThinking(t *testing.T) {
	// Proves that a response containing both a thinking block and a text block is parsed correctly: the thinking content is accumulated via OnThinkingDelta, the text via OnTextDelta, and both content blocks appear in the final response.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		events := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_think","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me reason"}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}`,
			`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
			`event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Result"}}`,
			`event: content_block_stop
data: {"type":"content_block_stop","index":1}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		}

		for _, event := range events {
			fmt.Fprintf(w, "%s\n\n", event)
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := NewClient(StaticToken("test-key"), 120*time.Second)
	client.SetBaseURL(server.URL)

	var thinkingDeltas []string
	var textDeltas []string
	handler := &provider.StreamHandler{
		OnTextDelta: func(delta string) {
			textDeltas = append(textDeltas, delta)
		},
		OnThinkingDelta: func(delta string) {
			thinkingDeltas = append(thinkingDeltas, delta)
		},
	}

	resp, err := client.StreamMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("think about this")}},
	}, handler)

	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}

	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" {
		t.Errorf("content[0].type = %q, want thinking", resp.Content[0].Type)
	}
	if resp.Content[0].Thinking != "Let me reason" {
		t.Errorf("thinking = %q", resp.Content[0].Thinking)
	}

	if len(thinkingDeltas) != 1 || thinkingDeltas[0] != "Let me reason" {
		t.Errorf("thinking deltas = %v, want [Let me reason]", thinkingDeltas)
	}
	if len(textDeltas) != 1 || textDeltas[0] != "Result" {
		t.Errorf("text deltas = %v, want [Result]", textDeltas)
	}
}


func TestStreamMessageNilHandler(t *testing.T) {
	// Proves that StreamMessage completes successfully when passed a nil handler, allowing callers that only want the final response to omit the handler.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		events := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_nil","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
			`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		}
		for _, event := range events {
			fmt.Fprintf(w, "%s\n\n", event)
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := NewClient(StaticToken("test-key"), 120*time.Second)
	client.SetBaseURL(server.URL)

	resp, err := client.StreamMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	}, nil)

	if err != nil {
		t.Fatalf("StreamMessage with nil handler: %v", err)
	}
	if resp.ID != "msg_nil" {
		t.Errorf("resp.ID = %q", resp.ID)
	}
}

func TestStreamingClientInterface(t *testing.T) {
	// Proves that *Client satisfies the provider.StreamingClient interface at runtime (compile-time assertion exists in stream.go; this documents the contract explicitly in the test suite).
	var _ provider.StreamingClient = (*Client)(nil)
}

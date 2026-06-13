package compaction

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"foci/internal/provider"
)

// nonStreamingClient wraps a provider.Client so it does not satisfy
// provider.StreamingClient. This ensures tests exercise the SendMessage
// fallback path rather than attempting (and failing) to stream via the
// SDK-only transport.
type nonStreamingClient struct{ provider.Client }

func noStream(c provider.Client) provider.Client { return nonStreamingClient{c} }

// retryable mirrors the provider.retryableClient interface so
// nonStreamingClient can forward retry methods through the wrapper,
// allowing the provider layer's type assertion to succeed.
type retryable interface {
	OnRetrySuccess()
	WaitForRecovery() <-chan struct{}
	RetryBaseDelay() time.Duration
	OverloadBaseDelay() time.Duration
	OverloadMaxDuration() time.Duration
	ServerErrorMaxDuration() time.Duration
}

func (n nonStreamingClient) OnRetrySuccess() { n.Client.(retryable).OnRetrySuccess() }
func (n nonStreamingClient) WaitForRecovery() <-chan struct{} {
	return n.Client.(retryable).WaitForRecovery()
}
func (n nonStreamingClient) RetryBaseDelay() time.Duration {
	return n.Client.(retryable).RetryBaseDelay()
}
func (n nonStreamingClient) OverloadBaseDelay() time.Duration {
	return n.Client.(retryable).OverloadBaseDelay()
}
func (n nonStreamingClient) OverloadMaxDuration() time.Duration {
	return n.Client.(retryable).OverloadMaxDuration()
}
func (n nonStreamingClient) ServerErrorMaxDuration() time.Duration {
	return n.Client.(retryable).ServerErrorMaxDuration()
}

// mockCompactionServer returns a test API server for compaction tests.
func mockCompactionServer(summaryText string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(provider.MessageResponse{
			ID:         "msg_compact",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent(summaryText),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50},
		})
	}))
}

// toolUseMsg builds an assistant message with one or more tool_use blocks.
func toolUseMsg(ids ...string) provider.Message {
	var blocks []provider.ContentBlock
	for _, id := range ids {
		blocks = append(blocks, provider.ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  "test_tool",
			Input: json.RawMessage(`{}`),
		})
	}
	return provider.Message{Role: "assistant", Content: blocks}
}

// toolResultMsg builds a user message with tool_result blocks matching the given IDs.
func toolResultMsg(ids ...string) provider.Message {
	var blocks []provider.ContentBlock
	for _, id := range ids {
		blocks = append(blocks, provider.ToolResultBlock(id, "ok", false))
	}
	return provider.Message{Role: "user", Content: blocks}
}

// mockStreamingCompactionServer returns an SSE-streaming test server for compaction.
func mockStreamingCompactionServer(summaryText string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "flushing not supported", http.StatusInternalServerError)
			return
		}

		events := []string{
			`event: message_start
data: {"type":"message_start","message":{"id":"msg_compact_stream","type":"message","role":"assistant","content":[],"model":"claude-haiku-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
			`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			fmt.Sprintf(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"%s"}}`, summaryText),
			`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":50}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		}

		for _, event := range events {
			fmt.Fprintf(w, "%s\n\n", event)
			flusher.Flush()
		}
	}))
}

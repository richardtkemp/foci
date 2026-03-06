package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"foci/internal/anthropic"
	"foci/internal/provider"
)

// mockServer returns a test HTTP server that returns canned Anthropic responses.
// responseFunc is called for each request and should return the MessageResponse.
// Handles both non-streaming (JSON) and streaming (SSE) requests automatically.
func mockServer(responseFunc func(req *provider.MessageRequest) *provider.MessageResponse) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read raw body to check for stream flag before decoding.
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)

		var req provider.MessageRequest
		json.Unmarshal(raw, &req)

		resp := responseFunc(&req)

		// Check if this is a streaming request.
		var envelope struct{ Stream bool }
		json.Unmarshal(raw, &envelope)
		if envelope.Stream {
			serveSSE(w, resp)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

// serveSSE writes a MessageResponse as an SSE event stream.
func serveSSE(w http.ResponseWriter, resp *provider.MessageResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "flushing not supported", http.StatusInternalServerError)
		return
	}

	text := provider.TextOf(resp.Content)

	fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": resp.ID, "type": "message", "role": "assistant",
			"content": []any{}, "model": "claude-haiku-4-5",
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens": resp.Usage.InputTokens, "output_tokens": 0,
				"cache_creation_input_tokens": resp.Usage.CacheCreationInputTokens,
				"cache_read_input_tokens":     resp.Usage.CacheReadInputTokens,
			},
		},
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "content_block_stop", "index": 0,
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{"stop_reason": resp.StopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": resp.Usage.OutputTokens},
	}))
	flusher.Flush()

	fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", mustJSON(map[string]any{
		"type": "message_stop",
	}))
	flusher.Flush()
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newTestClientWithBase(baseURL, apiKey string) *anthropic.Client {
	c := anthropic.NewClientWithBase(baseURL, apiKey)
	c.SetUseSDK(true)
	return c
}

package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"foci/internal/provider"

	sdk "github.com/anthropics/anthropic-sdk-go"
)

// Compile-time check: Client implements provider.StreamingClient.
var _ provider.StreamingClient = (*Client)(nil)

// StreamMessage sends a streaming message request and returns the accumulated response.
// Delta callbacks in handler are invoked as content arrives. The full response is
// returned once the stream completes.
//
// Retry logic is handled by the provider layer. Pre-stream errors (before any deltas)
// are retryable. Mid-stream errors (after deltas have been emitted) are not retryable.
//
// Requires useSDK=true. Returns an error if called with useSDK=false.
func (c *Client) StreamMessage(ctx context.Context, req *MessageRequest, handler *provider.StreamHandler) (*MessageResponse, error) {
	if !c.useSDK {
		return nil, fmt.Errorf("streaming requires SDK transport (use_sdk = true)")
	}

	stripUnsupportedParams(req)
	return c.streamOnce(ctx, req, handler)
}

// streamOnce performs a single streaming request. Returns the accumulated response.
// Errors that occur before any deltas are emitted are retryable (pre-stream).
// Errors after deltas have been emitted are returned as-is (mid-stream, not retryable).
func (c *Client) streamOnce(ctx context.Context, req *MessageRequest, handler *provider.StreamHandler) (*MessageResponse, error) {
	token, err := c.resolveToken()
	if err != nil {
		return nil, fmt.Errorf("resolve token: %w", err)
	}

	params := buildSDKParams(req)
	wireReq, _ := json.Marshal(params)
	sc := c.ensureSDKClient()

	slog.Debug("anthropic: stream_call_start", "model", req.Model)

	stream := sc.Messages.NewStreaming(ctx, params, sdkRequestOptions(token, req.Speed)...)

	var msg sdk.Message
	deltasEmitted := false

	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			slog.Warn("anthropic: stream accumulate error", "error", err)
		}

		// Fire delta callbacks.
		if event.Type == "content_block_delta" {
			switch event.Delta.Type {
			case "text_delta":
				if handler != nil && handler.OnTextDelta != nil && event.Delta.Text != "" {
					deltasEmitted = true
					handler.OnTextDelta(event.Delta.Text)
				}
			case "thinking_delta":
				if handler != nil && handler.OnThinkingDelta != nil && event.Delta.Thinking != "" {
					deltasEmitted = true
					handler.OnThinkingDelta(event.Delta.Thinking)
				}
			}
		}
	}

	if err := stream.Err(); err != nil {
		sdkErr := classifySDKError(err)
		if deltasEmitted {
			// Mid-stream error: deltas already emitted, can't retry.
			// Wrap so callers know it's a stream error.
			return nil, fmt.Errorf("mid-stream error (deltas already emitted): %w", sdkErr)
		}
		return nil, sdkErr
	}

	resp := responseFromSDK(&msg)
	resp.WireRequest = wireReq
	return resp, nil
}

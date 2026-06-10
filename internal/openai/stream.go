package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"foci/internal/log"
	"foci/internal/provider"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
)

// Compile-time check: Client implements provider.StreamingClient.
var _ provider.StreamingClient = (*Client)(nil)

// StreamMessage sends a streaming message request and returns the accumulated response.
// Delta callbacks in handler are invoked as content arrives. The full response is
// returned once the stream completes.
//
// Retry logic is handled by the provider layer. Pre-stream errors (before any deltas)
// are retryable. Mid-stream errors (after deltas have been emitted) are not retryable.
func (c *Client) StreamMessage(ctx context.Context, req *provider.MessageRequest, handler *provider.StreamHandler) (*provider.MessageResponse, error) {
	return c.streamOnce(ctx, req, handler)
}

// streamOnce performs a single streaming request. Returns the accumulated response.
// Errors that occur before any deltas are emitted are retryable (pre-stream).
// Errors after deltas have been emitted are returned as-is (mid-stream, not retryable).
func (c *Client) streamOnce(ctx context.Context, req *provider.MessageRequest, handler *provider.StreamHandler) (*provider.MessageResponse, error) {
	params := buildParams(req)
	params.StreamOptions.IncludeUsage = param.NewOpt(true)

	wireReq, _ := json.Marshal(params)

	log.Debugf("openai", "stream_call_start: model=%s", req.Model)

	// Bound the gap between chunks with an idle watchdog instead of a total
	// wall-clock cap, so a long-but-progressing stream is not truncated (P2-6).
	sctx, wd := provider.NewStreamIdleWatchdog(ctx, c.timeout)
	defer wd.Stop()

	stream := c.client.Chat.Completions.NewStreaming(sctx, params)
	defer func() { _ = stream.Close() }()

	var acc openai.ChatCompletionAccumulator
	var reasoning strings.Builder
	deltasEmitted := false

	// The SDK accumulator sums usage fields across all chunks (+=). Per the
	// OpenAI spec, only the final chunk carries usage — but we've observed
	// providers (OpenRouter/GLM-5) where intermediate chunks also carry small
	// non-zero cached_tokens values. The accumulator inflates cache_read by
	// summing those, which causes our cache-bust detector to see phantom
	// drops on the next turn. We're not 100% certain this is the root cause
	// (it could also be provider-side token-counting variance), but capturing
	// the last chunk's usage directly is strictly more correct regardless.
	var lastUsage openai.CompletionUsage
	var hasUsage bool

	for stream.Next() {
		wd.Reset()
		chunk := stream.Current()
		acc.AddChunk(chunk)

		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			lastUsage = chunk.Usage
			hasUsage = true
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta

		// Text deltas.
		if delta.Content != "" {
			deltasEmitted = true
			if handler != nil && handler.OnTextDelta != nil {
				handler.OnTextDelta(delta.Content)
			}
		}

		// Reasoning deltas (OpenRouter reasoning_content extra field).
		// ExtraFields have invalid status in the SDK (not modeled by struct fields),
		// so check Raw() instead of Valid().
		if f, ok := delta.JSON.ExtraFields["reasoning_content"]; ok && f.Raw() != "" && f.Raw() != "null" {
			var text string
			if json.Unmarshal([]byte(f.Raw()), &text) == nil && text != "" {
				deltasEmitted = true
				reasoning.WriteString(text)
				if handler != nil && handler.OnThinkingDelta != nil {
					handler.OnThinkingDelta(text)
				}
			}
		}
	}

	if err := stream.Err(); err != nil {
		// An idle-watchdog cancellation surfaces as a context error; report it
		// as a clear stream-idle timeout rather than a generic cancel. (P2-6.)
		if wd.Fired() {
			idleErr := fmt.Errorf("stream idle timeout: no data for %s", c.timeout)
			if deltasEmitted {
				return nil, fmt.Errorf("mid-stream error (deltas already emitted): %w", idleErr)
			}
			return nil, idleErr
		}
		streamErr := classifyStreamError(err)
		if deltasEmitted {
			return nil, fmt.Errorf("mid-stream error (deltas already emitted): %w", streamErr)
		}
		return nil, streamErr
	}

	// Override accumulated usage with the final chunk's authoritative value.
	if hasUsage {
		acc.ChatCompletion.Usage = lastUsage
	}

	result, err := responseFromOpenAI(&acc.ChatCompletion, req.Model)
	if err != nil {
		return nil, err
	}

	// Prepend thinking block if reasoning was accumulated.
	if reasoning.Len() > 0 {
		rawJSON, _ := json.Marshal(reasoning.String())
		thinkingBlock := provider.ContentBlock{
			Type:         "thinking",
			Thinking:     reasoning.String(),
			ReasoningRaw: rawJSON,
		}
		result.Content = append([]provider.ContentBlock{thinkingBlock}, result.Content...)
	}

	result.WireRequest = wireReq
	result.KeySuffix = log.FormatKeySuffix(c.apiKey)
	return result, nil
}

// classifyStreamError maps streaming errors to provider.APIError where possible.
func classifyStreamError(err error) error {
	// Pre-stream HTTP errors from the SDK.
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return classifyError(err)
	}

	// Mid-stream SSE error events.
	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		var parsed struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(streamErr.Event.Data, &parsed) == nil && parsed.Error.Message != "" {
			return &provider.APIError{
				StatusCode: 500,
				Body:       fmt.Sprintf("%s: %s", parsed.Error.Type, parsed.Error.Message),
			}
		}
		return &provider.APIError{
			StatusCode: 500,
			Body:       streamErr.Message,
		}
	}

	return fmt.Errorf("openai stream: %w", err)
}

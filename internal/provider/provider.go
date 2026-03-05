package provider

import "context"

// Client is the interface that all LLM providers implement.
type Client interface {
	SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error)
	CountTokens(ctx context.Context, req *MessageRequest) (int, error)
}

// StreamHandler receives delta events during a streaming response.
type StreamHandler struct {
	OnTextDelta     func(delta string)
	OnThinkingDelta func(delta string)
}

// StreamingClient extends Client with streaming support.
// Providers that support streaming implement this interface.
type StreamingClient interface {
	StreamMessage(ctx context.Context, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error)
}

// Send sends a message request using streaming only when a handler is provided.
// Pass a non-nil handler to enable streaming; pass nil to use non-streaming SendMessage.
func Send(ctx context.Context, client Client, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
	// Only use streaming if handler is explicitly provided
	if handler != nil {
		if sc, ok := client.(StreamingClient); ok {
			return sc.StreamMessage(ctx, req, handler)
		}
	}
	return client.SendMessage(ctx, req)
}

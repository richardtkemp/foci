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

// Send sends a message request, preferring streaming when the client supports it.
// The handler is optional — pass nil when deltas are not needed.
func Send(ctx context.Context, client Client, req *MessageRequest, handler *StreamHandler) (*MessageResponse, error) {
	if sc, ok := client.(StreamingClient); ok {
		if handler == nil {
			handler = &StreamHandler{}
		}
		return sc.StreamMessage(ctx, req, handler)
	}
	return client.SendMessage(ctx, req)
}

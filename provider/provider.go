package provider

import "context"

// Client is the interface that all LLM providers implement.
type Client interface {
	SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error)
	CountTokens(ctx context.Context, req *MessageRequest) (int, error)
}

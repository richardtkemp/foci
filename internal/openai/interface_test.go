package openai

import "foci/internal/provider"

// Compile-time check: *Client implements provider.Client and provider.StreamingClient.
var _ provider.Client = (*Client)(nil)
var _ provider.StreamingClient = (*Client)(nil)

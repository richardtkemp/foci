package openai

import "foci/internal/provider"

// Compile-time check: *Client implements provider.Client.
var _ provider.Client = (*Client)(nil)

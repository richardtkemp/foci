package openai

import "foci/provider"

// Compile-time check: *Client implements provider.Client.
var _ provider.Client = (*Client)(nil)

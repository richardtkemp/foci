package compaction

import (
	"time"

	"foci/internal/anthropic"
)

// newTestAnthropicClient creates an Anthropic client pointed at a test HTTP server.
// Disables SDK transport and uses fast retries.
func newTestAnthropicClient(baseURL, key string) *anthropic.Client {
	c := anthropic.NewClient(anthropic.StaticToken(key), 120*time.Second)
	c.SetBaseURL(baseURL)
	c.SetUseSDK(false)
	return c
}

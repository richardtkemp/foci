package tools

import (
	"time"

	"foci/internal/anthropic"
)

// newTestAnthropicClient creates an Anthropic client pointed at a test HTTP server.
func newTestAnthropicClient(baseURL, key string) *anthropic.Client {
	c := anthropic.NewClient(anthropic.StaticToken(key), 120*time.Second)
	c.SetBaseURL(baseURL)
	return c
}

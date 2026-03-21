package tools

import (
	"foci/internal/openai"
)

// newTestOpenAIClient creates an OpenAI client pointed at a test HTTP server.
func newTestOpenAIClient(baseURL, key string) *openai.Client {
	return openai.NewClient(key, openai.WithBaseURL(baseURL))
}

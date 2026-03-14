package anthropic

import "time"

// newTestClientWithBase creates a Client pointed at a test HTTP server.
// Disables SDK transport for use with httptest servers.
func newTestClientWithBase(baseURL, key string) *Client {
	c := NewClient(StaticToken(key), 120*time.Second)
	c.SetBaseURL(baseURL)
	c.SetUseSDK(false)
	return c
}

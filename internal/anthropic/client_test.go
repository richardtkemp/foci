package anthropic

import (
	"testing"
	"time"
)

func TestSignalRecoveryNoOp(t *testing.T) {
	// Proves that signalRecovery is safe to call when no recovery channel has been configured — it should be a no-op that does not panic.
	client := NewClient(StaticToken("test-key"), 60*time.Second)
	client.signalRecovery() // no-op, no panic
}

func TestNewClientDefaults(t *testing.T) {
	// Proves that NewClient sets the production Anthropic API base URL.
	client := NewClient(StaticToken("my-key"), 60*time.Second)
	if client.baseURL != "https://api.anthropic.com" {
		t.Errorf("baseURL = %q", client.baseURL)
	}
}

func TestSetBaseURL(t *testing.T) {
	// Proves that SetBaseURL overrides the base URL, enabling tests to point the client at a local mock server.
	client := NewClient(StaticToken("test-key"), 60*time.Second)
	client.SetBaseURL("http://localhost:8080")
	if client.baseURL != "http://localhost:8080" {
		t.Errorf("baseURL = %q", client.baseURL)
	}
}

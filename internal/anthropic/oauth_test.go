package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- TokenFunc integration ---

func TestClientWithTokenFunc(t *testing.T) {
	// Proves that a client configured with a tokenFunc calls it on each request and uses the returned token as the Bearer credential.
	var callCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer dynamic-token" {
			t.Errorf("Authorization = %q, want Bearer dynamic-token", auth)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_dynamic",
			Content:    TextContent("ok"),
			StopReason: "end_turn",
		})
	}))
	defer server.Close()

	client := &Client{
		tokenFunc: func() (string, error) {
			callCount++
			return "dynamic-token", nil
		},
		httpClient:     http.DefaultClient,
		baseURL:        server.URL,
		retryBaseDelay: 1,
	}

	_, err := client.SendMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if callCount == 0 {
		t.Error("tokenFunc was never called")
	}
}

func TestClientTokenFuncError(t *testing.T) {
	// Proves that when the tokenFunc returns an error, SendMessage propagates it rather than sending a request with an empty or stale token.
	client := &Client{
		tokenFunc: func() (string, error) {
			return "", fmt.Errorf("token expired")
		},
		httpClient:     http.DefaultClient,
		baseURL:        "http://localhost",
		retryBaseDelay: 1,
	}

	_, err := client.SendMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "token expired") {
		t.Errorf("error = %q, want 'token expired'", err.Error())
	}
}

// --- Credential format parsing ---

func TestParseCredentialsNativeFormat(t *testing.T) {
	// Proves that parseCredentials handles foci's native flat JSON format, mapping top-level access_token, refresh_token, and expires_at to the OAuthCredentials struct.
	data := `{"access_token":"native-at","refresh_token":"native-rt","expires_at":1234567890}`
	creds, err := parseCredentials([]byte(data))
	if err != nil {
		t.Fatalf("parseCredentials: %v", err)
	}
	if creds.AccessToken != "native-at" {
		t.Errorf("AccessToken = %q", creds.AccessToken)
	}
	if creds.RefreshToken != "native-rt" {
		t.Errorf("RefreshToken = %q", creds.RefreshToken)
	}
	if creds.ExpiresAt != 1234567890 {
		t.Errorf("ExpiresAt = %d", creds.ExpiresAt)
	}
}

func TestParseCredentialsClaudeFormat(t *testing.T) {
	// Proves that parseCredentials handles the Claude Code nested credential format (claudeAiOauth wrapper), extracting the inner fields correctly.
	data := `{"claudeAiOauth":{"accessToken":"claude-at","refreshToken":"claude-rt","expiresAt":9876543210}}`
	creds, err := parseCredentials([]byte(data))
	if err != nil {
		t.Fatalf("parseCredentials: %v", err)
	}
	if creds.AccessToken != "claude-at" {
		t.Errorf("AccessToken = %q", creds.AccessToken)
	}
	if creds.RefreshToken != "claude-rt" {
		t.Errorf("RefreshToken = %q", creds.RefreshToken)
	}
}

func TestParseCredentialsInvalid(t *testing.T) {
	// Proves that parseCredentials returns an error for non-JSON input.
	_, err := parseCredentials([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestUsageClientWithFunc(t *testing.T) {
	// Proves that NewUsageClient creates a usage client that calls the provided tokenFunc and sends it as the Bearer token on usage API requests.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer func-token" {
			t.Errorf("Authorization = %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		util := 30.0
		json.NewEncoder(w).Encode(UsageResponse{
			FiveHour: &UsageWindow{Utilization: &util},
		})
	}))
	defer server.Close()

	client := NewUsageClient(func() (string, error) {
		return "func-token", nil
	})
	client.baseURL = server.URL

	resp, err := client.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if resp.FiveHour == nil {
		t.Fatal("FiveHour is nil")
	}
}

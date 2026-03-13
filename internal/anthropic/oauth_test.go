package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Setup token validation ---

func TestValidateSetupToken(t *testing.T) {
	// Proves that a well-formed setup token (correct prefix and sufficient length) passes validation without error.
	// Valid token (80+ chars with correct prefix)
	valid := "sk-ant-oat01-" + strings.Repeat("a", 80)
	if err := ValidateSetupToken(valid); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
}

func TestValidateSetupTokenEmpty(t *testing.T) {
	// Proves that an empty string is rejected by ValidateSetupToken.
	if err := ValidateSetupToken(""); err == nil {
		t.Error("expected error for empty token")
	}
}

func TestValidateSetupTokenBadPrefix(t *testing.T) {
	// Proves that a token with the wrong prefix (e.g. an API key instead of an OAuth token) is rejected, and that the error message names the expected prefix.
	err := ValidateSetupToken("sk-ant-api03-" + strings.Repeat("a", 80))
	if err == nil {
		t.Fatal("expected error for wrong prefix")
	}
	if !strings.Contains(err.Error(), SetupTokenPrefix) {
		t.Errorf("error = %q, want mention of prefix", err.Error())
	}
}

func TestValidateSetupTokenTooShort(t *testing.T) {
	// Proves that a token with the right prefix but insufficient total length is rejected with a "too short" message.
	err := ValidateSetupToken("sk-ant-oat01-short")
	if err == nil {
		t.Fatal("expected error for short token")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("error = %q, want 'too short'", err.Error())
	}
}

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

func TestReadCredentials(t *testing.T) {
	// Proves that ReadCredentials correctly parses the Claude Code credential file format and returns the access token, refresh token, and expiry timestamp as separate values.
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")

	creds := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test123","refreshToken":"sk-ant-ort01-test456","expiresAt":1771770729992}}`
	os.WriteFile(path, []byte(creds), 0644)

	access, refresh, expiresAt, err := ReadCredentials(path)
	if err != nil {
		t.Fatalf("ReadCredentials: %v", err)
	}
	if access != "sk-ant-oat01-test123" {
		t.Errorf("access = %q", access)
	}
	if refresh != "sk-ant-ort01-test456" {
		t.Errorf("refresh = %q", refresh)
	}
	if expiresAt != 1771770729992 {
		t.Errorf("expiresAt = %d", expiresAt)
	}
}

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
	// Proves that NewUsageClientWithFunc creates a usage client that calls the provided tokenFunc and sends it as the Bearer token on usage API requests.
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

	client := NewUsageClientWithFunc(func() (string, error) {
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

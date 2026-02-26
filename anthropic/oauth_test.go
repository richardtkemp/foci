package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// writeTestCreds writes a credentials file and returns the path.
func writeTestCreds(t *testing.T, dir string, creds CredentialsFile) string {
	t.Helper()
	path := filepath.Join(dir, ".credentials.json")
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		t.Fatalf("marshal test creds: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write test creds: %v", err)
	}
	return path
}

func TestOAuthManagerToken(t *testing.T) {
	dir := t.TempDir()
	path := writeTestCreds(t, dir, CredentialsFile{
		ClaudeAiOauth: OAuthCredentials{
			AccessToken:  "access-token-123",
			RefreshToken: "refresh-token-456",
			ExpiresAt:    time.Now().Add(8 * time.Hour).Format(time.RFC3339),
		},
	})

	mgr, err := NewOAuthManager(path)
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	if got := mgr.Token(); got != "access-token-123" {
		t.Errorf("Token() = %q, want %q", got, "access-token-123")
	}
}

func TestOAuthManagerRefreshSuccess(t *testing.T) {
	dir := t.TempDir()
	path := writeTestCreds(t, dir, CredentialsFile{
		ClaudeAiOauth: OAuthCredentials{
			AccessToken:  "old-access",
			RefreshToken: "my-refresh-token",
			ExpiresAt:    time.Now().Add(1 * time.Minute).Format(time.RFC3339),
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)

		if req["grant_type"] != "refresh_token" {
			t.Errorf("grant_type = %q", req["grant_type"])
		}
		if req["refresh_token"] != "my-refresh-token" {
			t.Errorf("refresh_token = %q", req["refresh_token"])
		}
		if req["client_id"] != OAuthClientID {
			t.Errorf("client_id = %q", req["client_id"])
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"expires_in":    28800,
		})
	}))
	defer server.Close()

	mgr, err := NewOAuthManager(path, WithRefreshURL(server.URL))
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	if err := mgr.RefreshIfNeeded("old-access"); err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}

	if got := mgr.Token(); got != "new-access-token" {
		t.Errorf("Token() = %q, want %q", got, "new-access-token")
	}

	// Verify written back to file.
	data, _ := os.ReadFile(path)
	var creds CredentialsFile
	json.Unmarshal(data, &creds)
	if creds.ClaudeAiOauth.AccessToken != "new-access-token" {
		t.Errorf("file accessToken = %q", creds.ClaudeAiOauth.AccessToken)
	}
	if creds.ClaudeAiOauth.RefreshToken != "new-refresh-token" {
		t.Errorf("file refreshToken = %q", creds.ClaudeAiOauth.RefreshToken)
	}
}

func TestOAuthManagerRefreshDedup(t *testing.T) {
	dir := t.TempDir()
	path := writeTestCreds(t, dir, CredentialsFile{
		ClaudeAiOauth: OAuthCredentials{
			AccessToken:  "stale-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(1 * time.Minute).Format(time.RFC3339),
		},
	})

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		// Small delay to simulate network.
		time.Sleep(50 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "new-token",
			"expires_in":   28800,
		})
	}))
	defer server.Close()

	mgr, err := NewOAuthManager(path, WithRefreshURL(server.URL))
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	// Launch 10 concurrent refresh requests with the same stale token.
	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			done <- mgr.RefreshIfNeeded("stale-token")
		}()
	}

	// The first goroutine will grab the lock, refresh, and update the token.
	// Subsequent goroutines will see the token changed and skip the refresh.
	// Due to mutex serialization, some may still refresh if they grabbed the
	// lock before the first one completed. But the total should be far less
	// than 10 — practically 1 or 2.
	for i := 0; i < 10; i++ {
		if err := <-done; err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	count := requestCount.Load()
	if count > 3 {
		t.Errorf("expected ≤3 HTTP requests (dedup), got %d", count)
	}
}

func TestOAuthManagerRefreshSkipWhenAlreadyRefreshed(t *testing.T) {
	dir := t.TempDir()
	path := writeTestCreds(t, dir, CredentialsFile{
		ClaudeAiOauth: OAuthCredentials{
			AccessToken:  "current-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(8 * time.Hour).Format(time.RFC3339),
		},
	})

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "new-token",
			"expires_in":   28800,
		})
	}))
	defer server.Close()

	mgr, err := NewOAuthManager(path, WithRefreshURL(server.URL))
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	// Calling with a stale token that doesn't match should skip refresh.
	if err := mgr.RefreshIfNeeded("some-old-token"); err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}

	if requestCount.Load() != 0 {
		t.Error("expected 0 HTTP requests when stale token doesn't match")
	}
	if got := mgr.Token(); got != "current-token" {
		t.Errorf("Token() = %q, want %q", got, "current-token")
	}
}

func TestOAuthManagerFileWritebackPreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")

	// Write a file with extra fields.
	original := map[string]interface{}{
		"claudeAiOauth": map[string]interface{}{
			"accessToken":  "old-token",
			"refreshToken": "refresh-token",
			"expiresAt":    time.Now().Add(1 * time.Minute).Format(time.RFC3339),
			"scopes":       "user:inference user:usage",
			"customField":  "should-survive",
		},
		"extraSection": map[string]interface{}{
			"key": "value",
		},
	}
	data, _ := json.MarshalIndent(original, "", "  ")
	os.WriteFile(path, data, 0600)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    28800,
		})
	}))
	defer server.Close()

	mgr, err := NewOAuthManager(path, WithRefreshURL(server.URL))
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	if err := mgr.RefreshIfNeeded("old-token"); err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}

	// Read back and verify unknown fields survived.
	data, _ = os.ReadFile(path)
	var doc map[string]interface{}
	json.Unmarshal(data, &doc)

	// Check extraSection survived.
	extra, ok := doc["extraSection"].(map[string]interface{})
	if !ok {
		t.Fatal("extraSection missing from written file")
	}
	if extra["key"] != "value" {
		t.Errorf("extraSection.key = %v", extra["key"])
	}

	// Check customField survived in claudeAiOauth.
	oauth := doc["claudeAiOauth"].(map[string]interface{})
	if oauth["customField"] != "should-survive" {
		t.Errorf("customField = %v", oauth["customField"])
	}
	if oauth["scopes"] != "user:inference user:usage" {
		t.Errorf("scopes = %v", oauth["scopes"])
	}
	if oauth["accessToken"] != "new-access" {
		t.Errorf("accessToken = %v", oauth["accessToken"])
	}
}

func TestOAuthManagerInvalidCredentials(t *testing.T) {
	dir := t.TempDir()

	// Missing file.
	_, err := NewOAuthManager(filepath.Join(dir, "nonexistent.json"))
	if err == nil {
		t.Error("expected error for missing file")
	}

	// Bad JSON.
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("not json{{{"), 0600)
	_, err = NewOAuthManager(bad)
	if err == nil {
		t.Error("expected error for bad JSON")
	}

	// Missing accessToken.
	noAccess := filepath.Join(dir, "no-access.json")
	data, _ := json.Marshal(CredentialsFile{
		ClaudeAiOauth: OAuthCredentials{
			RefreshToken: "refresh",
		},
	})
	os.WriteFile(noAccess, data, 0600)
	_, err = NewOAuthManager(noAccess)
	if err == nil {
		t.Error("expected error for missing accessToken")
	}

	// Missing refreshToken.
	noRefresh := filepath.Join(dir, "no-refresh.json")
	data, _ = json.Marshal(CredentialsFile{
		ClaudeAiOauth: OAuthCredentials{
			AccessToken: "access",
		},
	})
	os.WriteFile(noRefresh, data, 0600)
	_, err = NewOAuthManager(noRefresh)
	if err == nil {
		t.Error("expected error for missing refreshToken")
	}
}

func TestOAuthManagerRefreshHTTPError(t *testing.T) {
	dir := t.TempDir()
	path := writeTestCreds(t, dir, CredentialsFile{
		ClaudeAiOauth: OAuthCredentials{
			AccessToken:  "old-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(1 * time.Minute).Format(time.RFC3339),
		},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()

	mgr, err := NewOAuthManager(path, WithRefreshURL(server.URL))
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	err = mgr.RefreshIfNeeded("old-token")
	if err == nil {
		t.Fatal("expected error for HTTP error response")
	}

	// Old token should be preserved on failure.
	if got := mgr.Token(); got != "old-token" {
		t.Errorf("Token() = %q after failed refresh, want %q", got, "old-token")
	}
}

func TestOAuthManagerProactiveRefresh(t *testing.T) {
	dir := t.TempDir()
	// Set expiry to 10 minutes from now (less than RefreshWindow of 30 min).
	path := writeTestCreds(t, dir, CredentialsFile{
		ClaudeAiOauth: OAuthCredentials{
			AccessToken:  "expiring-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(10 * time.Minute).Format(time.RFC3339),
		},
	})

	var refreshed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshed.Add(1)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "refreshed-token",
			"expires_in":   28800,
		})
	}))
	defer server.Close()

	mgr, err := NewOAuthManager(path, WithRefreshURL(server.URL))
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	// Manually check the condition that Start() would check.
	mgr.mu.Lock()
	remaining := time.Until(mgr.expiresAt)
	mgr.mu.Unlock()

	if remaining >= RefreshWindow {
		t.Fatalf("test setup: remaining %s >= RefreshWindow %s", remaining, RefreshWindow)
	}

	// Simulate what the background ticker would do.
	if err := mgr.refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if refreshed.Load() != 1 {
		t.Errorf("expected 1 refresh, got %d", refreshed.Load())
	}
	if got := mgr.Token(); got != "refreshed-token" {
		t.Errorf("Token() = %q, want %q", got, "refreshed-token")
	}
}

func TestClient401RetryWithRefreshFunc(t *testing.T) {
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		auth := r.Header.Get("Authorization")

		if n == 1 {
			// First attempt: return 401.
			if auth != "Bearer stale-token" {
				t.Errorf("attempt 1: auth = %q, want Bearer stale-token", auth)
			}
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		// Second attempt after refresh: should have new token.
		if auth != "Bearer fresh-token" {
			t.Errorf("attempt 2: auth = %q, want Bearer fresh-token", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MessageResponse{
			ID:         "msg_ok",
			Content:    TextContent("success"),
			StopReason: "end_turn",
		})
	}))
	defer server.Close()

	currentToken := "stale-token"
	client := &Client{
		tokenFunc:      func() string { return currentToken },
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		baseURL:        server.URL,
		retryBaseDelay: time.Millisecond,
		refreshFunc: func(staleToken string) error {
			if staleToken != "stale-token" {
				t.Errorf("refreshFunc staleToken = %q", staleToken)
			}
			currentToken = "fresh-token"
			return nil
		},
	}

	resp, err := client.SendMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp.ID != "msg_ok" {
		t.Errorf("resp.ID = %q", resp.ID)
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want 2", attempts.Load())
	}
}

func TestClient401WithoutRefreshFunc(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	client := NewClientWithBase(server.URL, "test-key")

	_, err := client.SendMessage(context.Background(), &MessageRequest{
		Model:     "claude-haiku-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: TextContent("hi")}},
	})

	if err == nil {
		t.Fatal("expected error for 401 without refreshFunc")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected *APIError")
	}
	if !apiErr.IsAuthError() {
		t.Error("expected IsAuthError() == true")
	}
}

func TestIsAuthError(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{401, true},
		{403, false},
		{200, false},
		{500, false},
		{429, false},
	}
	for _, tc := range tests {
		apiErr := &APIError{StatusCode: tc.status}
		if got := apiErr.IsAuthError(); got != tc.want {
			t.Errorf("IsAuthError(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestNewClientWithTokenFunc(t *testing.T) {
	token := "dynamic-token"
	client := NewClientWithTokenFunc(func() string { return token }, 30*time.Second)

	if client.tokenFunc == nil {
		t.Fatal("tokenFunc should not be nil")
	}
	if got := client.getToken(); got != "dynamic-token" {
		t.Errorf("getToken() = %q", got)
	}

	// Change the token.
	token = "updated-token"
	if got := client.getToken(); got != "updated-token" {
		t.Errorf("getToken() after update = %q", got)
	}
}

func TestClientGetTokenFallback(t *testing.T) {
	// Without tokenFunc, should use apiKey.
	client := NewClient("static-key")
	if got := client.getToken(); got != "static-key" {
		t.Errorf("getToken() = %q, want %q", got, "static-key")
	}
}

func TestOAuthManagerStartStop(t *testing.T) {
	dir := t.TempDir()
	path := writeTestCreds(t, dir, CredentialsFile{
		ClaudeAiOauth: OAuthCredentials{
			AccessToken:  "token",
			RefreshToken: "refresh",
			ExpiresAt:    time.Now().Add(8 * time.Hour).Format(time.RFC3339),
		},
	})

	mgr, err := NewOAuthManager(path)
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	mgr.Start()

	// Stop should not hang.
	done := make(chan struct{})
	go func() {
		mgr.Stop()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() hung")
	}
}

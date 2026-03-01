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
	"sync"
	"testing"
	"time"
)

func TestGeneratePKCE(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}

	// Verifier is base64url-encoded 32 bytes = 43 chars
	if len(verifier) != 43 {
		t.Errorf("verifier length = %d, want 43", len(verifier))
	}

	// Challenge is base64url-encoded SHA256 = 43 chars
	if len(challenge) != 43 {
		t.Errorf("challenge length = %d, want 43", len(challenge))
	}

	// Verifier and challenge should be different
	if verifier == challenge {
		t.Error("verifier and challenge should differ")
	}

	// Two calls should produce different values
	v2, _, _ := GeneratePKCE()
	if verifier == v2 {
		t.Error("two calls should produce different verifiers")
	}
}

func TestBuildAuthURL(t *testing.T) {
	url := BuildAuthURL("test-challenge")

	if !strings.HasPrefix(url, OAuthAuthURL+"?") {
		t.Errorf("URL should start with auth URL, got %q", url)
	}
	for _, param := range []string{
		"response_type=code",
		"client_id=" + OAuthClientID,
		"code_challenge=test-challenge",
		"code_challenge_method=S256",
	} {
		if !strings.Contains(url, param) {
			t.Errorf("URL missing param %q: %s", param, url)
		}
	}
	if !strings.Contains(url, "scope=") {
		t.Error("URL missing scope param")
	}
}

func TestExchangeCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.FormValue("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", r.FormValue("grant_type"))
		}
		if r.FormValue("client_id") != OAuthClientID {
			t.Errorf("client_id = %q", r.FormValue("client_id"))
		}
		if r.FormValue("code") != "test-code" {
			t.Errorf("code = %q", r.FormValue("code"))
		}
		if r.FormValue("code_verifier") != "test-verifier" {
			t.Errorf("code_verifier = %q", r.FormValue("code_verifier"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	creds, err := exchangeCodeWithURL(context.Background(), server.URL, "test-code", "test-verifier")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if creds.AccessToken != "new-access-token" {
		t.Errorf("AccessToken = %q", creds.AccessToken)
	}
	if creds.RefreshToken != "new-refresh-token" {
		t.Errorf("RefreshToken = %q", creds.RefreshToken)
	}
	if creds.ExpiresAt == 0 {
		t.Error("ExpiresAt should be non-zero")
	}
}

func TestExchangeCodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()

	_, err := exchangeCodeWithURL(context.Background(), server.URL, "bad-code", "bad-verifier")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "status 400") {
		t.Errorf("error = %q, want status 400", err.Error())
	}
}

func TestRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.FormValue("grant_type"))
		}
		if r.FormValue("client_id") != OAuthClientID {
			t.Errorf("client_id = %q", r.FormValue("client_id"))
		}
		if r.FormValue("refresh_token") != "old-refresh" {
			t.Errorf("refresh_token = %q", r.FormValue("refresh_token"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "refreshed-access",
			"refresh_token": "refreshed-refresh",
			"expires_in":    7200,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")

	mgr := &OAuthManager{
		creds: OAuthCredentials{
			AccessToken:  "old-access",
			RefreshToken: "old-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		},
		credsPath:  credsPath,
		httpClient: http.DefaultClient,
		tokenURL:   server.URL,
	}

	if err := mgr.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	token, err := mgr.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "refreshed-access" {
		t.Errorf("Token() = %q, want refreshed-access", token)
	}
}

func TestRefreshTokenSavesToDisk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "disk-access",
			"refresh_token": "disk-refresh",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")

	mgr := &OAuthManager{
		creds: OAuthCredentials{
			AccessToken:  "old",
			RefreshToken: "old-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		},
		credsPath:  credsPath,
		httpClient: http.DefaultClient,
		tokenURL:   server.URL,
	}

	if err := mgr.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Verify file was written
	data, err := os.ReadFile(credsPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var saved OAuthCredentials
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if saved.AccessToken != "disk-access" {
		t.Errorf("saved AccessToken = %q", saved.AccessToken)
	}
	if saved.RefreshToken != "disk-refresh" {
		t.Errorf("saved RefreshToken = %q", saved.RefreshToken)
	}
}

func TestRefreshTokenError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer server.Close()

	mgr := &OAuthManager{
		creds: OAuthCredentials{
			AccessToken:  "old",
			RefreshToken: "bad-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		},
		credsPath:  filepath.Join(t.TempDir(), "creds.json"),
		httpClient: http.DefaultClient,
		tokenURL:   server.URL,
	}

	err := mgr.Refresh()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "status 401") {
		t.Errorf("error = %q, want status 401", err.Error())
	}
}

func TestNewOAuthManagerNative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")

	creds := OAuthCredentials{
		AccessToken:  "native-access",
		RefreshToken: "native-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}
	data, _ := json.Marshal(creds)
	os.WriteFile(path, data, 0600)

	mgr, err := NewOAuthManager(path)
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	token, err := mgr.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "native-access" {
		t.Errorf("Token() = %q", token)
	}
}

func TestNewOAuthManagerClaudeFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")

	data := `{"claudeAiOauth":{"accessToken":"claude-access","refreshToken":"claude-refresh","expiresAt":` +
		fmt.Sprintf("%d", time.Now().Add(time.Hour).UnixMilli()) + `}}`
	os.WriteFile(path, []byte(data), 0600)

	mgr, err := NewOAuthManager(path)
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	token, err := mgr.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "claude-access" {
		t.Errorf("Token() = %q", token)
	}
}

func TestNewOAuthManagerMissingFile(t *testing.T) {
	_, err := NewOAuthManager("/nonexistent/path/creds.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestNewOAuthManagerNoRefreshToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")

	// File has access token but no refresh token (like a static setup-token)
	data := `{"claudeAiOauth":{"accessToken":"static-token","refreshToken":"","expiresAt":0}}`
	os.WriteFile(path, []byte(data), 0600)

	_, err := NewOAuthManager(path)
	if err == nil {
		t.Fatal("expected error for missing refresh token")
	}
	if !strings.Contains(err.Error(), "no refresh_token") {
		t.Errorf("error = %q, want 'no refresh_token'", err.Error())
	}
}

func TestTokenThreadSafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	creds := OAuthCredentials{
		AccessToken:  "safe-token",
		RefreshToken: "safe-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}
	data, _ := json.Marshal(creds)
	os.WriteFile(path, data, 0600)

	mgr, err := NewOAuthManager(path)
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := mgr.Token()
			if err != nil {
				t.Errorf("Token: %v", err)
			}
			if token != "safe-token" {
				t.Errorf("Token() = %q", token)
			}
		}()
	}
	wg.Wait()
}

func TestSaveCredentialsPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")

	creds := &OAuthCredentials{
		AccessToken:  "perm-test",
		RefreshToken: "perm-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}

	if err := saveCredentials(path, creds); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestStartRefreshBackground(t *testing.T) {
	var refreshCount int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		refreshCount++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  fmt.Sprintf("refreshed-%d", refreshCount),
			"refresh_token": "bg-refresh",
			"expires_in":    1, // 1 second — will trigger quick re-refresh
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	credsPath := filepath.Join(dir, "creds.json")

	mgr := &OAuthManager{
		creds: OAuthCredentials{
			AccessToken:  "bg-access",
			RefreshToken: "bg-refresh",
			ExpiresAt:    time.Now().Add(100 * time.Millisecond).UnixMilli(), // nearly expired
		},
		credsPath:  credsPath,
		httpClient: http.DefaultClient,
		tokenURL:   server.URL,
	}

	ctx, cancel := context.WithCancel(context.Background())
	mgr.StartRefresh(ctx)

	// Wait for at least one refresh
	time.Sleep(500 * time.Millisecond)
	cancel()

	mu.Lock()
	count := refreshCount
	mu.Unlock()

	if count == 0 {
		t.Error("expected at least one background refresh")
	}
}

func TestClientWithTokenFunc(t *testing.T) {
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
		retryBaseDelay: time.Millisecond,
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
	client := &Client{
		tokenFunc: func() (string, error) {
			return "", fmt.Errorf("token expired")
		},
		httpClient:     http.DefaultClient,
		baseURL:        "http://localhost",
		retryBaseDelay: time.Millisecond,
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

func TestExpiresIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	future := time.Now().Add(time.Hour)
	creds := OAuthCredentials{
		AccessToken:  "test",
		RefreshToken: "test-refresh",
		ExpiresAt:    future.UnixMilli(),
	}
	data, _ := json.Marshal(creds)
	os.WriteFile(path, data, 0600)

	mgr, err := NewOAuthManager(path)
	if err != nil {
		t.Fatalf("NewOAuthManager: %v", err)
	}

	ttl := mgr.ExpiresIn()
	if ttl < 59*time.Minute || ttl > 61*time.Minute {
		t.Errorf("ExpiresIn() = %s, want ~1h", ttl)
	}
}

func TestReadCredentials(t *testing.T) {
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
	_, err := parseCredentials([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestUsageClientWithFunc(t *testing.T) {
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

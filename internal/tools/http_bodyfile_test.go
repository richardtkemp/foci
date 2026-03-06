package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHTTPRequestBodyFile verifies request body can be loaded from file
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
	if !strings.Contains(result.Text, "HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if receivedBody != `{"audio":"base64data","model":"whisper"}` {
		t.Errorf("server received body = %q", receivedBody)
	}
}

// TestHTTPRequestBodyFileWithSecrets verifies secrets in body_file are resolved
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

// TestHTTPRequestBodyFileNotFound verifies missing body_file is rejected
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

// TestHTTPRequestBodyFileMutualExclusionWithBody verifies body + body_file is rejected
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

// TestHTTPRequestBodyFileMutualExclusionWithFiles verifies body_file + files is rejected
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

// TestHTTPRequestBodyFileIsDirectory verifies directories are rejected as body_file
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

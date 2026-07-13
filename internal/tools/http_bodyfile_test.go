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

func TestHTTPRequestBodyFile(t *testing.T) {
	// Proves that body_file reads a file's contents and sends them verbatim as the request body, so the server receives exactly the file's bytes.
	t.Parallel()
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		receivedBody = string(data)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	bodyPath := filepath.Join(t.TempDir(), "payload.json")
	os.WriteFile(bodyPath, []byte(`{"audio":"base64data","model":"whisper"}`), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
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

func TestHTTPRequestBodyFileWithSecrets(t *testing.T) {
	// Proves that secret templates inside a body_file are resolved before sending, so the server receives the actual secret value with no unresolved placeholders.
	t.Parallel()
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
allowed_in_body = ["api_key"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	bodyPath := filepath.Join(t.TempDir(), "payload.json")
	os.WriteFile(bodyPath, []byte(`{"key":"{{secret:custom.api_key}}","data":"hello"}`), 0644)

	tool := NewHTTPRequestTool(store, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
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
	// Proves that a nonexistent body_file path returns an error mentioning "body_file" rather than silently sending an empty body.
	t.Parallel()
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
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
	// Proves that specifying both "body" and "body_file" is rejected as mutually exclusive, preventing ambiguous request bodies.
	t.Parallel()
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
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
	// Proves that combining body_file and files is rejected as mutually exclusive, since both would attempt to set the request body.
	t.Parallel()
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
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
	// Proves that passing a directory path as body_file returns an error mentioning "directory" rather than attempting to read the directory as file content.
	t.Parallel()
	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
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

package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHTTPRequestSaveToText verifies response body is saved to file with save_to parameter
func TestHTTPRequestSaveToText(t *testing.T) {
	t.Parallel()
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
	if !strings.Contains(result.Text, "HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if !strings.Contains(result.Text, savePath) {
		t.Errorf("expected save path in result: %s", result.Text)
	}
	if strings.Contains(result.Text, "hello world") {
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

// TestHTTPRequestSaveToParentDirs verifies parent directories are created as needed
func TestHTTPRequestSaveToParentDirs(t *testing.T) {
	t.Parallel()
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

// TestHTTPRequestBinaryAutoSave verifies binary responses are auto-saved without save_to
func TestHTTPRequestBinaryAutoSave(t *testing.T) {
	t.Parallel()
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

	if !strings.Contains(result.Text, "Saved") {
		t.Errorf("expected Saved in result: %s", result.Text)
	}
	if !strings.Contains(result.Text, ".png") {
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

// TestHTTPRequestTextNotAutoSaved verifies text responses are returned inline, not saved
func TestHTTPRequestTextNotAutoSaved(t *testing.T) {
	t.Parallel()
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
	if strings.Contains(result.Text, "Saved") {
		t.Errorf("text response should not be auto-saved: %s", result.Text)
	}
	if !strings.Contains(result.Text, `"status":"ok"`) {
		t.Errorf("expected body in result: %s", result.Text)
	}
}

// TestHTTPRequestSaveFromJSONPath verifies response data can be extracted via JSON path
func TestHTTPRequestSaveFromJSONPath(t *testing.T) {
	t.Parallel()
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

	if !strings.Contains(result.Text, "Saved") {
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

// TestHTTPRequestSaveFromJSONPathDataURI verifies data URIs can be extracted and saved
func TestHTTPRequestSaveFromJSONPathDataURI(t *testing.T) {
	// Simulate an image generation API returning base64 data URI
	t.Parallel()
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

	if !strings.Contains(result.Text, "Saved") {
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

// TestHTTPRequestSaveFromJSONPathRequiresSaveTo verifies save_from_json_path requires save_to
func TestHTTPRequestSaveFromJSONPathRequiresSaveTo(t *testing.T) {
	t.Parallel()
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

// TestHTTPRequestSaveToLargeBody verifies large responses can be saved to file
func TestHTTPRequestSaveToLargeBody(t *testing.T) {
	// 2MB response — exceeds 1MB inline limit but within 10MB save_to limit
	t.Parallel()
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
	if !strings.Contains(result.Text, "2097152") {
		t.Errorf("result should mention byte count: %s", result.Text)
	}
}

// TestHTTPRequestMaxResponseBytesOverride verifies response truncation respects max_response_bytes
func TestHTTPRequestMaxResponseBytesOverride(t *testing.T) {
	// 500KB response, override limit to 256KB — should truncate
	t.Parallel()
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

// TestHTTPRequestMaxResponseBytesLargeOverride verifies larger max_response_bytes override
func TestHTTPRequestMaxResponseBytesLargeOverride(t *testing.T) {
	// 3MB response with save_to, override limit to 5MB
	t.Parallel()
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

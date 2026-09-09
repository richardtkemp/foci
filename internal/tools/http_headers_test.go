package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestHTTPResponseHeaders_AllShown proves the response header block carries
// every header the server sent, not the four-name allowlist that used to cap it
// (#1810). Retry-After, the x-ratelimit-* family and Link were unreachable
// through the tool at any flag combination; each is asserted by name here.
func TestHTTPResponseHeaders_AllShown(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "42")
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "17")
		w.Header().Set("Link", `<https://example.com/p2>; rel="next"`)
		w.Header().Set("Etag", `W/"abc123"`)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]any{"url": srv.URL, "include_headers": true})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, want := range []string{
		"Retry-After: 42",
		"X-Ratelimit-Remaining-Tokens: 17",
		`Link: <https://example.com/p2>; rel="next"`,
		`Etag: W/"abc123"`,
		"Content-Type: application/json",
	} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("header block is missing %q\ngot:\n%s", want, result.Text)
		}
	}
}

// TestHTTPResponseHeaders_RepeatedHeaderKeepsEveryValue proves a header sent
// more than once prints one line per value. Set-Cookie is the case that
// actually occurs; collapsing to the last value would silently drop cookies.
func TestHTTPResponseHeaders_RepeatedHeaderKeepsEveryValue(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Trace", "first")
		w.Header().Add("X-Trace", "second")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]any{"url": srv.URL, "include_headers": true})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"X-Trace: first", "X-Trace: second"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("header block is missing %q\ngot:\n%s", want, result.Text)
		}
	}
}

// TestHTTPResponseHeaders_SecretRedacted proves the header block goes through
// the same secret redaction as the body. Before #1810 only the body was
// redacted, so a resolved {{secret:}} echoed back in a response header printed
// raw. Showing every header is only safe because this holds.
func TestHTTPResponseHeaders_SecretRedacted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the caller's bearer token back in a response header, as debug and
		// echo endpoints really do.
		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-header-redaction-canary"
allowed_hosts = ["%s"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tool := NewHTTPRequestTool(store, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
	params, _ := json.Marshal(map[string]any{
		"url":             srv.URL,
		"include_headers": true,
		"headers":         map[string]string{"Authorization": "Bearer {{secret:custom.api_key}}"},
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(result.Text, "sk-header-redaction-canary") {
		t.Errorf("secret leaked in the header block:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "X-Echo-Auth: Bearer [REDACTED]") {
		t.Errorf("expected the echoed header redacted, got:\n%s", result.Text)
	}
}

// TestHTTPRequestIncludeHeadersDefaultsToBodyOnly pins the tool-level contract
// behind include_headers (#1817): unset, the result is the body and nothing
// else, so `foci_http_request URL | jq` and API-backend callers see the same
// clean output; set, the status line leads and the header block follows.
// Before this the strip lived only in the exec bridge, so the flag was absent
// from the schema and --help, and non-bridge callers always got headers.
func TestHTTPRequestIncludeHeadersDefaultsToBodyOnly(t *testing.T) {
	t.Parallel()
	const body = `{"ok":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-1817")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)

	params, _ := json.Marshal(map[string]any{"url": srv.URL})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute (default): %v", err)
	}
	if result.Text != body {
		t.Errorf("default result should be the body only\nwant %q\ngot  %q", body, result.Text)
	}

	params, _ = json.Marshal(map[string]any{"url": srv.URL, "include_headers": false})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute (include_headers=false): %v", err)
	}
	if result.Text != body {
		t.Errorf("include_headers=false result should be the body only\nwant %q\ngot  %q", body, result.Text)
	}

	params, _ = json.Marshal(map[string]any{"url": srv.URL, "include_headers": true})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute (include_headers=true): %v", err)
	}
	if !strings.HasPrefix(result.Text, "HTTP 200 OK\n") {
		t.Errorf("include_headers=true result should start with the status line, got:\n%s", result.Text)
	}
	if !strings.Contains(result.Text, "X-Request-Id: req-1817\n") {
		t.Errorf("include_headers=true result should carry the header block, got:\n%s", result.Text)
	}
	if !strings.HasSuffix(result.Text, "\n\n"+body) {
		t.Errorf("include_headers=true result should end with a blank line then the body, got:\n%s", result.Text)
	}
}

// TestHTTPRequestSaveToIncludeHeaders pins what save_to actually emits, which
// its description now states: "Saved N bytes to <path>" alone by default, the
// header block in front of it when include_headers is set. The old description
// promised "status and headers only" and delivered neither (#1817).
func TestHTTPRequestSaveToIncludeHeaders(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	tool := NewHTTPRequestTool(nil, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, nil, 0640)
	savePath := filepath.Join(t.TempDir(), "out.txt")
	want := "Saved 5 bytes to " + savePath

	params, _ := json.Marshal(map[string]any{"url": srv.URL, "save_to": savePath})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute (default): %v", err)
	}
	if result.Text != want {
		t.Errorf("save_to default result\nwant %q\ngot  %q", want, result.Text)
	}

	params, _ = json.Marshal(map[string]any{"url": srv.URL, "save_to": savePath, "include_headers": true})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute (include_headers=true): %v", err)
	}
	if !strings.HasPrefix(result.Text, "HTTP 200 OK\n") || !strings.HasSuffix(result.Text, "\n\n"+want) {
		t.Errorf("save_to with include_headers should be status, headers, blank line, Saved line; got:\n%s", result.Text)
	}
}

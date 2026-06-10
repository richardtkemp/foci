package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSend_RejectsSessionTraversal proves the /send request boundary rejects a
// request-controlled session name containing path traversal (P1-5): the handler
// returns 400 Bad Request instead of constructing a key that would escape the
// session directory. Reuses webhookTestSetup, which wires the full handler mux.
func TestSend_RejectsSessionTraversal(t *testing.T) {
	d, _ := webhookTestSetup(t, t.TempDir(), "", nil)
	mux := newWebhookMux(d)

	malicious := []string{
		"../../../../other-agent/c123/0",
		"a/b",
		"..",
	}
	for _, name := range malicious {
		body := `{"agent":"test-agent","text":"hello","session":"` + name + `"}`
		req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("POST /send session=%q: status = %d, want 400; body: %s",
				name, w.Code, w.Body.String())
		}
	}
}

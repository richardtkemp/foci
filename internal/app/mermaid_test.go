package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/mermaid"
	"foci/internal/mermaid/mermaidtest"
)

func mermaidHub(t *testing.T, cmd string) (*Hub, string) {
	t.Helper()
	h := newTestHub()
	h.mermaid = mermaid.New(t.TempDir(), cmd, "/no/browser")
	d := h.devices.pair("mm", "")
	return h, d.Token
}

func postMermaid(h *Hub, tok, query, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/app/render/mermaid"+query, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	h.ServeMermaid(w, req)
	return w
}

func TestServeMermaid_RendersAndCaches(t *testing.T) {
	cmd, runs := mermaidtest.FakeCmd(t)
	h, tok := mermaidHub(t, cmd)

	w := postMermaid(h, tok, "?theme=dark", "graph TD\n A-->B\n")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d (%s), want 200", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %q, want image/png", ct)
	}
	if !strings.HasPrefix(w.Body.String(), "\x89PNG") {
		t.Errorf("body is not the rendered PNG: %q", w.Body.String())
	}
	// Same source and theme: served from the cache, no second render.
	if w := postMermaid(h, tok, "?theme=dark", "graph TD\n A-->B\n"); w.Code != http.StatusOK {
		t.Fatalf("cached code = %d", w.Code)
	}
	if n := mermaidtest.RunCount(t, runs); n != 1 {
		t.Errorf("renders = %d, want 1 (second request must hit the cache)", n)
	}
	// A different theme is a different picture.
	if w := postMermaid(h, tok, "", "graph TD\n A-->B\n"); w.Code != http.StatusOK {
		t.Fatalf("default-theme code = %d", w.Code)
	}
	b, _ := os.ReadFile(runs)
	if got := string(b); got != "dark\ndefault\n" {
		t.Errorf("themes passed to mmdc = %q, want dark then default", got)
	}
}

func TestServeMermaid_ConcurrentSameSourceRendersOnce(t *testing.T) {
	cmd, runs := mermaidtest.FakeCmd(t)
	h, tok := mermaidHub(t, cmd)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = postMermaid(h, tok, "", "graph LR\n SLOW-->Y\n").Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d code = %d", i, c)
		}
	}
	if n := mermaidtest.RunCount(t, runs); n != 1 {
		t.Errorf("renders = %d, want 1 (identical concurrent requests share a render)", n)
	}
}

func TestServeMermaid_SyntaxErrorIs422WithMessage(t *testing.T) {
	cmd, _ := mermaidtest.FakeCmd(t)
	h, tok := mermaidHub(t, cmd)
	w := postMermaid(h, tok, "", "graph TD\n BAD-->\n")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422", w.Code)
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "Parse error on line 2:") || !strings.Contains(body, "Expecting NODE_STRING") {
		t.Errorf("body = %q, want mmdc's parse error", body)
	}
	if strings.Contains(body, "intercept.invalid") || strings.Contains(body, "#evaluate") {
		t.Errorf("body leaks the JS stack: %q", body)
	}
}

func TestServeMermaid_BrowserFailureIsServerError(t *testing.T) {
	cmd, _ := mermaidtest.FakeCmd(t)
	h, tok := mermaidHub(t, cmd)
	// Not the diagram's fault, so not a 422: the app would show it as a
	// syntax error under the user's diagram.
	if w := postMermaid(h, tok, "", "graph TD\n NOBROWSER-->B\n"); w.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", w.Code)
	}
}

func TestServeMermaid_Unavailable(t *testing.T) {
	h, tok := mermaidHub(t, "/nonexistent/mmdc")
	if w := postMermaid(h, tok, "", "graph TD\n A-->B\n"); w.Code != http.StatusNotImplemented {
		t.Errorf("missing mmdc: code = %d, want 501", w.Code)
	}
	h.mermaid = mermaid.New(t.TempDir(), mermaid.DisabledCmd, "")
	if w := postMermaid(h, tok, "", "graph TD\n A-->B\n"); w.Code != http.StatusNotImplemented {
		t.Errorf("mermaid_cmd=off: code = %d, want 501", w.Code)
	}
	h.mermaid = nil
	if w := postMermaid(h, tok, "", "graph TD\n A-->B\n"); w.Code != http.StatusNotImplemented {
		t.Errorf("no renderer: code = %d, want 501", w.Code)
	}
}

func TestServeMermaid_Timeout(t *testing.T) {
	cmd, _ := mermaidtest.FakeCmd(t)
	h, tok := mermaidHub(t, cmd)
	h.mermaid.Timeout = 200 * time.Millisecond
	if w := postMermaid(h, tok, "", "graph TD\n SLEEP-->B\n"); w.Code != http.StatusGatewayTimeout {
		t.Errorf("code = %d, want 504", w.Code)
	}
}

func TestServeMermaid_RequestValidation(t *testing.T) {
	cmd, runs := mermaidtest.FakeCmd(t)
	h, tok := mermaidHub(t, cmd)
	if w := postMermaid(h, "", "", "graph TD\n A-->B\n"); w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: code = %d, want 401", w.Code)
	}
	if w := postMermaid(h, tok, "?theme=neon", "graph TD\n A-->B\n"); w.Code != http.StatusBadRequest {
		t.Errorf("bad theme: code = %d, want 400", w.Code)
	}
	if w := postMermaid(h, tok, "", "  \n"); w.Code != http.StatusBadRequest {
		t.Errorf("empty: code = %d, want 400", w.Code)
	}
	big := "graph TD\n" + strings.Repeat("A-->B\n", mermaid.MaxSource/6+1)
	if w := postMermaid(h, tok, "", big); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: code = %d, want 413", w.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/app/render/mermaid", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeMermaid(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: code = %d, want 405", w.Code)
	}
	if n := mermaidtest.RunCount(t, runs); n != 0 {
		t.Errorf("renders = %d, want 0 (every request above is refused before rendering)", n)
	}
}

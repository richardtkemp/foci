package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMmdc writes a stand-in for mermaid-cli: it copies a fixed PNG to the -o
// path and logs each run (one line per call) to runs. A source containing BAD
// fails like a real parse error; NOBROWSER like a browser that will not
// start; SLOW takes 300ms; SLEEP hangs.
func fakeMmdc(t *testing.T) (cmd, runs string) {
	t.Helper()
	dir := t.TempDir()
	runs = filepath.Join(dir, "runs")
	cmd = filepath.Join(dir, "mmdc")
	script := `#!/bin/sh
in=""; out=""; theme=""
while [ $# -gt 0 ]; do
  case "$1" in
    -i) in="$2"; shift ;;
    -o) out="$2"; shift ;;
    -t) theme="$2"; shift ;;
  esac
  shift
done
echo "$theme" >> "` + runs + `"
if grep -q BAD "$in"; then
  printf 'Generating single mermaid chart\n\nError: Parse error on line 2:\nA-->\n----^\nExpecting NODE_STRING, got EOF\nParser.parseError (https://mermaid-cli-intercept.invalid/x.mjs:1:2)\n    at #evaluate (file:///x.js:1:2)\n' >&2
  exit 1
fi
if grep -q NOBROWSER "$in"; then
  printf 'Error: Failed to launch the browser process!\nThe setuid sandbox is not running as root.\n' >&2
  exit 1
fi
if grep -q SLEEP "$in"; then sleep 5; fi
if grep -q SLOW "$in"; then sleep 0.3; fi
printf '\211PNG\r\n\032\nfake' > "$out"
`
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, runs
}

func runCount(t *testing.T, runs string) int {
	t.Helper()
	b, err := os.ReadFile(runs)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func mermaidHub(t *testing.T, cmd string) (*Hub, string) {
	t.Helper()
	h := newTestHub()
	h.mermaid = newMermaidRenderer(t.TempDir(), cmd, "/no/browser")
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
	cmd, runs := fakeMmdc(t)
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
	if n := runCount(t, runs); n != 1 {
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
	cmd, runs := fakeMmdc(t)
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
	if n := runCount(t, runs); n != 1 {
		t.Errorf("renders = %d, want 1 (identical concurrent requests share a render)", n)
	}
}

func TestServeMermaid_SyntaxErrorIs422WithMessage(t *testing.T) {
	cmd, _ := fakeMmdc(t)
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
	cmd, _ := fakeMmdc(t)
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
	h.mermaid = newMermaidRenderer(t.TempDir(), mermaidDisabledCmd, "")
	if w := postMermaid(h, tok, "", "graph TD\n A-->B\n"); w.Code != http.StatusNotImplemented {
		t.Errorf("mermaid_cmd=off: code = %d, want 501", w.Code)
	}
	h.mermaid = nil
	if w := postMermaid(h, tok, "", "graph TD\n A-->B\n"); w.Code != http.StatusNotImplemented {
		t.Errorf("no renderer: code = %d, want 501", w.Code)
	}
}

func TestServeMermaid_Timeout(t *testing.T) {
	cmd, _ := fakeMmdc(t)
	h, tok := mermaidHub(t, cmd)
	h.mermaid.timeout = 200 * time.Millisecond
	if w := postMermaid(h, tok, "", "graph TD\n SLEEP-->B\n"); w.Code != http.StatusGatewayTimeout {
		t.Errorf("code = %d, want 504", w.Code)
	}
}

func TestServeMermaid_RequestValidation(t *testing.T) {
	cmd, runs := fakeMmdc(t)
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
	big := "graph TD\n" + strings.Repeat("A-->B\n", mermaidMaxSource/6+1)
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
	if n := runCount(t, runs); n != 0 {
		t.Errorf("renders = %d, want 0 (every request above is refused before rendering)", n)
	}
}

func TestMermaidPrune_KeepsMostRecent(t *testing.T) {
	m := newMermaidRenderer(t.TempDir(), "mmdc", "")
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < mermaidMaxCached+3; i++ {
		p := m.cachePath(strings.Repeat("x", i), "default")
		if err := os.WriteFile(p, []byte("png"), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := time.Unix(1_700_000_000+int64(i), 0)
		_ = os.Chtimes(p, ts, ts)
	}
	m.prune()
	entries, _ := os.ReadDir(m.dir)
	if len(entries) != mermaidMaxCached {
		t.Fatalf("after prune: %d files, want %d", len(entries), mermaidMaxCached)
	}
	for i := 0; i < 3; i++ {
		if _, err := os.Stat(m.cachePath(strings.Repeat("x", i), "default")); !os.IsNotExist(err) {
			t.Errorf("oldest file %d survived the prune", i)
		}
	}
}

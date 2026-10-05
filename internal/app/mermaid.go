package app

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"foci/internal/mermaid"
)

// Mermaid rendering (#1980). The app renders markdown natively, and neither
// Compose target can run mermaid.js, so a ```mermaid block is rendered on the
// server (internal/mermaid) into a PNG the app fetches on demand:
// POST /app/render/mermaid with the diagram source as the body.

// mermaidTheme maps the client's theme request onto a mermaid theme name. Only
// two are offered: the app picks by whether its card background is dark.
func mermaidTheme(q string) (string, bool) {
	switch q {
	case "", "default", "light":
		return "default", true
	case "dark":
		return "dark", true
	}
	return "", false
}

// ServeMermaid handles POST /app/render/mermaid?theme=dark|default: the body
// is the diagram source; the response is image/png. Errors: 413 source too
// large, 422 the diagram did not render (body: mmdc's message), 501 no
// renderer on this server (the app then shows the source), 504 timeout.
func (h *Hub) ServeMermaid(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.authBlob(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	theme, ok := mermaidTheme(r.URL.Query().Get("theme"))
	if !ok {
		http.Error(w, "bad theme", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, mermaid.MaxSource+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if len(body) > mermaid.MaxSource {
		http.Error(w, "diagram too large", http.StatusRequestEntityTooLarge)
		return
	}
	src := strings.TrimSpace(string(body))
	if src == "" {
		http.Error(w, "empty diagram", http.StatusBadRequest)
		return
	}
	if h.mermaid == nil {
		http.Error(w, mermaid.ErrUnavailable.Error(), http.StatusNotImplemented)
		return
	}
	appLog.Debugf("mermaid render: device=%s bytes=%d theme=%s", dev.DeviceID, len(src), theme)
	path, err := h.mermaid.Render(r.Context(), src, theme, mermaid.Transparent)
	var syn *mermaid.SyntaxError
	switch {
	case err == nil:
	case errors.Is(err, mermaid.ErrUnavailable):
		http.Error(w, err.Error(), http.StatusNotImplemented)
		return
	case errors.Is(err, mermaid.ErrTimeout):
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	case errors.As(err, &syn):
		http.Error(w, syn.Msg, http.StatusUnprocessableEntity)
		return
	default:
		appLog.Errorf("mermaid render for device=%s: %v", dev.DeviceID, err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		// Pruned between render and open by a concurrent render: rare, retryable.
		http.Error(w, "render evicted, retry", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "image/png")
	http.ServeContent(w, r, "diagram.png", time.Time{}, f)
}

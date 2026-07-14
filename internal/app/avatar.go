package app

import (
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)
import

// ServeAvatar handles GET /app/avatar/<agentId>: authenticates (a valid device
// valid device token, same gate as /app/blob), then serves the agent's avatar
// image range-capably via http.ServeContent. The file's mtime gives
// Last-Modified / conditional-GET / range support for free. Avatars are
// persistent (keyed by agent ID, not a TTL'd blob), so there is no reaper.
"foci/internal/log"

var (
	appLog      = log.NewComponentLogger("app")
	app_toolLog = log.NewComponentLogger("app.tool")
)

func (h *Hub) ServeAvatar(w http.ResponseWriter, r *http.Request) {
	if !h.authBlob(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/app/avatar/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "bad agent id", http.StatusBadRequest)
		return
	}
	path := h.agentAvatarPath(id)
	if path == "" {
		http.Error(w, "no avatar", http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "avatar unavailable", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.Error(w, "avatar unavailable", http.StatusNotFound)
		return
	}
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

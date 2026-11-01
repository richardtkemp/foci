package app

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"foci/internal/skills"
)

// skillFilePrefix is the path prefix of the skill-file endpoint (#2392):
// GET /app/skill/<agentId>?path=<absolute path>.
const skillFilePrefix = "/app/skill/"

// skillFileURL is the fap.Notification.SkillURL a "Loaded skill" chip carries
// for ref, read by agentID: the relative URL the app GETs to fetch the file's
// current text. The agent id rides in the path (like /app/avatar/<id>) so the
// fetch is judged against that agent's skill roots; the path is the file's
// logical path, exactly as skills.ReadScope.ServableFile will check it.
func skillFileURL(agentID string, ref skills.FileRef) string {
	return skillFilePrefix + url.PathEscape(agentID) + "?path=" + url.QueryEscape(ref.Path)
}

// ServeSkillFile handles GET /app/skill/<agentId>?path=<absolute path>: it
// authenticates (the device-token gate /app/blob and /app/ws use), then
// serves the named skill markdown file as it is on disk NOW (#2392) — the
// whole file, unparsed, so frontmatter included. Which paths are servable is
// skills.ReadScope.ServableFile against the agent's live skill scope;
// symlinks under a root are followed (skills.OpenServable).
//
// Answers, each distinct so the app can say what went wrong:
//   - 404: refused — not a .md file under one of the agent's skill roots, or
//     an unknown agent (which has no roots). Decided before any disk access,
//     so it reads the same whether or not the file exists.
//   - 410: the path is servable but no file is there any more.
//   - 500: anything else (permission, I/O).
//
// Plain HTTP on its own request: nothing here touches the app's WebSocket.
func (h *Hub) ServeSkillFile(w http.ResponseWriter, r *http.Request) {
	dev, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agentID := strings.TrimPrefix(r.URL.Path, skillFilePrefix)
	path, err := h.skillScope(agentID).ServableFile(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "not a skill file", http.StatusNotFound)
		return
	}
	f, fi, err := skills.OpenServable(path)
	switch {
	case errors.Is(err, skills.ErrNotServable):
		http.Error(w, "not a skill file", http.StatusNotFound)
		return
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "skill file gone", http.StatusGone)
		return
	case err != nil:
		appLog.Warnf("skill file GET: device=%s agent=%s path=%s: %v", dev.DeviceID, agentID, path, err)
		http.Error(w, "skill file unavailable", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	appLog.Debugf("skill file GET: device=%s agent=%s path=%s", dev.DeviceID, agentID, path)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	// The current file is the point: let the client revalidate every time
	// (ServeContent still answers a matching If-Modified-Since with 304).
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// skillScope returns the live skill scope of the agent registered as
// agentID, or the zero scope (no roots: every path refused) when there is no
// such agent.
func (h *Hub) skillScope(agentID string) skills.ReadScope {
	if agentID == "" || strings.Contains(agentID, "/") {
		return skills.ReadScope{}
	}
	conn := h.PrimaryBot(agentID)
	if conn == nil || conn.agentRef == nil {
		return skills.ReadScope{}
	}
	return conn.agentRef.SkillScope()
}

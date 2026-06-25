package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/config"
	"foci/internal/platform"
)

// pngBytes is a minimal 1x1 PNG (enough for Content-Type + body assertions).
var pngBytes = []byte("\x89PNG\r\n\x1a\n-fake-png-body-")

func hubWithAvatar(t *testing.T, agentID, avatarPath string) *Hub {
	t.Helper()
	h := newTestHub()
	h.apiKey = "secret-key"
	h.deps = platform.ProviderDeps{Config: &config.Config{
		Agents: []config.AgentConfig{{ID: agentID, Avatar: avatarPath}},
	}}
	return h
}

func TestServeAvatar_OK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "avatar.png")
	if err := os.WriteFile(path, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	h := hubWithAvatar(t, "clutch", path)

	req := httptest.NewRequest(http.MethodGet, "/app/avatar/clutch", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	w := httptest.NewRecorder()
	h.ServeAvatar(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %q, want image/png", ct)
	}
	if w.Body.String() != string(pngBytes) {
		t.Errorf("body mismatch")
	}
}

func TestServeAvatar_NoneConfigured(t *testing.T) {
	h := hubWithAvatar(t, "clutch", "") // agent exists, no avatar
	req := httptest.NewRequest(http.MethodGet, "/app/avatar/clutch", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	w := httptest.NewRecorder()
	h.ServeAvatar(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

func TestServeAvatar_UnknownAgent(t *testing.T) {
	h := hubWithAvatar(t, "clutch", "/nonexistent")
	req := httptest.NewRequest(http.MethodGet, "/app/avatar/ghost", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	w := httptest.NewRecorder()
	h.ServeAvatar(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

func TestServeAvatar_NoAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "avatar.png")
	_ = os.WriteFile(path, pngBytes, 0o644)
	h := hubWithAvatar(t, "clutch", path)

	req := httptest.NewRequest(http.MethodGet, "/app/avatar/clutch", nil) // no Bearer
	w := httptest.NewRecorder()
	h.ServeAvatar(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-bearer code = %d, want 401", w.Code)
	}
}

// TestServeAvatar_RosterRef checks agentAvatarRef advertises the URL + a
// fingerprint when the file exists, and nothing when it doesn't.
func TestServeAvatar_RosterRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "avatar.png")
	if err := os.WriteFile(path, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	h := hubWithAvatar(t, "clutch", path)
	url, ver := h.agentAvatarRef("clutch")
	if url != "/app/avatar/clutch" || ver == "" {
		t.Fatalf("ref = (%q, %q), want url=/app/avatar/clutch and non-empty ver", url, ver)
	}

	h2 := hubWithAvatar(t, "clutch", "")
	if url, ver := h2.agentAvatarRef("clutch"); url != "" || ver != "" {
		t.Fatalf("ref with no avatar = (%q, %q), want empty", url, ver)
	}
}

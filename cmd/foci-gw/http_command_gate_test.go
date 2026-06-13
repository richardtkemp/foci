package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/session"
)

// TestCommand_IfInactive proves POST /command honours the activity gate: a
// command carrying if_inactive is skipped when the targeted session ran a turn
// within the window. This is the wiring behind the overnight-reset cron
// (`foci command --if-inactive 55m -a <agent> /reset`) — the gate must stop the
// reset from firing on a session that is still active or mid-turn.
//
// Mirrors TestWebhook_IfInactive: same stubConnMgr session base ("test-agent/i0")
// and same last_activity seeding via session_metadata.
func TestCommand_IfInactive(t *testing.T) {
	d, _ := webhookTestSetup(t, t.TempDir(), "", nil)

	idx, err := session.NewSessionIndex(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	// Recent session activity → an --if-inactive command must skip.
	idx.SetSessionMetadata("test-agent/i0", "last_activity", fmt.Sprintf("%d", time.Now().Unix()))
	d.sessionIndex = idx

	mux := newWebhookMux(d)

	body := `{"agent":"test-agent","command":"/reset","if_inactive":"1h"}`
	req := httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["response"] != "skipped: session recently active" {
		t.Errorf("response = %q, want skip message (gate not wired into /command?)", resp["response"])
	}
}

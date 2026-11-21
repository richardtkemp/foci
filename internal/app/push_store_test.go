package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/platform"
)

// #3196: a phone that registered its push token (and push features) before a
// gateway restart still gets wake pushes after it, without opening the app.
func TestPushTokens_SurviveHubRestart(t *testing.T) {
	dataDir := t.TempDir()
	at := time.Date(2026, 11, 21, 23, 0, 0, 0, time.UTC)

	h1 := newHub(platform.ProviderDeps{Config: &config.Config{DataDir: dataDir}})
	h1.devices.pair("phone", "Phone")
	h1.tokens.hello("phone", "tok-phone",
		[]string{featureReadWake, featureAskgwPrompts, featureAskgwValueInput, featureAlarmPush}, at)

	// The restart: a fresh hub over the same data dir, no hello yet.
	h2 := newHub(platform.ProviderDeps{Config: &config.Config{DataDir: dataDir}})
	if got := h2.tokens.targetsExcluding(nil); got["phone"] != "tok-phone" {
		t.Errorf("message wake targets after restart = %v, want phone→tok-phone", got)
	}
	if tok, ok := h2.tokens.alarmPushTarget(nil); !ok || tok != "tok-phone" {
		t.Errorf("alarm push target after restart = %q,%v, want tok-phone", tok, ok)
	}
	if got := h2.tokens.readWakeTargetsExcluding(nil); got["phone"] != "tok-phone" {
		t.Errorf("read wake targets after restart = %v", got)
	}
	if got := h2.tokens.askgwValueTargetsExcluding(nil); got["phone"] != "tok-phone" {
		t.Errorf("askgw value targets after restart = %v", got)
	}
	if !h2.tokens.askgwSeenSinceStart() {
		t.Error("askgwSeenSinceStart = false after loading an askgw-capable device")
	}
	h2.tokens.mu.RLock()
	gotAt := h2.tokens.alarmPush["phone"]
	h2.tokens.mu.RUnlock()
	if !gotAt.Equal(at) {
		t.Errorf("alarm hello time = %v, want %v (it picks the newest phone)", gotAt, at)
	}

	fi, err := os.Stat(filepath.Join(dataDir, defaultPushTokensFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("push token file mode = %v, want 0600", fi.Mode().Perm())
	}
}

// A later hello without a feature (a downgrade) is saved too: the restarted
// gateway must not push an alarm to a build that cannot handle it.
func TestPushTokens_DowngradeSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), defaultPushTokensFile)
	p := loadPushTokens(path, nil)
	p.hello("phone", "tok", []string{featureAlarmPush, featureReadWake}, time.Now())
	p.hello("phone", "", nil, time.Now()) // empty token keeps the held one

	q := loadPushTokens(path, nil)
	if _, ok := q.alarmPushTarget(nil); ok {
		t.Error("alarm target found after a downgrade hello and restart")
	}
	if got := q.readWakeTargetsExcluding(nil); len(got) != 0 {
		t.Errorf("read wake targets = %v, want none after downgrade", got)
	}
	if got := q.targetsExcluding(nil); got["phone"] != "tok" {
		t.Errorf("token = %v, want the held token kept", got)
	}
}

// An entry for a device that is no longer paired (revoked while the gateway
// was down) is dropped at load, and the file is rewritten without it.
func TestLoadPushTokens_DropsUnpairedDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), defaultPushTokensFile)
	p := loadPushTokens(path, nil)
	p.set("phone", "tok-phone")
	p.set("ghost", "tok-ghost")

	q := loadPushTokens(path, func(id string) bool { return id == "phone" })
	if got := q.targetsExcluding(nil); len(got) != 1 || got["phone"] != "tok-phone" {
		t.Errorf("targets = %v, want only phone", got)
	}
	var saved []savedPushDevice
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].DeviceID != "phone" {
		t.Errorf("saved = %+v, want only phone after the prune", saved)
	}
}

// Revoking a device forgets its push token, in memory and on disk: a revoked
// device gets no more wake pushes (they carry previews), even after a restart.
func TestServeRevoke_ForgetsPushToken(t *testing.T) {
	dataDir := t.TempDir()
	h := newHub(platform.ProviderDeps{Config: &config.Config{DataDir: dataDir}})
	actor := h.devices.pair("phone-1", "One")
	h.devices.pair("phone-2", "Two")
	h.tokens.hello("phone-2", "tok-2", []string{featureAlarmPush}, time.Now())

	w := httptest.NewRecorder()
	h.ServeRevoke(w, secReq(http.MethodPost, "/app/pair/revoke", `{"deviceId":"phone-2"}`, "Bearer "+actor.Token))
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke code = %d, want 204", w.Code)
	}
	if got := h.tokens.targetsExcluding(nil); len(got) != 0 {
		t.Errorf("targets after revoke = %v, want none", got)
	}
	if _, ok := h.tokens.alarmPushTarget(nil); ok {
		t.Error("alarm target still set after revoke")
	}
	q := loadPushTokens(filepath.Join(dataDir, defaultPushTokensFile), nil)
	if got := q.targetsExcluding(nil); len(got) != 0 {
		t.Errorf("saved targets after revoke = %v, want none", got)
	}
}

// A stale token FCM reported (removeByToken) stays gone after a restart.
func TestPushTokens_RemoveByTokenIsSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), defaultPushTokensFile)
	p := loadPushTokens(path, nil)
	p.set("phone", "tok")
	p.removeByToken("tok")
	if got := loadPushTokens(path, nil).targetsExcluding(nil); len(got) != 0 {
		t.Errorf("targets = %v, want none after removeByToken", got)
	}
}

// An unreadable file starts the registry empty rather than failing the hub;
// phones register again at their next connect.
func TestLoadPushTokens_CorruptFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), defaultPushTokensFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := loadPushTokens(path, nil)
	if got := p.targetsExcluding(nil); len(got) != 0 {
		t.Errorf("targets = %v, want none", got)
	}
	p.set("phone", "tok") // and it still saves
	if got := loadPushTokens(path, nil).targetsExcluding(nil); got["phone"] != "tok" {
		t.Errorf("targets after rewrite = %v", got)
	}
}

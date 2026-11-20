package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"foci/internal/tools"
)

// An alarm push goes to ONE phone: the alarm-capable one whose hello is newest,
// skipping connected devices, devices without the feature, and devices with no
// token (#3194).
func TestAlarmPushTarget_PicksTheNewestCapableOfflinePhone(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	tk := newPushTokens()
	tk.set("old", "tok-old")
	tk.setAlarmPush("old", true, t0)
	tk.set("new", "tok-new")
	tk.setAlarmPush("new", true, t0.Add(time.Minute))
	tk.set("legacy", "tok-legacy") // never advertised alarmPush
	tk.setAlarmPush("legacy", false, t0.Add(time.Hour))
	tk.setAlarmPush("tokenless", true, t0.Add(2*time.Hour))

	if tok, ok := tk.alarmPushTarget(nil); !ok || tok != "tok-new" {
		t.Errorf("target = %q,%v; want tok-new (newest capable)", tok, ok)
	}
	if tok, ok := tk.alarmPushTarget(map[string]bool{"new": true}); !ok || tok != "tok-old" {
		t.Errorf("with new connected, target = %q,%v; want tok-old", tok, ok)
	}
	// A later hello without the feature (a downgrade) removes the device.
	tk.setAlarmPush("new", false, t0.Add(3*time.Hour))
	tk.setAlarmPush("old", false, t0.Add(3*time.Hour))
	if tok, ok := tk.alarmPushTarget(nil); ok {
		t.Errorf("no capable device left, got target %q", tok)
	}
}

// pushAlarm sends one high-priority data push carrying the args JSON as text,
// and returns only after FCM accepted it.
func TestPushAlarm_SendsArgsToTheTarget(t *testing.T) {
	tk := newPushTokens()
	tk.set("phone", "tok-phone")
	tk.setAlarmPush("phone", true, time.Now())
	p, rec := newRecordingPusher(t, tk)

	args := json.RawMessage(`{"hour":7,"minute":30,"label":"nursery","days":["mon"]}`)
	if err := p.pushAlarm(args, nil); err != nil {
		t.Fatalf("pushAlarm: %v", err)
	}
	got := rec.pushes() // synchronous: no wait needed
	if len(got) != 1 {
		t.Fatalf("pushes = %d, want 1", len(got))
	}
	if got[0].Token != "tok-phone" || got[0].Data["type"] != pushTypeAlarm || got[0].Data["args"] != string(args) {
		t.Errorf("push = %+v", got[0])
	}
	if got[0].Android["priority"] != "high" {
		t.Errorf("android = %v, want high priority", got[0].Android)
	}
}

func TestPushAlarm_NoDeviceAndRejectedPush(t *testing.T) {
	var nilPusher *fcmPusher
	if err := nilPusher.pushAlarm(json.RawMessage(`{}`), nil); !errors.Is(err, tools.ErrNoAlarmPushDevice) {
		t.Errorf("no push configured: err = %v, want ErrNoAlarmPushDevice", err)
	}

	tk := newPushTokens()
	p, _ := newRecordingPusher(t, tk)
	if err := p.pushAlarm(json.RawMessage(`{}`), nil); !errors.Is(err, tools.ErrNoAlarmPushDevice) {
		t.Errorf("no capable phone: err = %v, want ErrNoAlarmPushDevice", err)
	}

	// FCM refuses (a permanent 400): the error reaches the caller, so the tool
	// does not claim the alarm was held.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	tk.set("phone", "tok-phone")
	tk.setAlarmPush("phone", true, time.Now())
	bad := &fcmPusher{
		tokens: tk, window: time.Hour, lastPush: make(map[pushKey]time.Time), baseURL: srv.URL,
		projectID: "proj", ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}),
		http: srv.Client(), ctx: t.Context(),
	}
	err := bad.pushAlarm(json.RawMessage(`{}`), nil)
	if err == nil || errors.Is(err, tools.ErrNoAlarmPushDevice) {
		t.Errorf("rejected push: err = %v, want a send error", err)
	}
}

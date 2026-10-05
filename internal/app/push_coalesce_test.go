package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// pushRecorder is a fake FCM endpoint that records the registration token of
// every send it receives, and each send's data and android blocks.
type pushRecorder struct {
	mu   sync.Mutex
	sent []string
	msgs []recordedPush
}

type recordedPush struct {
	Token   string            `json:"token"`
	Data    map[string]string `json:"data"`
	Android map[string]any    `json:"android"`
}

// pushes returns a copy of every send received so far.
func (r *pushRecorder) pushes() []recordedPush {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedPush(nil), r.msgs...)
}

func (r *pushRecorder) count(token string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.sent {
		if s == token {
			n++
		}
	}
	return n
}

// waitFor polls until token has received want sends, failing after a deadline.
// Sends are async (safeGo), so an exact count needs a wait.
func (r *pushRecorder) waitFor(t *testing.T, token string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.count(token) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("token %s received %d pushes, want %d", token, r.count(token), want)
}

func newRecordingPusher(t *testing.T, tk *pushTokens) (*fcmPusher, *pushRecorder) {
	t.Helper()
	rec := &pushRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Message recordedPush `json:"message"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		rec.mu.Lock()
		rec.sent = append(rec.sent, body.Message.Token)
		rec.msgs = append(rec.msgs, body.Message)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return &fcmPusher{
		tokens:    tk,
		window:    time.Hour, // everything in a test is "inside the window"
		lastPush:  make(map[pushKey]time.Time),
		baseURL:   srv.URL,
		projectID: "proj",
		ts:        oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}),
		http:      srv.Client(),
		ctx:       t.Context(),
	}, rec
}

// A push to one device must not coalesce away the wake another device needs
// (#1204, f24df38c): the tablet was connected (excluded) for the first push and
// disconnected inside the window; the next message must still wake it.
func TestPusher_CoalescesPerDevice(t *testing.T) {
	tk := newPushTokens()
	tk.set("phone", "tok-phone")
	tk.set("tablet", "tok-tablet")
	p, rec := newRecordingPusher(t, tk)

	p.notify(pushPayload{ConvID: "c1", Preview: "a"}, map[string]bool{"tablet": true})
	rec.waitFor(t, "tok-phone", 1)

	p.notify(pushPayload{ConvID: "c1", Preview: "b"}, nil) // tablet now offline
	rec.waitFor(t, "tok-tablet", 1)

	// The phone's own window still holds: it was already woken for c1.
	time.Sleep(50 * time.Millisecond)
	if n := rec.count("tok-phone"); n != 1 {
		t.Errorf("phone pushes = %d, want 1 (second wake inside its window must coalesce)", n)
	}
}

// The one-phone failure (#1204): push at T0 to an offline phone; the phone wakes,
// connects, catches up and drops its socket; the next message inside the window
// must wake it again, because the connect consumed the first wake.
func TestPusher_DeviceConnectedReopensWindow(t *testing.T) {
	tk := newPushTokens()
	tk.set("phone", "tok-phone")
	p, rec := newRecordingPusher(t, tk)

	p.notify(pushPayload{ConvID: "c1", Preview: "a"}, nil)
	rec.waitFor(t, "tok-phone", 1)

	p.deviceConnected("phone")

	p.notify(pushPayload{ConvID: "c1", Preview: "b"}, nil)
	rec.waitFor(t, "tok-phone", 2)
}

// deviceConnected clears only that device's windows, not another device's.
func TestPusher_DeviceConnectedLeavesOtherDevices(t *testing.T) {
	tk := newPushTokens()
	tk.set("phone", "tok-phone")
	tk.set("tablet", "tok-tablet")
	p, _ := newRecordingPusher(t, tk)
	p.notify(pushPayload{ConvID: "c1"}, nil)

	p.deviceConnected("phone")

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.lastPush[pushKey{convID: "c1", deviceID: "phone"}]; ok {
		t.Error("phone's window must be cleared on connect")
	}
	if _, ok := p.lastPush[pushKey{convID: "c1", deviceID: "tablet"}]; !ok {
		t.Error("tablet's window must survive the phone connecting")
	}
}

// The hello handler is what calls deviceConnected: a device's hello must reopen
// its coalescing windows.
func TestClientHello_ClearsDevicePushWindows(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	p, _ := newRecordingPusher(t, h.tokens)
	h.pusher = p
	p.lastPush[pushKey{convID: "c1", deviceID: "d1"}] = time.Now()
	p.lastPush[pushKey{convID: "c1", deviceID: "d2"}] = time.Now()

	c := fakeClientFor(h)
	h.dispatchInbound(c, []byte(`{"t":"hello","id":"x","d":{"client":{"deviceId":"d1"},"pushToken":"ptok"}}`))

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.lastPush[pushKey{convID: "c1", deviceID: "d1"}]; ok {
		t.Error("hello from d1 must clear d1's push windows")
	}
	if _, ok := p.lastPush[pushKey{convID: "c1", deviceID: "d2"}]; !ok {
		t.Error("hello from d1 must not clear d2's push windows")
	}
}

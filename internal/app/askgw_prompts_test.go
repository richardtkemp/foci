package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"foci/internal/fap"
	flog "foci/internal/log"
	"foci/internal/platform"
)

// askgwClient is a live socket whose hello advertised askgw.prompts support.
func askgwClient(h *Hub, device string) *wsClient {
	c := deviceClient(h, device)
	c.features = featureSet([]string{featureAskgwPrompts})
	return c
}

// askgwSnapshots returns the prompt ids of every askgw.prompts frame queued
// for c, in order: one []string per frame ([] for an empty set).
func askgwSnapshots(t *testing.T, c *wsClient) [][]string {
	t.Helper()
	var out [][]string
	for _, f := range drain(t, c) {
		if f.t != fap.TypeAskgwPrompts {
			continue
		}
		raw, ok := f.d["prompts"].([]any)
		if !ok {
			t.Fatalf("askgw.prompts without a prompts list: %v", f.d)
		}
		ids := []string{}
		for _, r := range raw {
			ids = append(ids, r.(map[string]any)["promptId"].(string))
		}
		out = append(out, ids)
	}
	return out
}

var askgwButtons = []platform.ButtonChoice{
	{Label: "Approve", Data: "opt:0"},
	{Label: "Cancel", Data: "cancel"},
}

// A presented prompt reaches every capable device, carries its content, and
// is not sent to a client that did not advertise the feature (its unknown-frame
// warning would fire).
func TestAskgwPresent_BroadcastsToCapableDevicesOnly(t *testing.T) {
	h := newTestHub()
	mac, phone := askgwClient(h, "mac"), askgwClient(h, "phone")
	old := deviceClient(h, "old")

	h.presentAskgw("askgw-a1-q0", "Sudo", "Run `ls`?", "", askgwButtons, func(string) {})

	for name, c := range map[string]*wsClient{"mac": mac, "phone": phone} {
		frames := drain(t, c)
		if len(frames) != 1 || frames[0].t != fap.TypeAskgwPrompts {
			t.Fatalf("%s frames = %+v, want one askgw.prompts", name, frames)
		}
		p := frames[0].d["prompts"].([]any)[0].(map[string]any)
		if p["promptId"] != "askgw-a1-q0" || p["header"] != "Sudo" || p["text"] != "Run `ls`?" {
			t.Errorf("%s prompt = %v", name, p)
		}
		if got := len(p["choices"].([]any)); got != 2 {
			t.Errorf("%s choices = %d, want 2", name, got)
		}
	}
	if got := askgwSnapshots(t, old); len(got) != 0 {
		t.Errorf("incapable client got %v, want nothing", got)
	}
}

// The first device's answer resolves the ask exactly once and closes the
// dialog everywhere; a second device's stale answer does nothing but correct
// that device.
func TestAskgwResponse_FirstAnswerWinsAndClosesEverywhere(t *testing.T) {
	h := newTestHub()
	mac, phone := askgwClient(h, "mac"), askgwClient(h, "phone")
	var got []string
	h.presentAskgw("p1", "Sudo", "x", "", askgwButtons, func(d string) { got = append(got, d) })
	drain(t, mac)
	drain(t, phone)

	h.handleAskgwResponse(phone, fap.AskgwResponse{PromptID: "p1", Data: "opt:0"})
	if !reflect.DeepEqual(got, []string{"opt:0"}) {
		t.Fatalf("callback data = %v, want [opt:0]", got)
	}
	for name, c := range map[string]*wsClient{"mac": mac, "phone": phone} {
		if s := askgwSnapshots(t, c); !reflect.DeepEqual(s, [][]string{{}}) {
			t.Errorf("%s snapshots after answer = %v, want [[]]", name, s)
		}
	}

	h.handleAskgwResponse(mac, fap.AskgwResponse{PromptID: "p1", Data: "cancel"})
	if len(got) != 1 {
		t.Errorf("stale answer fired the callback again: %v", got)
	}
	if s := askgwSnapshots(t, mac); !reflect.DeepEqual(s, [][]string{{}}) {
		t.Errorf("stale sender snapshots = %v, want [[]] (its dialog must close)", s)
	}
	if s := askgwSnapshots(t, phone); len(s) != 0 {
		t.Errorf("stale answer broadcast to phone: %v", s)
	}
}

// Data that is not one of the prompt's choices must not consume it: askgw
// could not resolve the answer and the ask would hang until its timeout.
func TestAskgwResponse_UnknownChoiceKeepsPromptPending(t *testing.T) {
	h := newTestHub()
	phone := askgwClient(h, "phone")
	called := false
	h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) { called = true })
	drain(t, phone)

	h.handleAskgwResponse(phone, fap.AskgwResponse{PromptID: "p1", Data: "opt:9"})
	if called {
		t.Fatal("callback fired for an unknown choice")
	}
	if s := askgwSnapshots(t, phone); !reflect.DeepEqual(s, [][]string{{"p1"}}) {
		t.Errorf("snapshots = %v, want [[p1]] (still pending)", s)
	}
}

// An answer that presents the ask's next question from inside the callback
// (a multi-question ask) leaves the next question pending and shown.
func TestAskgwResponse_CallbackPresentsNextQuestion(t *testing.T) {
	h := newTestHub()
	phone := askgwClient(h, "phone")
	h.presentAskgw("q0", "", "first", "", askgwButtons, func(string) {
		h.presentAskgw("q1", "", "second", "", askgwButtons, func(string) {})
	})
	drain(t, phone)

	h.handleAskgwResponse(phone, fap.AskgwResponse{PromptID: "q0", Data: "opt:0"})
	s := askgwSnapshots(t, phone)
	if len(s) == 0 || !reflect.DeepEqual(s[len(s)-1], []string{"q1"}) {
		t.Errorf("snapshots = %v, want the last to be [q1]", s)
	}
}

// Closing a pending prompt (cancel, timeout) clears it everywhere; closing one
// that is not pending reports false and sends nothing.
func TestAskgwClose(t *testing.T) {
	h := newTestHub()
	phone := askgwClient(h, "phone")
	h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) { t.Error("callback fired on close") })
	drain(t, phone)

	if !h.closeAskgw("p1") {
		t.Fatal("closeAskgw(pending) = false")
	}
	if s := askgwSnapshots(t, phone); !reflect.DeepEqual(s, [][]string{{}}) {
		t.Errorf("snapshots after close = %v, want [[]]", s)
	}
	if h.closeAskgw("p1") {
		t.Error("closeAskgw(already closed) = true")
	}
	if s := askgwSnapshots(t, phone); len(s) != 0 {
		t.Errorf("second close sent %v", s)
	}
}

// A device that connects learns the pending set, and an empty set still goes
// out so a dialog it showed before going offline closes.
func TestAskgwHello_SendsPendingSetEvenWhenEmpty(t *testing.T) {
	h := newTestHub()
	phone := askgwClient(h, "phone")
	h.pushAskgw(phone)
	if s := askgwSnapshots(t, phone); !reflect.DeepEqual(s, [][]string{{}}) {
		t.Errorf("empty hello snapshots = %v, want [[]]", s)
	}

	h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) {})
	late := askgwClient(h, "mac")
	drain(t, late) // the broadcast reached it already; the hello push is what is under test
	h.pushAskgw(late)
	if s := askgwSnapshots(t, late); !reflect.DeepEqual(s, [][]string{{"p1"}}) {
		t.Errorf("hello snapshots = %v, want [[p1]]", s)
	}
}

// The inbound frame routes from the wire to the handler (dispatch case).
func TestAskgwResponse_DispatchedFromWire(t *testing.T) {
	h := newTestHub()
	phone := askgwClient(h, "phone")
	var got string
	h.presentAskgw("p1", "", "x", "", askgwButtons, func(d string) { got = d })
	drain(t, phone)

	h.dispatchInbound(phone, []byte(`{"t":"askgw.response","id":"i1","d":{"promptId":"p1","data":"opt:0"}}`))
	if got != "opt:0" {
		t.Errorf("callback data = %q, want opt:0", got)
	}
}

func TestAskgwPreview(t *testing.T) {
	if got := askgwPreview("Sudo", "Run\n\n`ls  -la`"); got != "Sudo: Run `ls -la`" {
		t.Errorf("preview = %q", got)
	}
	long := string(make([]rune, 500))
	if got := []rune(askgwPreview("", long)); len(got) > askgwPreviewMax {
		t.Errorf("preview len = %d, want <= %d", len(got), askgwPreviewMax)
	}
}

// A hello that advertises the feature gets the pending set over the wire; one
// that does not gets no askgw frame at all.
func TestAskgwHello_WiredIntoHelloHandler(t *testing.T) {
	h := newTestHub()
	askgwClient(h, "mac") // a capable device, so the prompt is shown app-wide at all
	h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) {})

	phone := deviceClient(h, "phone")
	h.dispatchInbound(phone, []byte(`{"t":"hello","id":"i1","d":{"client":{"deviceId":"phone","app":"foci","os":"android","version":"1"},"features":["askgwPrompts"]}}`))
	if s := askgwSnapshots(t, phone); !reflect.DeepEqual(s, [][]string{{"p1"}}) {
		t.Errorf("capable hello snapshots = %v, want [[p1]]", s)
	}

	old := deviceClient(h, "old")
	h.dispatchInbound(old, []byte(`{"t":"hello","id":"i2","d":{"client":{"deviceId":"old","app":"foci","os":"android","version":"1"}}}`))
	if s := askgwSnapshots(t, old); len(s) != 0 {
		t.Errorf("incapable hello snapshots = %v, want none", s)
	}
}

// The package entry points askgw_setup calls: no hub reports false (the caller
// falls back to the agent chat); with a hub the prompt is shown, its header
// line is stripped from the body (the dialog shows it as the title), and it
// can be closed.
func TestPresentAskgwPrompt_EntryPoints(t *testing.T) {
	setActiveHub(nil)
	if PresentAskgwPrompt("p1", "Sudo", "x", "", askgwButtons, func(string) {}) {
		t.Fatal("PresentAskgwPrompt with no hub = true, want false")
	}
	if CloseAskgwPrompt("p1") {
		t.Fatal("CloseAskgwPrompt with no hub = true")
	}

	h := newTestHub()
	setActiveHub(h)
	t.Cleanup(func() { setActiveHub(nil) })
	phone := askgwClient(h, "phone")
	if !PresentAskgwPrompt("p1", "Sudo", "**Sudo**\n\nRun `ls`?", "nuc", askgwButtons, func(string) {}) {
		t.Fatal("PresentAskgwPrompt with a hub = false")
	}
	frames := drain(t, phone)
	if len(frames) != 1 {
		t.Fatalf("frames = %+v, want one", frames)
	}
	if got := frames[0].d["prompts"].([]any)[0].(map[string]any)["text"]; got != "Run `ls`?" {
		t.Errorf("text = %q, want the embedded header stripped", got)
	}
	// #2800: the host the ask came from rides on the frame for the dialog.
	if got := frames[0].d["prompts"].([]any)[0].(map[string]any)["host"]; got != "nuc" {
		t.Errorf("host = %v, want nuc", got)
	}
	if !CloseAskgwPrompt("p1") {
		t.Error("CloseAskgwPrompt(pending) = false")
	}
}

// Fallback (#2707 review): a sudo approval must never be invisible. With no
// device at all, or only old-build devices connected, the hub refuses the
// prompt (nothing registered, nothing sent) so askgw_setup posts it to the
// agent chat; a capable device makes it app-wide.
func TestAskgwPresent_RefusesWhenNoCapableDevice(t *testing.T) {
	h := newTestHub()
	if h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) {}) {
		t.Fatal("presentAskgw with no device = true, want false (agent-chat fallback)")
	}

	old := deviceClient(h, "old")
	if h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) {}) {
		t.Fatal("presentAskgw with only an old-build device = true, want false")
	}
	if s := askgwSnapshots(t, old); len(s) != 0 {
		t.Errorf("old-build device got %v", s)
	}
	if got := h.askgw.snapshot().Prompts; len(got) != 0 {
		t.Errorf("refused prompt was registered: %v", got)
	}

	capable := askgwClient(h, "phone")
	if !h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) {}) {
		t.Fatal("presentAskgw with a capable device connected = false")
	}
	if s := askgwSnapshots(t, capable); !reflect.DeepEqual(s, [][]string{{"p1"}}) {
		t.Errorf("capable device snapshots = %v, want [[p1]]", s)
	}
}

// askgwPushHub is a test hub with a live pusher whose FCM endpoint records the
// tokens it was sent to.
func askgwPushHub(t *testing.T) (*Hub, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = append(sent, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	h := newTestHub()
	h.pusher = &fcmPusher{
		tokens:    h.tokens,
		window:    defaultPushCoalesce,
		lastPush:  make(map[pushKey]time.Time),
		baseURL:   srv.URL,
		projectID: "proj",
		ts:        oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}),
		http:      srv.Client(),
		ctx:       context.Background(), // nil makes every send fail silently
	}
	return h, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

// An offline device counts only when its latest hello advertised the feature
// and it can be woken: an offline old-build phone with a push token falls back
// to the agent chat; an offline capable phone gets the prompt and an askgw wake.
func TestAskgwPresent_OfflineDevicesCountOnlyWhenCapable(t *testing.T) {
	h, sent := askgwPushHub(t)
	h.tokens.set("oldphone", "tok-old")
	h.tokens.setAskgw("oldphone", false)
	if h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) {}) {
		t.Fatal("only an offline old-build device: presentAskgw = true, want false")
	}

	h.tokens.set("phone", "tok-new")
	h.tokens.setAskgw("phone", true)
	if !h.presentAskgw("p2", "Sudo", "x", "", askgwButtons, func(string) {}) {
		t.Fatal("offline capable device with a push token: presentAskgw = false")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let any (wrong) second wake land too
	got := sent()
	if len(got) != 1 || !strings.Contains(got[0], `"tok-new"`) || !strings.Contains(got[0], `"askgw"`) {
		t.Errorf("wakes = %v, want one askgw wake to tok-new only", got)
	}
}

// Without push configured an offline capable device cannot be woken, so it
// does not count.
func TestAskgwPresent_OfflineCapableNeedsPush(t *testing.T) {
	h := newTestHub() // no pusher
	h.tokens.set("phone", "tok-new")
	h.tokens.setAskgw("phone", true)
	if h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) {}) {
		t.Fatal("offline capable device but no pusher: presentAskgw = true, want false")
	}
}

// A fallback caused by the in-memory capability list being empty after a
// restart names that cause (#2758). A fallback after a capable device has said
// hello has another cause, so it must not blame the restart.
func TestAskgwPresent_FallbackLogNamesRestartReset(t *testing.T) {
	var buf bytes.Buffer
	flog.SetOutput(&buf)
	flog.SetLevel(flog.INFO)
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })
	const resetCause = "since this gateway started"
	refuse := func(h *Hub, what string) string {
		t.Helper()
		buf.Reset()
		if h.presentAskgw("p1", "", "x", "", askgwButtons, func(string) {}) {
			t.Fatalf("%s: presentAskgw = true, want false", what)
		}
		line := buf.String()
		if !strings.Contains(line, "p1") || !strings.Contains(line, "falling back to the agent chat") {
			t.Fatalf("%s: no fallback line for p1 at INFO: %q", what, line)
		}
		return line
	}

	h := newTestHub() // a restart: nothing has said hello
	if line := refuse(h, "fresh hub"); !strings.Contains(line, resetCause) {
		t.Errorf("fresh hub: line does not name the reset: %q", line)
	}
	h.tokens.setAskgw("oldphone", false)
	if line := refuse(h, "old-build hello only"); !strings.Contains(line, resetCause) {
		t.Errorf("old-build hello only: line does not name the reset: %q", line)
	}

	// No pusher, so an offline capable device cannot be woken: not a reset.
	h.tokens.setAskgw("phone", true)
	if line := refuse(h, "capable device offline"); strings.Contains(line, resetCause) {
		t.Errorf("capable device offline: line blames the reset: %q", line)
	}
	// A downgrade removes the device from the list; the reset is still not why.
	h.tokens.setAskgw("phone", false)
	if line := refuse(h, "capable device downgraded"); strings.Contains(line, resetCause) {
		t.Errorf("capable device downgraded: line blames the reset: %q", line)
	}
}

// The hello records the feature per device: an upgrade starts counting, a
// downgrade stops.
func TestAskgwHello_RecordsDeviceCapability(t *testing.T) {
	h := newTestHub()
	c := deviceClient(h, "phone")
	h.dispatchInbound(c, []byte(`{"t":"hello","id":"i1","d":{"client":{"deviceId":"phone"},"pushToken":"tok","features":["askgwPrompts"]}}`))
	if got := h.tokens.askgwTargetsExcluding(nil); got["phone"] != "tok" {
		t.Fatalf("after capable hello targets = %v, want phone", got)
	}
	h.dispatchInbound(c, []byte(`{"t":"hello","id":"i2","d":{"client":{"deviceId":"phone"},"pushToken":"tok"}}`))
	if got := h.tokens.askgwTargetsExcluding(nil); len(got) != 0 {
		t.Errorf("after old-build hello targets = %v, want none", got)
	}
}

package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flog "foci/internal/log"

	"foci/internal/fap"
)

func checkboxHub(t *testing.T) (*Hub, *checkboxStore) {
	t.Helper()
	return checkboxHubAt(t, filepath.Join(t.TempDir(), "app-checkboxes.db"))
}

func checkboxHubAt(t *testing.T, path string) (*Hub, *checkboxStore) {
	t.Helper()
	s, err := newCheckboxStore(path)
	if err != nil {
		t.Fatalf("newCheckboxStore(%s): %v", path, err)
	}
	t.Cleanup(s.Close)
	h := newTestHub()
	h.checkboxes = s
	return h, s
}

// checkboxSyncs collects every CheckboxSync queued for c, keyed by document
// (last one wins, matching what the client would end up applying).
func checkboxSyncs(t *testing.T, c *wsClient) map[string][]int {
	t.Helper()
	out := map[string][]int{}
	for _, f := range checkboxSyncsOrdered(t, c) {
		out[f.key] = f.lines
	}
	return out
}

// checkboxSyncsOrdered is checkboxSyncs preserving arrival order (the replay's
// most-recently-updated-first contract needs it).
type checkboxSyncFrame struct {
	key   string
	lines []int
}

func checkboxSyncsOrdered(t *testing.T, c *wsClient) []checkboxSyncFrame {
	t.Helper()
	var out []checkboxSyncFrame
	for _, f := range drain(t, c) {
		if f.t != fap.TypeCheckboxSync {
			continue
		}
		key, _ := f.d["documentId"].(string)
		raw, _ := f.d["lines"].([]any)
		lines := make([]int, 0, len(raw))
		for _, l := range raw {
			if n, ok := l.(float64); ok {
				lines = append(lines, int(n))
			}
		}
		out = append(out, checkboxSyncFrame{key: key, lines: lines})
	}
	return out
}

// wideClient is a fakeClient whose send buffer can hold a full replay burst
// (checkboxReplayMaxDocs frames): enqueue blocks 2s and then closes the socket
// when the 64-slot default fills, which would hang the test and drop frames.
func wideClient() *wsClient {
	c := fakeClient()
	c.send = make(chan []byte, 512+checkboxReplayMaxDocs)
	return c
}

// captureAppLog redirects the app component's log to a buffer for the test's
// duration — the WARN-count assertions need it. The handlers under test log
// synchronously, so the buffer is stable as soon as the call returns.
func captureAppLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	flog.SetOutput(&buf)
	flog.SetLevel(flog.DEBUG)
	t.Cleanup(func() { flog.SetOutput(os.Stderr); flog.SetLevel(flog.INFO) })
	return &buf
}

// TestHandleCheckboxPut_PersistsAndFansOutToOtherClients proves the mirror's
// fan-out half: a put is stored whole (sorted, deduplicated) and reaches the
// user's OTHER sockets as a checkbox.sync — never the sender, which already
// applied its tick locally (#1360, the draft mirror's contract).
func TestHandleCheckboxPut_PersistsAndFansOutToOtherClients(t *testing.T) {
	h, s := checkboxHub(t)
	sender, other := fakeClient(), fakeClient()
	sender.deviceID = "dev-a"
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleCheckboxPut(sender, fap.CheckboxPut{DocumentID: "doc-01J", Lines: []int{7, 3, 3, 12}})

	if lines := storedCheckboxState(t, s, "doc-01J"); len(lines) != 3 || lines[0] != 3 || lines[1] != 7 || lines[2] != 12 {
		t.Errorf("stored state = %v, want sorted deduped [3 7 12]", lines)
	}
	got := checkboxSyncs(t, other)["doc-01J"]
	if len(got) != 3 || got[0] != 3 || got[1] != 7 || got[2] != 12 {
		t.Errorf("other client CheckboxSync = %v, want doc-01J/[3 7 12]", got)
	}
	if n := len(drain(t, sender)); n != 0 {
		t.Errorf("sender received %d frame(s), want 0 (no echo to the ticking device)", n)
	}
}

// TestHandleCheckboxPut_SecondPutReplacesFirst proves last-write-wins with no
// merge: a second put on the same document replaces the stored state and the
// other client's latest sync carries only the new state.
func TestHandleCheckboxPut_SecondPutReplacesFirst(t *testing.T) {
	h, s := checkboxHub(t)
	sender, other := fakeClient(), fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleCheckboxPut(sender, fap.CheckboxPut{DocumentID: "doc-01J", Lines: []int{1, 2}})
	h.handleCheckboxPut(sender, fap.CheckboxPut{DocumentID: "doc-01J", Lines: []int{3}})

	if lines := storedCheckboxState(t, s, "doc-01J"); len(lines) != 1 || lines[0] != 3 {
		t.Errorf("stored state after second put = %v, want [3] (replace, not merge)", lines)
	}
	syncs := checkboxSyncsOrdered(t, other)
	if len(syncs) != 2 {
		t.Fatalf("other client got %d syncs, want 2", len(syncs))
	}
	if last := syncs[1]; last.key != "doc-01J" || len(last.lines) != 1 || last.lines[0] != 3 {
		t.Errorf("latest sync = %+v, want doc-01J/[3]", last)
	}
}

// TestHandleCheckboxPut_EmptyPutRemovesStoredDocAndStillFansOut proves an
// empty state is a clear: the stored document is REMOVED, and the clear still
// fans out (so it reaches devices offline at tick time via the replay being
// empty, and live devices directly). Both the existing-key and the absent-key
// case fan out — a device clearing an already-clear list is still a clear.
func TestHandleCheckboxPut_EmptyPutRemovesStoredDocAndStillFansOut(t *testing.T) {
	h, s := checkboxHub(t)
	sender, other := fakeClient(), fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}
	if err := s.put("doc-01J", "[1,2]", time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h.handleCheckboxPut(sender, fap.CheckboxPut{DocumentID: "doc-01J"})
	h.handleCheckboxPut(sender, fap.CheckboxPut{DocumentID: "doc-02J", Lines: []int{}})

	if got := s.replay(10, 0); len(got) != 0 {
		t.Errorf("replay after clears = %+v, want no stored documents", got)
	}
	syncs := checkboxSyncs(t, other)
	if lines, ok := syncs["doc-01J"]; !ok || len(lines) != 0 {
		t.Errorf("clear for existing doc = %v/%v, want an empty-state sync", lines, ok)
	}
	if lines, ok := syncs["doc-02J"]; !ok || len(lines) != 0 {
		t.Errorf("clear for absent doc = %v/%v, want an empty-state sync (a clear to peers)", lines, ok)
	}
}

// TestHandleCheckboxPut_ValidationDrops proves every validation row of
// requirement 5: a put breaking a limit is logged at WARN (exactly once,
// naming the device) and dropped — not stored, not fanned out, and silent to
// the sender on the wire (like the other mirrors' refused puts).
func TestHandleCheckboxPut_ValidationDrops(t *testing.T) {
	buf := captureAppLog(t)
	cases := []struct {
		name string
		put  fap.CheckboxPut
	}{
		{"empty key", fap.CheckboxPut{DocumentID: "", Lines: []int{1}}},
		{"key too long", fap.CheckboxPut{DocumentID: strings.Repeat("a", 129), Lines: []int{1}}},
		{"bad key charset", fap.CheckboxPut{DocumentID: "doc/../../etc", Lines: []int{1}}},
		{"negative line index", fap.CheckboxPut{DocumentID: "doc-neg", Lines: []int{-1}}},
		{"line index too high", fap.CheckboxPut{DocumentID: "doc-hi", Lines: []int{checkboxLineIndexMax + 1}}},
		{"too many entries", fap.CheckboxPut{DocumentID: "doc-many", Lines: make([]int, checkboxMaxEntries+1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, s := checkboxHub(t)
			sender, other := fakeClient(), fakeClient()
			sender.deviceID = "dev-a"
			h.clients[sender] = struct{}{}
			h.clients[other] = struct{}{}

			h.handleCheckboxPut(sender, tc.put)

			if got := s.replay(10, 0); len(got) != 0 {
				t.Errorf("rule-breaking put was stored: %+v", got)
			}
			if n := len(checkboxSyncsOrdered(t, other)); n != 0 {
				t.Errorf("fanned out %d sync(s), want 0", n)
			}
			if n := len(drain(t, sender)); n != 0 {
				t.Errorf("sender got %d frame(s), want 0 (validation is silent)", n)
			}
			if warns := strings.Count(buf.String(), fmt.Sprintf("checkbox.put %q from device dev-a:", tc.put.DocumentID)); warns != 1 {
				t.Errorf("validation WARN for %q logged %d time(s), want exactly 1:\n%s", tc.put.DocumentID, warns, buf.String())
			}
		})
	}
}

// TestHandleCheckboxPut_CapDrop_NoFanOutNoReply is the handler-level half of
// the document cap: with the store full, a NEW key's put is dropped — not
// stored, not fanned out, no reply to the sender, exactly one WARN naming the
// device and key — while an EXISTING key at the cap is still stored and fanned
// out (the "existing document can always be updated" rule).
func TestHandleCheckboxPut_CapDrop_NoFanOutNoReply(t *testing.T) {
	h, s := checkboxHub(t)
	buf := captureAppLog(t)
	seedCheckboxDocs(t, s, checkboxDocCap, time.Now().UnixMilli())
	sender, other := fakeClient(), fakeClient()
	sender.deviceID = "dev-a"
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleCheckboxPut(sender, fap.CheckboxPut{DocumentID: "doc-new", Lines: []int{1}})

	if got := s.replay(checkboxDocCap+1, 0); len(got) != checkboxDocCap {
		t.Errorf("store holds %d documents after a refused put, want exactly the cap %d", len(got), checkboxDocCap)
	}
	for _, c := range []*wsClient{other, sender} {
		if n := len(drain(t, c)); n != 0 {
			t.Errorf("%s socket got %d frame(s) at the cap, want 0 (no fan-out, no reply)", map[bool]string{true: "sender", false: "other"}[c == sender], n)
		}
	}
	warns := strings.Count(buf.String(), `checkbox.put "doc-new" from device dev-a`)
	if warns != 1 {
		t.Errorf("cap WARN logged %d time(s), want exactly 1:\n%s", warns, buf.String())
	}

	// The existing-key half: still stored and fanned out at the cap.
	h.handleCheckboxPut(sender, fap.CheckboxPut{DocumentID: "doc-00042", Lines: []int{5}})
	if lines := storedCheckboxState(t, s, "doc-00042"); len(lines) != 1 || lines[0] != 5 {
		t.Errorf("existing key at cap = %v, want [5] stored", lines)
	}
	if got, ok := checkboxSyncs(t, other)["doc-00042"]; !ok || len(got) != 1 || got[0] != 5 {
		t.Errorf("existing key at cap fanned out %v/%v, want doc-00042/[5]", got, ok)
	}
}

// TestHandleCheckboxPut_PersistFailure_NoFanOutErrorReplyOldStateReplays is
// review finding A: when the persist fails, the put must NOT fan out (the live
// devices would hold the new state while every later hello replays the old
// stored one, rolling back each reconnecting device including the ticker).
// Instead the sender gets a retryable error frame so its UI keeps the local
// state and retries, one WARN names the device and key, and a later hello
// replays the OLD stored state. The write failure is injected with
// query_only on the store's single pooled connection: reads (the replay)
// still work, writes fail — the outage shape.
func TestHandleCheckboxPut_PersistFailure_NoFanOutErrorReplyOldStateReplays(t *testing.T) {
	h, s := checkboxHub(t)
	buf := captureAppLog(t)
	if err := s.put("doc-01J", encodeCheckboxState([]int{1, 2}), time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed old state: %v", err)
	}
	s.db.SetMaxOpenConns(1)
	if _, err := s.db.Exec(`PRAGMA query_only = true`); err != nil {
		t.Fatalf("inject write outage: %v", err)
	}
	sender, other := fakeClient(), fakeClient()
	sender.deviceID = "dev-a"
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleCheckboxPut(sender, fap.CheckboxPut{DocumentID: "doc-01J", Lines: []int{9}})

	if n := len(drain(t, other)); n != 0 {
		t.Errorf("other socket got %d frame(s) on a failed persist, want 0 (no fan-out)", n)
	}
	got := drain(t, sender)
	if len(got) != 1 || got[0].t != fap.TypeError {
		t.Fatalf("sender frames = %v, want exactly one error reply", got)
	}
	if got[0].d["code"] != "checkbox_put_failed" || got[0].d["retryable"] != true {
		t.Errorf("error reply = %v, want code=checkbox_put_failed retryable=true", got[0].d)
	}
	if msg, _ := got[0].d["message"].(string); !strings.Contains(msg, "doc-01J") {
		t.Errorf("error reply message %q must name the document key", msg)
	}
	warns := strings.Count(buf.String(), `checkbox.put "doc-01J" from device dev-a: persist:`)
	if warns != 1 {
		t.Errorf("persist WARN logged %d time(s), want exactly 1:\n%s", warns, buf.String())
	}

	// The later hello replays the OLD stored state — still readable through
	// the read-only outage.
	mac := wideClient()
	h.dispatchInbound(mac, []byte(`{"t":"hello","id":"i1","d":{"client":{"deviceId":"mac","app":"foci","os":"macos","version":"1"}}}`))
	if lines, ok := checkboxSyncs(t, mac)["doc-01J"]; !ok || len(lines) != 2 || lines[0] != 1 || lines[1] != 2 {
		t.Errorf("post-failure hello replay = %v/%v, want the OLD state [1 2] (the stored value must win, not the failed put)", lines, ok)
	}
}

// TestHandleCheckboxPut_NoStore_FansOutLiveAndReplaysNothing proves the
// no-store hub (no data_dir, or the open failed): puts still fan out live,
// nothing is persisted, and a later hello replays nothing — the code's
// live-only degradation, stated in docs/WIRING.md.
func TestHandleCheckboxPut_NoStore_FansOutLiveAndReplaysNothing(t *testing.T) {
	h := newTestHub() // no checkbox store
	sender, other := fakeClient(), fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.handleCheckboxPut(sender, fap.CheckboxPut{DocumentID: "doc-01J", Lines: []int{4}})

	if got, ok := checkboxSyncs(t, other)["doc-01J"]; !ok || len(got) != 1 || got[0] != 4 {
		t.Errorf("no-store fan-out = %v/%v, want doc-01J/[4] (live-only mirror)", got, ok)
	}
	mac := fakeClient()
	h.pushCheckboxes(mac)
	if n := len(drain(t, mac)); n != 0 {
		t.Errorf("no-store hello replay sent %d frame(s), want 0 (nothing is persisted)", n)
	}
}

// TestCheckbox_DispatchPutsAndReplaysAfterHello drives both halves at the
// wire level through dispatchInbound: a `checkbox.put` envelope from a socket
// with the codec + dispatch case wired (the sender excluded, the other socket
// mirrored), then — the case that gets reported — a device that was OFFLINE
// at tick time reconnects, and its hello replays the stored state.
func TestCheckbox_DispatchPutsAndReplaysAfterHello(t *testing.T) {
	h, _ := checkboxHub(t)
	phone, mac := fakeClient(), fakeClient()
	phone.deviceID = "phone"
	h.clients[phone] = struct{}{}
	h.clients[mac] = struct{}{}

	h.dispatchInbound(phone, []byte(`{"t":"checkbox.put","id":"i1","d":{"documentId":"doc-01J","lines":[2,5]}}`))

	if got, ok := checkboxSyncs(t, mac)["doc-01J"]; !ok || len(got) != 2 || got[0] != 2 || got[1] != 5 {
		t.Errorf("live mirror = %v/%v, want doc-01J/[2 5]", got, ok)
	}
	if n := len(drain(t, phone)); n != 0 {
		t.Errorf("sender got %d frame(s), want 0", n)
	}

	// The offline-then-online device: its hello is the only chance to learn
	// the tick. The replay must be wired into the handshake branch.
	tablet := wideClient()
	h.dispatchInbound(tablet, []byte(`{"t":"hello","id":"i2","d":{"client":{"deviceId":"tablet","app":"foci","os":"android","version":"1"}}}`))
	if got, ok := checkboxSyncs(t, tablet)["doc-01J"]; !ok || len(got) != 2 || got[0] != 2 || got[1] != 5 {
		t.Errorf("hello replay = %v/%v, want doc-01J/[2 5] (the replay is not wired into the handshake)", got, ok)
	}
}

// TestCheckbox_DispatchDropsNonIntegerLines is requirement 5's "line indexes
// are integers" row at the wire level: a non-integer entry cannot decode into
// []int, so the codec rejects the frame and dispatch drops it — one WARN naming
// the device, nothing stored, nothing fanned out, no reply. This is the one
// limit row enforced below the handler (the fap layer, where []int lives), so
// dispatchInbound is the level that can prove the drop.
func TestCheckbox_DispatchDropsNonIntegerLines(t *testing.T) {
	h, s := checkboxHub(t)
	buf := captureAppLog(t)
	sender, other := fakeClient(), fakeClient()
	sender.deviceID = "dev-a"
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}

	h.dispatchInbound(sender, []byte(`{"t":"checkbox.put","id":"i1","d":{"documentId":"doc-01J","lines":[3,1.5]}}`))
	h.dispatchInbound(sender, []byte(`{"t":"checkbox.put","id":"i2","d":{"documentId":"doc-02J","lines":["7"]}}`))

	if got := s.replay(10, 0); len(got) != 0 {
		t.Errorf("non-integer put was stored: %+v", got)
	}
	if n := len(checkboxSyncsOrdered(t, other)); n != 0 {
		t.Errorf("fanned out %d sync(s), want 0", n)
	}
	if n := len(drain(t, sender)); n != 0 {
		t.Errorf("sender got %d frame(s), want 0 (a decode drop is silent)", n)
	}
	warns := strings.Count(buf.String(), "drop malformed inbound frame from device dev-a")
	if warns != 2 {
		t.Errorf("decode-drop WARNs = %d, want exactly 2 (one per frame):\n%s", warns, buf.String())
	}
}

// TestPushCheckboxes_ReplayBoundsAndOrder pins requirement 4's bounds: at most
// checkboxReplayMaxDocs documents, only those updated within the last
// checkboxReplayMaxAge days, most recently updated first.
func TestPushCheckboxes_ReplayBoundsAndOrder(t *testing.T) {
	h, s := checkboxHub(t)
	now := time.Now().UnixMilli()
	for i := 0; i < checkboxReplayMaxDocs+50; i++ {
		if err := s.put(fmt.Sprintf("doc-%05d", i), "[1]", now-int64(i)); err != nil {
			t.Fatalf("put doc-%05d: %v", i, err)
		}
	}
	if err := s.put("doc-ancient", "[1]", now-(checkboxReplayMaxAge+24*time.Hour).Milliseconds()); err != nil {
		t.Fatalf("put ancient: %v", err)
	}

	c := wideClient()
	h.pushCheckboxes(c)
	syncs := checkboxSyncsOrdered(t, c)

	if len(syncs) != checkboxReplayMaxDocs {
		t.Errorf("replayed %d document(s), want the %d bound", len(syncs), checkboxReplayMaxDocs)
	}
	if syncs[0].key != "doc-00000" {
		t.Errorf("first replayed = %q, want doc-00000 (most recently updated first)", syncs[0].key)
	}
	if syncs[len(syncs)-1].key != "doc-00199" {
		t.Errorf("last replayed = %q, want doc-00199", syncs[len(syncs)-1].key)
	}
	for _, f := range syncs {
		if f.key == "doc-ancient" {
			t.Error("a document older than the replay age bound was replayed")
		}
	}
}

// TestPushCheckboxes_SkipsUndecodableEntry proves a corrupt stored row cannot
// crash the hello replay and is not sent as a state (finding B's replay half).
func TestPushCheckboxes_SkipsUndecodableEntry(t *testing.T) {
	h, s := checkboxHub(t)
	if err := s.put("doc-good", "[1]", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.put("doc-bad", "[2]", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE app_checkboxes SET state = 'not json' WHERE doc_key = 'doc-bad'`); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	c := fakeClient()
	h.pushCheckboxes(c) // must not panic
	got := checkboxSyncs(t, c)
	if _, ok := got["doc-bad"]; ok {
		t.Error("a corrupt stored row was replayed as a state")
	}
	if lines, ok := got["doc-good"]; !ok || len(lines) != 1 || lines[0] != 1 {
		t.Errorf("good document = %v/%v, want [1]; it must still replay", lines, ok)
	}
}

// TestPushCheckboxes_SkipsInvalidStoredState is the round-3 review's finding
// that a stored state which DECODES but breaks the put limits must not be
// replayed: a `null` row is corrupt, not a clear (a clear is an absent row),
// and an out-of-range or oversized list must not reach a device out of
// contract. validateCheckboxPut gates the read path exactly as it gates the
// write path, so the replay can only send what a put could have stored.
func TestPushCheckboxes_SkipsInvalidStoredState(t *testing.T) {
	h, s := checkboxHub(t)
	seed := map[string]string{
		"doc-null":  `null`,
		"doc-range": `[-5,999999999]`,
		"doc-many":  "[" + strings.TrimSuffix(strings.Repeat("0,", checkboxMaxEntries+1), ",") + "]",
		"doc-good":  `[4]`,
	}
	now := time.Now().UnixMilli()
	for key, state := range seed {
		if _, err := s.db.Exec(
			`INSERT OR REPLACE INTO app_checkboxes (doc_key, state, updated_ms) VALUES (?, ?, ?)`, key, state, now,
		); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	c := fakeClient()
	h.pushCheckboxes(c) // must not panic
	got := checkboxSyncs(t, c)
	for _, key := range []string{"doc-null", "doc-range", "doc-many"} {
		if lines, ok := got[key]; ok {
			t.Errorf("invalid stored state %s was replayed as %v; it must be skipped, not sent out of contract", key, lines)
		}
	}
	if lines, ok := got["doc-good"]; !ok || len(lines) != 1 || lines[0] != 4 {
		t.Errorf("valid document = %v/%v, want [4]; it must still replay", lines, ok)
	}
}

// TestCheckbox_ReplayPersistsAcrossHubRebuild proves the state survives a
// gateway restart on the same storage at the HUB level: a second hub over the
// same data file replays to its own clients what the first hub stored
// (requirement 2).
func TestCheckbox_ReplayPersistsAcrossHubRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-checkboxes.db")
	h1, s1 := checkboxHubAt(t, path)
	phone := fakeClient()
	phone.deviceID = "phone"
	h1.clients[phone] = struct{}{}
	h1.dispatchInbound(phone, []byte(`{"t":"checkbox.put","id":"i1","d":{"documentId":"doc-01J","lines":[8]}}`))
	s1.Close() // graceful shutdown: the hub's data is on disk

	s2, err := newCheckboxStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(s2.Close)
	h2 := newTestHub()
	h2.checkboxes = s2

	mac := wideClient()
	h2.pushCheckboxes(mac)
	if got, ok := checkboxSyncs(t, mac)["doc-01J"]; !ok || len(got) != 1 || got[0] != 8 {
		t.Errorf("rebuilt hub replay = %v/%v, want doc-01J/[8]", got, ok)
	}
}

// TestStartAll_SweepsAgedCheckboxDocs proves requirement 6's housekeeping: a
// document not updated for checkboxDocMaxAge is removed at hub start. The hub
// here has NO frame store — newTestHub's default — which is exactly what
// proves the sweep sits before StartAll's frames early return (it must run
// even when the frame store is absent or failed to open).
func TestStartAll_SweepsAgedCheckboxDocs(t *testing.T) {
	h, s := checkboxHub(t)
	if h.frames != nil {
		t.Fatal("test premise: newTestHub has no frame store")
	}
	now := time.Now()
	if err := s.put("doc-old", "[1]", now.Add(-checkboxDocMaxAge-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.put("doc-new", "[1]", now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	h.StartAll(nil)

	got := s.replay(10, 0)
	if len(got) != 1 || got[0].key != "doc-new" {
		t.Errorf("after the start sweep = %+v, want only doc-new", got)
	}
}

// TestNormaliseCheckboxLines pins the stored/fanned state's canonical form:
// sorted, deduplicated, and never nil (an empty state must encode as
// "lines":[] on the wire, not null).
func TestNormaliseCheckboxLines(t *testing.T) {
	if got := normaliseCheckboxLines([]int{5, 3, 5, 1}); len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 5 {
		t.Errorf("normalise([5 3 5 1]) = %v, want [1 3 5]", got)
	}
	if got := normaliseCheckboxLines(nil); got == nil || len(got) != 0 {
		t.Errorf("normalise(nil) = %v, want a non-nil empty slice", got)
	}
}

// TestInboundConvID_CheckboxPutIsFireAndForget proves CheckboxPut is
// classified exactly like its DraftPut/ScrollPut/ConvPinPut siblings: no
// conversation id from inboundConvID, so the reliability gate never dedups or
// acks it — the mirror is deliberately outside per-conversation reliability.
func TestInboundConvID_CheckboxPutIsFireAndForget(t *testing.T) {
	if got := inboundConvID(fap.CheckboxPut{DocumentID: "doc-01J", Lines: []int{1}}); got != "" {
		t.Errorf("inboundConvID(CheckboxPut) = %q, want \"\" (fire-and-forget, like ScrollPut)", got)
	}
	if got := inboundConvID(fap.ScrollPut{ConversationID: "c1"}); got != "" {
		t.Errorf("control: inboundConvID(ScrollPut) = %q, want \"\"", got)
	}
	if got := inboundConvID(fap.ClientMessage{ConversationID: "c1"}); got != "c1" {
		t.Errorf("control: inboundConvID(ClientMessage) = %q, want c1", got)
	}
}

// TestHub_CloseWithAndWithoutCheckboxStore proves Hub.Close is safe both with
// no checkbox store (nil receiver) and with one, and — the load-bearing part —
// that Hub.Close owns the store's DB: after it, the store refuses further use
// (the graceful-shutdown drain-and-close contract, alongside h.frames.Close).
// A second Close of the store itself is harmless (sql.DB.Close is idempotent).
func TestHub_CloseWithAndWithoutCheckboxStore(t *testing.T) {
	bare := newTestHub()
	if err := bare.Close(); err != nil {
		t.Errorf("Close with no stores: %v", err)
	}
	h, s := checkboxHub(t)
	if err := h.Close(); err != nil {
		t.Errorf("Close with a checkbox store: %v", err)
	}
	if err := s.put("doc-after-close", "[1]", 0); err == nil {
		t.Errorf("put after Hub.Close succeeded; want the store's DB closed by Hub.Close")
	}
	s.Close() // idempotent
}

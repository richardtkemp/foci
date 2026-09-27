package app

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/sqlite"
)

// lastSnapshot returns the settings map of the most recent settings.snapshot the
// client received (nil if it received none).
func lastSnapshot(t *testing.T, c *wsClient) map[string]string {
	t.Helper()
	var out map[string]string
	for _, f := range drain(t, c) {
		if f.t != fap.TypeSettingsSnapshot {
			continue
		}
		out = map[string]string{}
		if s, ok := f.d["settings"].(map[string]any); ok {
			for k, v := range s {
				out[k], _ = v.(string)
			}
		}
	}
	return out
}

// TestHandleSettingPut_PersistsAndBroadcasts proves a setting.put stores the key
// in the global bag and fans the merged snapshot out to every settings-capable
// client — while a client that did not advertise settingsSync gets nothing.
func TestHandleSettingPut_PersistsAndBroadcasts(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}

	a := fakeClient()
	a.features = map[string]struct{}{featureSettingsSync: {}}
	b := fakeClient()
	b.features = map[string]struct{}{featureSettingsSync: {}}
	plain := fakeClient()
	h.clients[a] = struct{}{}
	h.clients[b] = struct{}{}
	h.clients[plain] = struct{}{}

	h.handleSettingPut(fap.SettingPut{Key: "theme", Value: "dark"})

	if got, _ := idx.GetSystemState(systemStateAppSettings); got == "" {
		t.Fatal("setting.put must persist the bag to system_state")
	}
	for _, c := range []*wsClient{a, b} {
		if snap := lastSnapshot(t, c); snap["theme"] != "dark" {
			t.Errorf("capable client snapshot theme = %q, want dark", snap["theme"])
		}
	}
	if len(drain(t, plain)) != 0 {
		t.Error("a client without settingsSync must not receive a snapshot")
	}
}

// TestPushSettings_SendsAccumulatedBag proves the hello-time push carries every
// previously-stored key to a capable client and skips a non-capable one.
func TestPushSettings_SendsAccumulatedBag(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.handleSettingPut(fap.SettingPut{Key: "theme", Value: "dark"})
	h.handleSettingPut(fap.SettingPut{Key: "accent_color", Value: "42"})

	c := fakeClient()
	c.features = map[string]struct{}{featureSettingsSync: {}}
	h.pushSettings(c)
	snap := lastSnapshot(t, c)
	if snap["theme"] != "dark" || snap["accent_color"] != "42" {
		t.Errorf("push snapshot = %v, want theme=dark accent_color=42", snap)
	}

	plain := fakeClient()
	h.pushSettings(plain)
	if len(drain(t, plain)) != 0 {
		t.Error("pushSettings must skip a client without settingsSync")
	}
}

// TestHandleSettingPut_IgnoresEmptyKey proves an empty key is a no-op (no bag written).
func TestHandleSettingPut_IgnoresEmptyKey(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	h.handleSettingPut(fap.SettingPut{Key: "", Value: "x"})
	if got, _ := idx.GetSystemState(systemStateAppSettings); got != "" {
		t.Error("empty key must not write the bag")
	}
}

// TestHandleSettingPut_ConcurrentPutsKeepEveryKey proves concurrent setting.put
// frames for DIFFERENT keys (two devices' read pumps) all survive in the
// persisted bag. The bag is a read-modify-write of one system_state row, so an
// unserialised load-merge-persist lets two puts read the same base and the
// second write drops the first's key (#1185).
func TestHandleSettingPut_ConcurrentPutsKeepEveryKey(t *testing.T) {
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}

	const n = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			h.handleSettingPut(fap.SettingPut{Key: fmt.Sprintf("k%d", i), Value: "v"})
		}(i)
	}
	close(start)
	wg.Wait()

	raw, err := idx.GetSystemState(systemStateAppSettings)
	if err != nil {
		t.Fatalf("GetSystemState: %v", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal bag %q: %v", raw, err)
	}
	if len(m) != n {
		t.Errorf("persisted bag has %d keys, want %d (a concurrent put was lost): %v", len(m), n, m)
	}
}

// settingsHub builds a hub over a fresh SessionIndex plus one settings-capable
// client, and returns the index's db path so a test can reach the file directly.
func settingsHub(t *testing.T) (*Hub, *session.SessionIndex, string, *wsClient) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	idx, err := session.NewSessionIndex(path)
	if err != nil {
		t.Fatalf("NewSessionIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	c := fakeClient()
	c.features = map[string]struct{}{featureSettingsSync: {}}
	h.clients[c] = struct{}{}
	return h, idx, path, c
}

// TestHandleSettingPut_UnreadableBagIsNotOverwritten proves a stored bag the
// server cannot parse is left alone: a put must not replace it with a one-key
// bag (which would silently discard every other setting), and must not
// broadcast a snapshot claiming the bag is just that one key (#2081).
func TestHandleSettingPut_UnreadableBagIsNotOverwritten(t *testing.T) {
	h, idx, _, c := settingsHub(t)
	const corrupt = `{"theme":"dark","accent_color":` // truncated JSON
	if err := idx.SetSystemState(systemStateAppSettings, corrupt); err != nil {
		t.Fatalf("seed: %v", err)
	}

	h.handleSettingPut(fap.SettingPut{Key: "font_size", Value: "14"})

	if got, _ := idx.GetSystemState(systemStateAppSettings); got != corrupt {
		t.Errorf("unreadable bag was overwritten: got %q, want it untouched", got)
	}
	if snap := lastSnapshot(t, c); snap != nil {
		t.Errorf("no snapshot may be broadcast when the bag is unreadable, got %v", snap)
	}
}

// TestPushSettings_UnreadableBagSendsNothing proves the hello-time push does not
// present an unreadable bag as an empty one.
func TestPushSettings_UnreadableBagSendsNothing(t *testing.T) {
	h, idx, _, c := settingsHub(t)
	if err := idx.SetSystemState(systemStateAppSettings, "not json"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.pushSettings(c)
	if snap := lastSnapshot(t, c); snap != nil {
		t.Errorf("pushSettings sent %v for an unreadable bag, want nothing", snap)
	}
}

// TestHandleSettingPut_FailedSaveBroadcastsStoredBag proves that when the save
// fails, devices are sent what is actually stored, not the unsaved merge — so
// no device shows a setting as synced that a restart would lose, and the sender
// is reconciled back to the stored value (#2081).
func TestHandleSettingPut_FailedSaveBroadcastsStoredBag(t *testing.T) {
	h, idx, path, c := settingsHub(t)
	h.handleSettingPut(fap.SettingPut{Key: "theme", Value: "light"})
	drain(t, c)

	// Make every write to system_state fail while reads keep working.
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("open second handle: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, op := range []string{"INSERT", "UPDATE"} {
		if _, err := db.Exec(`CREATE TRIGGER fail_` + op + ` BEFORE ` + op + ` ON system_state BEGIN SELECT RAISE(ABORT, 'forced write failure'); END`); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
	}
	if err := idx.SetSystemState("probe", "x"); err == nil {
		t.Fatal("premise: the trigger should make writes fail")
	}

	h.handleSettingPut(fap.SettingPut{Key: "theme", Value: "dark"})

	if got, _ := idx.GetSystemState(systemStateAppSettings); got != `{"theme":"light"}` {
		t.Fatalf("premise: stored bag = %q, want the pre-failure bag", got)
	}
	snap := lastSnapshot(t, c)
	if snap["theme"] != "light" {
		t.Errorf("after a failed save the snapshot must carry the stored theme=light, got %v", snap)
	}
}

// TestHandleSettingPut_SnapshotsArriveInSaveOrder proves each client receives the
// snapshots in the order the bags were saved. Every put adds a new key, so each
// saved bag is one key larger than the last; a client that sees a smaller bag
// after a larger one has had an older snapshot overwrite a newer one (#2081).
//
// Holding h.mu parks every broadcast in snapshotClients, so any put whose save
// can complete before its broadcast has gone out WILL save and then queue there;
// releasing them together makes the send order the scheduler's choice. With the
// save and the send serialised, only one put gets that far and the rest wait
// behind it, so the order is the save order.
func TestHandleSettingPut_SnapshotsArriveInSaveOrder(t *testing.T) {
	h, idx, _, _ := settingsHub(t)
	obs := fakeClient()
	obs.send = make(chan []byte, 1024)
	obs.features = map[string]struct{}{featureSettingsSync: {}}
	h.clients[obs] = struct{}{}

	const n = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			h.handleSettingPut(fap.SettingPut{Key: fmt.Sprintf("k%d", i), Value: "v"})
		}(i)
	}
	h.mu.Lock()
	close(start)
	// Wait until every put has saved (unserialised broadcast) or until it is
	// clear the rest are held back behind the first (serialised).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		raw, _ := idx.GetSystemState(systemStateAppSettings)
		m := map[string]string{}
		if json.Unmarshal([]byte(raw), &m) == nil && len(m) == n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.mu.Unlock()
	wg.Wait()

	var sizes []int
	for _, f := range drain(t, obs) {
		if f.t != fap.TypeSettingsSnapshot {
			continue
		}
		s, _ := f.d["settings"].(map[string]any)
		sizes = append(sizes, len(s))
	}
	if len(sizes) != n {
		t.Fatalf("premise: got %d snapshots, want %d", len(sizes), n)
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i] <= sizes[i-1] {
			t.Fatalf("snapshot %d has %d keys after one with %d: an older bag overtook a newer one (sizes %v)", i, sizes[i], sizes[i-1], sizes)
		}
	}
}

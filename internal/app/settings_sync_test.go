package app

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"foci/internal/fap"
	"foci/internal/platform"
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

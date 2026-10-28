package app

import (
	"path/filepath"
	"testing"

	"foci/internal/fap"
	"foci/internal/memory"
)

// withReminderStore opens a real reminder store, registers it for agentID with
// the given held predicate, and makes h the active hub, undoing both at
// cleanup.
func withReminderStore(t *testing.T, h *Hub, agentID string, held func(string) bool) *memory.ReminderStore {
	t.Helper()
	rs, err := memory.NewReminderStore(filepath.Join(t.TempDir(), "reminders.db"))
	if err != nil {
		t.Fatal(err)
	}
	setActiveHub(h)
	SetReminderStore(agentID, rs, held)
	t.Cleanup(func() {
		rs.SetOnChange(nil)
		reminderMu.Lock()
		delete(reminderSources, agentID)
		reminderMu.Unlock()
		setActiveHub(nil)
		_ = rs.Close()
	})
	return rs
}

// reminderLists returns the reminder lists of every Reminders frame queued on
// c, in order, as lists of (id, state) for compact comparison.
func reminderLists(t *testing.T, c *wsClient) [][]map[string]any {
	t.Helper()
	var out [][]map[string]any
	for _, d := range drain(t, c) {
		if d.t != fap.TypeReminders {
			continue
		}
		raw, ok := d.d["reminders"].([]any)
		if !ok {
			t.Fatalf("reminders frame without a list: %v", d.d)
		}
		list := []map[string]any{}
		for _, r := range raw {
			list = append(list, r.(map[string]any))
		}
		out = append(out, list)
	}
	return out
}

// A wake set from one chat's session shows on that chat only, follows the wake
// through fired/held to removal, and never touches another chat whose list did
// not change (#2600).
func TestReminders_WakeShowsOnItsOwnChatAndTracksState(t *testing.T) {
	h := newTestHub()
	held := false
	rs := withReminderStore(t, h, "ag", func(string) bool { return held })

	a := h.ensureBinding(nil, "ag", "conv-a")
	ca := fakeClient()
	a.attach(ca)
	b := h.ensureBinding(nil, "ag", "conv-b")
	cb := fakeClient()
	b.attach(cb)
	other := h.ensureBinding(nil, "other", "conv-o")
	co := fakeClient()
	other.attach(co)
	drain(t, ca)
	drain(t, cb)
	drain(t, co)

	id, err := rs.AddWake("ag", a.sessionKey, "check the build", "30m")
	if err != nil {
		t.Fatal(err)
	}
	got := reminderLists(t, ca)
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("chat A frames = %v, want one list with the wake", got)
	}
	if r := got[0][0]; r["text"] != "check the build" || r["state"] != memory.WakeStatePending || r["id"] != float64(id) {
		t.Errorf("chat A reminder = %v, want pending wake %d", r, id)
	}
	if dueAt, _ := got[0][0]["dueAtMs"].(float64); dueAt <= 0 {
		t.Errorf("dueAtMs = %v, want a positive unix ms", got[0][0]["dueAtMs"])
	}
	// Chat B's list was sent once (first push after start, empty) and the other
	// agent's chat never.
	if gb := reminderLists(t, cb); len(gb) != 1 || len(gb[0]) != 0 {
		t.Errorf("chat B frames = %v, want one empty list", gb)
	}
	if gotO := reminderLists(t, co); len(gotO) != 0 {
		t.Errorf("other agent's chat got %v, want nothing", gotO)
	}

	// A passive reminder is never shown and sends nothing.
	if err := rs.Add("ag", "passive thought", "now"); err != nil {
		t.Fatal(err)
	}
	if g := reminderLists(t, ca); len(g) != 0 {
		t.Errorf("passive reminder sent %v, want nothing", g)
	}

	// Fired, then held behind background work.
	rs.MarkWakeFired(id, a.sessionKey)
	if g := reminderLists(t, ca); len(g) != 1 || g[0][0]["state"] != memory.WakeStateFired {
		t.Fatalf("after fire = %v, want state fired", g)
	}
	held = true
	NotifyRemindersChanged("ag")
	if g := reminderLists(t, ca); len(g) != 1 || g[0][0]["state"] != memory.WakeStateHeld {
		t.Fatalf("while held = %v, want state held", g)
	}
	if g := reminderLists(t, cb); len(g) != 0 {
		t.Errorf("chat B got %v on A's change, want nothing (dedup)", g)
	}

	// The roster carries the same list, so a reconnecting device is reseeded.
	registerFakeAgent(h, "ag")
	for _, ag := range h.agentRoster() {
		for _, ci := range ag.Conversations {
			switch ci.ID {
			case "conv-a":
				if len(ci.Reminders) != 1 || ci.Reminders[0].ID != id {
					t.Errorf("roster conv-a reminders = %+v, want wake %d", ci.Reminders, id)
				}
			default:
				if len(ci.Reminders) != 0 {
					t.Errorf("roster %s reminders = %+v, want none", ci.ID, ci.Reminders)
				}
			}
		}
	}

	// The wake's turn runs: the row goes, and chat A's list empties.
	if err := rs.Dismiss(id); err != nil {
		t.Fatal(err)
	}
	if g := reminderLists(t, ca); len(g) != 1 || len(g[0]) != 0 {
		t.Fatalf("after dismiss = %v, want one empty list", g)
	}
}

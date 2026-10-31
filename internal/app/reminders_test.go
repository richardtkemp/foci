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
	// Chat B never had a wake, so its unchanged empty list is not sent, even on
	// the first push after a start (#2602: that write went to every chat of the
	// agent); the other agent's chat never.
	if gb := reminderLists(t, cb); len(gb) != 0 {
		t.Errorf("chat B frames = %v, want none (its list did not change)", gb)
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

// A wake set from a depth-mode leaf's session shows on the leaf's own
// conversation, not the root's, live and in the roster (#2646: "the pins and
// the remind wake must track the leaf not the root"). The leaf's binding holds
// the branch session key, which is the key the remind tool stores from a turn
// on that session.
func TestReminders_WakeFromALeafShowsOnTheLeaf(t *testing.T) {
	h := newTestHub()
	withDepthCtl(t)
	rs := withReminderStore(t, h, "ag", nil)
	client := fakeClientFor(h)
	h.addClient(client)
	root := h.ensureBinding(client, "ag", "conv-root")
	lb := registerLeaf(t, h, root, root.sessionKey+"/b1", "leaf-1", "Point A")
	drain(t, client)

	id, err := rs.AddWake("ag", lb.sessionKey, "check the leaf", "30m")
	if err != nil {
		t.Fatal(err)
	}
	byConv := map[string][][]any{}
	for _, d := range drain(t, client) {
		if d.t != fap.TypeReminders {
			continue
		}
		conv, _ := d.d["conversationId"].(string)
		list, _ := d.d["reminders"].([]any)
		byConv[conv] = append(byConv[conv], list)
	}
	leafFrames := byConv[lb.convID]
	if len(leafFrames) != 1 || len(leafFrames[0]) != 1 || leafFrames[0][0].(map[string]any)["text"] != "check the leaf" {
		t.Fatalf("leaf conversation frames = %v, want one list with the wake (all %v)", leafFrames, byConv)
	}
	for _, l := range byConv[root.convID] {
		if len(l) != 0 {
			t.Errorf("root conversation got %v, want the leaf's wake only on the leaf", l)
		}
	}

	registerFakeAgent(h, "ag")
	for _, ag := range h.agentRoster() {
		for _, ci := range ag.Conversations {
			switch ci.ID {
			case lb.convID:
				if len(ci.Reminders) != 1 || ci.Reminders[0].ID != id {
					t.Errorf("roster leaf reminders = %+v, want wake %d", ci.Reminders, id)
				}
			default:
				if len(ci.Reminders) != 0 {
					t.Errorf("roster %s reminders = %+v, want none", ci.ID, ci.Reminders)
				}
			}
		}
	}
}

// #2647: once a depth leaf has closed and handed over, its pending wake is
// listed on the node that owns it now (here the root), live and in the
// roster, and no longer on the leaf. The owner walk is the depth manager's
// (tools.WakeOwner), stubbed here.
func TestReminders_ClosedLeafWakeShowsOnItsNewOwner(t *testing.T) {
	h := newTestHub()
	withDepthCtl(t)
	rs := withReminderStore(t, h, "ag", nil)
	client := fakeClientFor(h)
	h.addClient(client)
	root := h.ensureBinding(client, "ag", "conv-root")
	lb := registerLeaf(t, h, root, root.sessionKey+"/b1", "leaf-1", "Point A")
	handedOver := false
	prev := wakeOwner
	wakeOwner = func(sk string) string {
		if handedOver && sk == lb.sessionKey {
			return root.sessionKey
		}
		return sk
	}
	t.Cleanup(func() { wakeOwner = prev })
	drain(t, client)

	id, err := rs.AddWake("ag", lb.sessionKey, "check the leaf", "30m")
	if err != nil {
		t.Fatal(err)
	}
	drain(t, client)

	// The leaf hands over (the gateway's OnHandover pushes the lists).
	handedOver = true
	NotifyRemindersChanged("ag")
	byConv := map[string][][]any{}
	for _, d := range drain(t, client) {
		if d.t != fap.TypeReminders {
			continue
		}
		conv, _ := d.d["conversationId"].(string)
		list, _ := d.d["reminders"].([]any)
		byConv[conv] = append(byConv[conv], list)
	}
	if rf := byConv[root.convID]; len(rf) != 1 || len(rf[0]) != 1 || rf[0][0].(map[string]any)["text"] != "check the leaf" {
		t.Fatalf("root frames = %v, want one list with the leaf's wake (all %v)", rf, byConv)
	}
	if lf := byConv[lb.convID]; len(lf) != 1 || len(lf[0]) != 0 {
		t.Fatalf("leaf frames = %v, want one empty list", lf)
	}

	registerFakeAgent(h, "ag")
	for _, ag := range h.agentRoster() {
		for _, ci := range ag.Conversations {
			switch ci.ID {
			case root.convID:
				if len(ci.Reminders) != 1 || ci.Reminders[0].ID != id {
					t.Errorf("roster root reminders = %+v, want wake %d", ci.Reminders, id)
				}
			default:
				if len(ci.Reminders) != 0 {
					t.Errorf("roster %s reminders = %+v, want none", ci.ID, ci.Reminders)
				}
			}
		}
	}
}

// After a restart the push cache is empty, but a device's copy comes from the
// roster it got on hello. A chat whose wake the roster already advertised is
// not sent the same list again when another chat's wakes change (#2602).
func TestReminders_RosterSeedsThePushCache(t *testing.T) {
	h := newTestHub()
	rs := withReminderStore(t, h, "ag", nil)
	registerFakeAgent(h, "ag")

	a := h.ensureBinding(nil, "ag", "conv-a")
	b := h.ensureBinding(nil, "ag", "conv-b")
	// A wake that was pending before the "restart": set with the hook off, so
	// no frame records it in the cache.
	rs.SetOnChange(nil)
	idA, err := rs.AddWake("ag", a.sessionKey, "pending across restart", "30m")
	if err != nil {
		t.Fatal(err)
	}
	rs.SetOnChange(func() { NotifyRemindersChanged("ag") })

	// The device reconnects: the roster advertises chat A's wake.
	_ = h.agentRoster()
	ca := fakeClient()
	a.attach(ca)
	cb := fakeClient()
	b.attach(cb)
	drain(t, ca)
	drain(t, cb)

	// A change on chat B only.
	if _, err := rs.AddWake("ag", b.sessionKey, "on b", "1h"); err != nil {
		t.Fatal(err)
	}
	if g := reminderLists(t, ca); len(g) != 0 {
		t.Errorf("chat A frames = %v, want none (the roster already sent its list)", g)
	}
	if g := reminderLists(t, cb); len(g) != 1 || len(g[0]) != 1 {
		t.Errorf("chat B frames = %v, want one list with its wake", g)
	}

	// Chat A's own change still goes out.
	if err := rs.Dismiss(idA); err != nil {
		t.Fatal(err)
	}
	if g := reminderLists(t, ca); len(g) != 1 || len(g[0]) != 0 {
		t.Errorf("after dismiss chat A frames = %v, want one empty list", g)
	}
}

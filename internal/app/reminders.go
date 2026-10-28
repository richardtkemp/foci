package app

import (
	"slices"
	"sync"

	"foci/internal/fap"
	"foci/internal/memory"
)

// Per-chat pending wake reminders (#2600). The app shows an indicator on a
// conversation while a wake reminder (foci_remind wake=true) set from its
// session is pending, with the list in a details dialog. Only wakes: a passive
// reminder belongs to the agent, not to a chat, and is never shown (Dick,
// 2026-10-28).
//
// Flow: the gateway registers each agent's reminder store here
// (SetReminderStore), which wires the store's change hook to push a fresh
// Reminders frame to every conversation of that agent whose list changed. The
// roster reads the same source, so a reconnecting client is reseeded.

// reminderSource lists the pending wakes set from one session.
type reminderSource func(sessionKey string) []fap.PendingReminder

var (
	reminderMu      sync.RWMutex
	reminderSources = map[string]reminderSource{} // agentID → source

	// reminderPushMu serialises pushReminders, so two changes racing from
	// different goroutines (a timer firing while a tool sets a wake) cannot
	// send their lists out of order.
	reminderPushMu sync.Mutex
)

// SetReminderStore registers agentID's reminder store as the source of its
// chats' reminder indicators and wires the store's change hook to push
// updates. held reports whether a session's system input is held behind
// background work (Agent.SystemInputHeld), which splits a fired wake into
// "held" and "fired"; nil reads every fired wake as "fired". A nil store is a
// no-op.
func SetReminderStore(agentID string, rs *memory.ReminderStore, held func(sessionKey string) bool) {
	if rs == nil {
		return
	}
	src := func(sessionKey string) []fap.PendingReminder {
		return pendingWakesFor(rs, agentID, sessionKey, held)
	}
	reminderMu.Lock()
	reminderSources[agentID] = src
	reminderMu.Unlock()
	rs.SetOnChange(func() { NotifyRemindersChanged(agentID) })
}

// pendingWakesFor converts the store's pending wakes set from sessionKey to
// wire form, ordered by due time. A wake with no session key (a legacy row)
// matches no chat. Never nil, so an empty list encodes as [].
func pendingWakesFor(rs *memory.ReminderStore, agentID, sessionKey string, held func(string) bool) []fap.PendingReminder {
	out := []fap.PendingReminder{}
	if sessionKey == "" {
		return out
	}
	wakes, err := rs.PendingWakes(agentID)
	if err != nil {
		appLog.Errorf("list pending wakes for %s: %v", agentID, err)
		return out
	}
	for _, r := range wakes {
		if r.SessionKey != sessionKey {
			continue
		}
		var due int64
		if !r.DueAt.IsZero() {
			due = r.DueAt.UnixMilli()
		}
		out = append(out, fap.PendingReminder{ID: r.ID, Text: r.Text, DueAtMs: due, State: rs.WakeState(r.ID, held)})
	}
	return out
}

// remindersFor returns the pending wakes for one conversation's session, or
// nil when the agent has no registered store.
func remindersFor(agentID, sessionKey string) []fap.PendingReminder {
	reminderMu.RLock()
	src := reminderSources[agentID]
	reminderMu.RUnlock()
	if src == nil {
		return nil
	}
	return src(sessionKey)
}

// NotifyRemindersChanged re-reads agentID's pending wakes and pushes a
// Reminders frame to each of its conversations whose list changed. No-op when
// the app provider is not running.
func NotifyRemindersChanged(agentID string) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	h.pushReminders(agentID)
}

// pushReminders refreshes the reminder list of every ordinary (non-leaf)
// conversation bound to agentID. A depth-mode leaf view is skipped: it has no
// indicator of its own, and its frames forward to the root.
func (h *Hub) pushReminders(agentID string) {
	reminderPushMu.Lock()
	defer reminderPushMu.Unlock()
	h.mu.RLock()
	var bindings []*convBinding
	for _, b := range h.convs {
		if b.agentID == agentID && b.leaf == nil {
			bindings = append(bindings, b)
		}
	}
	h.mu.RUnlock()
	for _, b := range bindings {
		b.setReminders(remindersFor(agentID, b.sessionKey))
	}
}

// setReminders records the conversation's pending wake list and sends a
// Reminders frame when it differs from the last one sent (dedup, so a change
// on one chat does not write a frame into every chat of the agent). The first
// call after a restart always sends, because the cache starts unset and the
// client's copy (from the last roster) is unknown here.
func (b *convBinding) setReminders(list []fap.PendingReminder) {
	if list == nil {
		list = []fap.PendingReminder{}
	}
	b.mu.Lock()
	changed := !b.remindersSent || !slices.Equal(list, b.reminders)
	if changed {
		b.reminders = list
		b.remindersSent = true
	}
	convID := b.convID
	b.mu.Unlock()
	if changed {
		b.send(fap.Reminders{ConversationID: convID, Reminders: list})
	}
}

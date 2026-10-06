package agent

import (
	"encoding/json"

	"foci/internal/delegator"
	"foci/internal/session"
)

// installDeliveryHooks wires a DeliveryTracker backend's reports to the
// session's persisted undelivered set, the consumed-input callback (the app's
// ✓✓) and redelivery. Backends that cannot confirm consumption are left alone:
// their inputs are never persisted, redelivered or reported consumed.
// The undelivered set lives in session_metadata under
// session.MetaKeyCCUndelivered (see the registry in session/metadata_keys.go).
func (m *DelegatedManager) installDeliveryHooks(mb *managedBackend, sk func() string) {
	dt, ok := delegator.As[delegator.DeliveryTracker](mb.be)
	if !ok {
		return
	}
	dt.SetDeliveryHooks(delegator.DeliveryHooks{
		OnPending: func(p delegator.PendingInput) {
			m.upsertUndelivered(sk(), p)
		},
		OnConsumed: func(p delegator.PendingInput) {
			key := sk()
			m.removeUndelivered(key, p.ID)
			if len(p.Refs) > 0 && m.InputConsumed != nil {
				m.InputConsumed(key, p.Refs)
			}
		},
		OnDropped: func(p delegator.PendingInput) {
			m.removeUndelivered(sk(), p.ID)
		},
		OnUndelivered: func(ps []delegator.PendingInput) {
			key := sk()
			if mb.dropUndelivered.Load() {
				m.logger().Infof("session %s reset with %d unconsumed input(s) — dropping them", key, len(ps))
				for _, p := range ps {
					m.removeUndelivered(key, p.ID)
				}
				return
			}
			for _, p := range ps {
				m.upsertUndelivered(key, p) // records the bumped redelivery count
				m.redeliver(key, p, "cc-exit")
			}
		},
	})
}

// redeliver hands one undelivered input to the Redeliver hook, which re-sends
// it as a fresh turn. Without a hook the input stays persisted and is retried
// by the next RestoreUndelivered.
func (m *DelegatedManager) redeliver(sessionKey string, p delegator.PendingInput, reason string) {
	if m.Redeliver == nil {
		m.logger().Warnf("session %s: no redelivery hook — input %s (%s) left persisted for the next restart", sessionKey, p.ID, p.Source)
		return
	}
	m.logger().Infof("session %s: redelivering input %s (%s, reason=%s, attempt %d)", sessionKey, p.ID, p.Source, reason, p.Redeliveries)
	m.Redeliver(sessionKey, Redelivery{Input: p, Reason: reason})
}

// stillUndelivered reports whether input id is still awaiting delivery on
// sessionKey. A redelivery turn checks it before writing: a /reset since the
// input was handed back dropped it, and it must not reach the fresh session.
// With no session index there is no persisted set to consult, so every
// redelivery proceeds.
func (m *DelegatedManager) stillUndelivered(sessionKey, id string) bool {
	if m.SessionIndex == nil {
		return true
	}
	m.deliveryMu.Lock()
	defer m.deliveryMu.Unlock()
	for _, p := range m.loadUndeliveredLocked(sessionKey) {
		if p.ID == id {
			return true
		}
	}
	return false
}

// RestoreUndelivered re-sends, after a gateway restart, every persisted input
// of this agent's sessions that no backend confirmed consuming. Each is first
// checked against the transcript it was written to: one found there was folded
// in before the old process went away and is only marked consumed. Called once
// at startup, after inboxes and platforms are up.
func (m *DelegatedManager) RestoreUndelivered() {
	if m.SessionIndex == nil {
		return
	}
	keys, err := m.SessionIndex.SessionKeysWithMetadata(session.MetaKeyCCUndelivered)
	if err != nil {
		m.logger().Warnf("restore undelivered inputs: %v", err)
		return
	}
	for _, sk := range keys {
		if parsed, err := session.ParseSessionKey(sk); err != nil || parsed.AgentID != m.AgentID {
			continue
		}
		m.deliveryMu.Lock()
		pending := m.loadUndeliveredLocked(sk)
		m.deliveryMu.Unlock()
		for _, p := range pending {
			m.restoreOne(sk, p)
		}
	}
}

func (m *DelegatedManager) restoreOne(sk string, p delegator.PendingInput) {
	sid := p.SessionID
	if sid == "" {
		sid = m.loadResumeID(sk)
	}
	// A Spec without a TranscriptChecker redelivers every persisted input
	// (at-least-once).
	if check := m.Spec.TranscriptChecker; check != nil && sid != "" && p.WorkDir != "" {
		found, err := check(p.WorkDir, sid, p.ID)
		switch {
		case err != nil:
			m.logger().Warnf("session %s: input %s transcript unreadable (%v) — redelivering (at-least-once)", sk, p.ID, err)
		case found:
			m.logger().Infof("session %s: input %s was folded in before the restart — not redelivering", sk, p.ID)
			m.removeUndelivered(sk, p.ID)
			if len(p.Refs) > 0 && m.InputConsumed != nil {
				m.InputConsumed(sk, p.Refs)
			}
			return
		}
	}
	if p.Redeliveries >= delegator.MaxRedeliveries {
		m.logger().Errorf("session %s: input %s never consumed after %d redeliveries — dropping it: %.80q", sk, p.ID, p.Redeliveries, p.Text)
		m.removeUndelivered(sk, p.ID)
		return
	}
	p.Redeliveries++
	m.upsertUndelivered(sk, p)
	m.redeliver(sk, p, "restart")
}

// clearUndelivered drops every persisted input of sessionKey (/reset).
func (m *DelegatedManager) clearUndelivered(sessionKey string) {
	if m.SessionIndex == nil {
		return
	}
	m.deliveryMu.Lock()
	defer m.deliveryMu.Unlock()
	if len(m.loadUndeliveredLocked(sessionKey)) == 0 {
		return
	}
	if err := m.SessionIndex.DeleteSessionMetadata(sessionKey, session.MetaKeyCCUndelivered); err != nil {
		m.logger().Warnf("clear undelivered inputs for %s: %v", sessionKey, err)
	}
}

// moveUndelivered re-keys a session's persisted inputs (RemapSession).
func (m *DelegatedManager) moveUndelivered(oldKey, newKey string) {
	if m.SessionIndex == nil {
		return
	}
	m.deliveryMu.Lock()
	defer m.deliveryMu.Unlock()
	moved := m.loadUndeliveredLocked(oldKey)
	if len(moved) == 0 {
		return
	}
	m.saveUndeliveredLocked(newKey, append(m.loadUndeliveredLocked(newKey), moved...))
	m.saveUndeliveredLocked(oldKey, nil)
}

// upsertUndelivered records p, replacing any entry with the same id (a
// redelivery re-writes its input under the original id).
func (m *DelegatedManager) upsertUndelivered(sessionKey string, p delegator.PendingInput) {
	if m.SessionIndex == nil {
		return
	}
	m.deliveryMu.Lock()
	defer m.deliveryMu.Unlock()
	list := m.loadUndeliveredLocked(sessionKey)
	for i := range list {
		if list[i].ID == p.ID {
			list[i] = p
			m.saveUndeliveredLocked(sessionKey, list)
			return
		}
	}
	m.saveUndeliveredLocked(sessionKey, append(list, p))
}

// removeUndelivered forgets input id.
func (m *DelegatedManager) removeUndelivered(sessionKey, id string) {
	if m.SessionIndex == nil {
		return
	}
	m.deliveryMu.Lock()
	defer m.deliveryMu.Unlock()
	list := m.loadUndeliveredLocked(sessionKey)
	for i := range list {
		if list[i].ID == id {
			m.saveUndeliveredLocked(sessionKey, append(list[:i], list[i+1:]...))
			return
		}
	}
}

func (m *DelegatedManager) loadUndeliveredLocked(sessionKey string) []delegator.PendingInput {
	raw, err := m.SessionIndex.GetSessionMetadata(sessionKey, session.MetaKeyCCUndelivered)
	if err != nil {
		m.logger().Warnf("load undelivered inputs for %s: %v", sessionKey, err)
		return nil
	}
	if raw == "" {
		return nil
	}
	var list []delegator.PendingInput
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		m.logger().Warnf("undelivered inputs for %s unreadable, discarding: %v", sessionKey, err)
		return nil
	}
	return list
}

func (m *DelegatedManager) saveUndeliveredLocked(sessionKey string, list []delegator.PendingInput) {
	if len(list) == 0 {
		if err := m.SessionIndex.DeleteSessionMetadata(sessionKey, session.MetaKeyCCUndelivered); err != nil {
			m.logger().Warnf("clear undelivered inputs for %s: %v", sessionKey, err)
		}
		return
	}
	raw, err := json.Marshal(list)
	if err != nil {
		m.logger().Warnf("encode undelivered inputs for %s: %v", sessionKey, err)
		return
	}
	if err := m.SessionIndex.SetSessionMetadata(sessionKey, session.MetaKeyCCUndelivered, string(raw)); err != nil {
		m.logger().Warnf("persist undelivered inputs for %s: %v", sessionKey, err)
	}
}

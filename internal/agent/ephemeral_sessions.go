package agent

import (
	"errors"
	"foci/internal/delegator"
	"foci/internal/session"
)

// ErrEphemeralSessionLost prevents a worker silently continuing without its context.
var ErrEphemeralSessionLost = errors.New("ephemeral backend conversation is no longer available; start a new worker")

// markEphemeralSession records lifetime durably, but retains its live ID only in memory.
// Backends without native ephemeral threads retain their existing transcript lifecycle.
func (m *DelegatedManager) markEphemeralSession(key, id string) error {
	if !m.Spec.Supports(delegator.CapEphemeralSessions) {
		return nil
	}
	if m.SessionIndex != nil {
		if err := m.SessionIndex.SetSessionMetadata(key, session.MetaKeyBackendEphemeral, "true"); err != nil {
			return err
		}
		if err := m.SessionIndex.DeleteSessionMetadata(key, session.MetaKeyCCResumeID); err != nil {
			return err
		}
	}
	m.ephemeralMu.Lock()
	defer m.ephemeralMu.Unlock()
	if m.ephemeralIDs == nil {
		m.ephemeralIDs = make(map[string]string)
	}
	m.ephemeralIDs[key] = id
	return nil
}

func (m *DelegatedManager) ephemeralID(key string) (string, bool) {
	m.ephemeralMu.Lock()
	defer m.ephemeralMu.Unlock()
	id, ok := m.ephemeralIDs[key]
	return id, ok
}

func (m *DelegatedManager) hasEphemeralSession(key string) bool {
	_, ok := m.ephemeralID(key)
	return ok
}

func (m *DelegatedManager) isEphemeralSession(key string) bool {
	if m.hasEphemeralSession(key) {
		return true
	}
	if m.SessionIndex == nil {
		return false
	}
	v, _ := m.SessionIndex.GetSessionMetadata(key, session.MetaKeyBackendEphemeral)
	return v == "true"
}

// saveEphemeralID never writes a live-only ID as a durable resume promise.
func (m *DelegatedManager) saveEphemeralID(key, id string) bool {
	if !m.isEphemeralSession(key) {
		return false
	}
	m.ephemeralMu.Lock()
	if _, ok := m.ephemeralIDs[key]; ok && id != "" {
		m.ephemeralIDs[key] = id
	}
	m.ephemeralMu.Unlock()
	return true
}

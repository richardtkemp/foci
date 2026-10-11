package nudge

import (
	"foci/internal/session"
	"sync"
)

// MuteKey is the chat_metadata key holding the set of nudge ids a chat has
// muted ("Stop showing me this"). The value is a JSON string array encoded by
// the shared session chat-set codec: sorted, de-duplicated.
const MuteKey = "nudge_muted"

// muteStore persists per-chat nudge mutes in chat_metadata under MuteKey.
//
// Keying rule (F1, #1321 round 3): a mute is keyed by (agent, chatID) ALONE —
// the index's platform-agnostic bucket (platform="") — never by the chat's
// resolved platform. Read and write both use that one key, so the mute lands
// under exactly the key Send later checks whether or not the chat's
// "registered" platform row exists yet, and it survives that row appearing
// later: a mute pressed before the platform row is written is not lost the
// way a resolved-platform key would be. (agent, chatID) is also the unit
// session keys themselves use (NewChatSessionKey), so per-chat == per-session.
// A new chat on the same platform sees the nudge once more — per chat is the
// stable unit the index offers.
type muteStore struct {
	idx *session.SessionIndex

	// mu serialises the read-modify-write in mute so two simultaneous mutes
	// both survive (like Hub.pinsMu for pins). muted takes no lock: it is a
	// single-row read, already serialised by the index's own mutex.
	mu sync.Mutex
}

func newMuteStore(idx *session.SessionIndex) *muteStore {
	return &muteStore{idx: idx}
}

// muted reports whether the chat has muted nudgeID. An unreadable stored
// value is treated as empty (not muted) by DecodeChatSet.
func (m *muteStore) muted(agentID string, chatID int64, nudgeID string) (bool, error) {
	stored, err := m.idx.GetChatMetadata(agentID, "", chatID, MuteKey)
	if err != nil {
		return false, err
	}
	for _, id := range session.DecodeChatSet(stored) {
		if id == nudgeID {
			return true, nil
		}
	}
	return false, nil
}

// mute adds nudgeID to the chat's muted set. The stored set is rewritten
// sorted and de-duplicated; an unreadable stored value is treated as empty
// and overwritten.
func (m *muteStore) mute(agentID string, chatID int64, nudgeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, err := m.idx.GetChatMetadata(agentID, "", chatID, MuteKey)
	if err != nil {
		return err
	}
	ids := append(session.DecodeChatSet(stored), nudgeID)
	return m.idx.SetChatMetadata(agentID, "", chatID, MuteKey, session.EncodeChatSet(ids))
}

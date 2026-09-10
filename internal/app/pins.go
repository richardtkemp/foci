package app

import (
	"encoding/json"
	"sort"
)

// chatMetaPins is the chat_metadata key holding a conversation's pinned-message
// set, alongside the existing "draft" and "last_read" keys. The value is a JSON
// array of message IDs — a set, stored sorted so an unchanged set round-trips to
// an identical string.
//
// This is MESSAGE pin (#893's bubble bookmark, surfaced in the media view's
// Pinned tab). The roster's CONVERSATION pin is a separate, deliberately
// per-device preference and is not stored here (#1882).
const chatMetaPins = "pins"

// decodePinSet parses a stored pins value. It never returns nil: an absent key,
// an empty value or unparseable JSON all yield an empty (non-nil) slice, so the
// encoded frame carries `[]` rather than `null` — the Kotlin PinSync field is a
// non-nullable List and would fail to decode a null.
//
// Unparseable input degrading to "nothing pinned" is the right failure: the set
// is a convenience bookmark, and a device applying an empty set is recoverable
// by re-pinning, whereas refusing to decode would wedge the whole replay.
func decodePinSet(stored string) []string {
	out := []string{}
	if stored == "" {
		return out
	}
	var ids []string
	if err := json.Unmarshal([]byte(stored), &ids); err != nil {
		appLog.Warnf("pins: unparseable stored set %q, treating as empty: %v", stored, err)
		return out
	}
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return normalisePinSet(out)
}

// encodePinSet renders a pin set for storage. The inverse of decodePinSet.
func encodePinSet(ids []string) string {
	b, err := json.Marshal(normalisePinSet(ids))
	if err != nil {
		// Marshalling []string cannot fail; keep the arm honest rather than silent.
		appLog.Warnf("pins: encode %d ids: %v", len(ids), err)
		return ""
	}
	return string(b)
}

// applyPin folds one message's new pinned state into a set, returning the new
// set. Idempotent: pinning an already-pinned message, or unpinning one that is
// not in the set, returns an equivalent set. Never returns nil.
func applyPin(ids []string, messageID string, pinned bool) []string {
	out := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		if id != messageID {
			out = append(out, id)
		}
	}
	if pinned {
		out = append(out, messageID)
	}
	return normalisePinSet(out)
}

// normalisePinSet sorts and de-duplicates, so the stored string is a canonical
// rendering of the set and two equal sets never differ on the wire.
func normalisePinSet(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

package session

import (
	"encoding/json"
	"sort"
)

// Chat-set codec: the shared encode/decode pair for chat_metadata values that
// hold a SET of ids (app message pins under "pins", nudge mutes under
// "nudge_muted"). Both consumers need identical storage semantics — sorted,
// de-duplicated, `[]`-never-`null`, garbage degrading to empty — so the codec
// lives here once, next to the chat_metadata table it renders for.

// NormaliseChatSet sorts and de-duplicates and drops empty ids, so a stored
// value is a canonical rendering of the set: two equal sets never differ on
// the wire, and an unchanged set round-trips to an identical string. Never
// returns nil.
func NormaliseChatSet(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// DecodeChatSet parses a stored chat-set value. It never returns nil: an
// absent key, an empty value or unparseable JSON all yield an empty (non-nil)
// slice, so the value re-encodes as `[]` rather than `null` (a JSON null fails
// consumers with non-nullable list fields, e.g. the app's Kotlin PinSync).
// The result is normalised on the way out — unsorted input decodes sorted and
// duplicates collapse — so a hand-edited or legacy value cannot leak an
// uncanonical form back into storage on the next write.
//
// Unparseable input degrading to "empty set" is the right failure for these
// sets: they are convenience state (bookmarks, mutes), and an unreadable value
// is recoverable by re-adding entries, whereas refusing to decode would wedge
// every reader of the row.
func DecodeChatSet(stored string) []string {
	out := []string{}
	if stored == "" {
		return out
	}
	var ids []string
	if err := json.Unmarshal([]byte(stored), &ids); err != nil {
		sessLog.Warnf("chatset: unparseable stored set %q, treating as empty: %v", stored, err)
		return out
	}
	return NormaliseChatSet(ids)
}

// EncodeChatSet renders a chat set for storage. The inverse of DecodeChatSet.
func EncodeChatSet(ids []string) string {
	b, err := json.Marshal(NormaliseChatSet(ids))
	if err != nil {
		// Marshalling []string cannot fail; keep the arm honest rather than silent.
		sessLog.Warnf("chatset: encode %d ids: %v", len(ids), err)
		return ""
	}
	return string(b)
}

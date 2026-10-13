package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"foci/internal/fap"
)

// Checkbox-put validation (#1360) — the protocol contract with the client,
// so it stays constants rather than config. A put that breaks one is logged at
// WARN and dropped: not stored, not fanned out, no reply (the neighbouring
// mirrors' refused-put shape — a misbehaving client cannot be helped by an
// error it will not read).
const (
	checkboxKeyMaxLen    = 128    // document key: 1–128 chars of [A-Za-z0-9._-]
	checkboxLineIndexMax = 100000 // line indexes are 0–100000
	checkboxMaxEntries   = 2000   // max entries (checked lines) per document
)

// validateCheckboxPut enforces a checkbox.put's wire limits. The error names
// the first violation so the WARN it becomes is diagnosable.
func validateCheckboxPut(key string, lines []int) error {
	if len(key) == 0 || len(key) > checkboxKeyMaxLen {
		return fmt.Errorf("document key must be 1-%d characters, got %d", checkboxKeyMaxLen, len(key))
	}
	for _, c := range key {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("document key %q has character %q outside [A-Za-z0-9._-]", key, c)
		}
	}
	if len(lines) > checkboxMaxEntries {
		return fmt.Errorf("%d entries exceeds the %d-entry limit", len(lines), checkboxMaxEntries)
	}
	for _, l := range lines {
		if l < 0 || l > checkboxLineIndexMax {
			return fmt.Errorf("line index %d outside 0-%d", l, checkboxLineIndexMax)
		}
	}
	return nil
}

// normaliseCheckboxLines returns lines sorted, de-duplicated and non-nil, so a
// state is deterministic wherever it goes (the pins.go discipline): the same
// set sent in any order encodes to one stored string, fans out as one frame,
// and compares by value under last-write-wins. Non-nil matters on the wire
// too: an empty state must encode as "lines":[], never null.
func normaliseCheckboxLines(lines []int) []int {
	out := slices.Clone(lines)
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		return []int{}
	}
	return out
}

// encodeCheckboxState renders a whole tick state for storage: the JSON array
// of checked line indexes (e.g. "[3,7]"). An EMPTY state encodes as the empty
// string — the stored form of a clear, which checkboxStore.put maps to a
// delete — so no row ever holds an empty state. The inverse is
// decodeCheckboxState, which reports ok=false for exactly that form.
func encodeCheckboxState(lines []int) string {
	if len(lines) == 0 {
		return ""
	}
	b, err := json.Marshal(lines)
	if err != nil {
		// A []int cannot fail to marshal; reaching here is a programming error.
		appLog.Errorf("encode checkbox state: %v", err)
		return ""
	}
	return string(b)
}

// decodeCheckboxState parses a stored state. ok is false for an empty,
// unreadable or null value — no writer of ours can produce one (a clear is
// stored as a delete, never a row), so it means a corrupt row (finding B),
// which the replay skips rather than send as a state. A value that decodes but
// breaks the put limits (an out-of-range index, an oversized list) is the
// caller's to reject with validateCheckboxPut — decode alone cannot know.
func decodeCheckboxState(state string) (lines []int, ok bool) {
	if state == "" {
		return nil, false
	}
	var out []int
	if err := json.Unmarshal([]byte(state), &out); err != nil || out == nil {
		return nil, false
	}
	return out, true
}

// handleCheckboxPut persists one document's whole checkbox tick state and
// mirrors it to the user's other devices — the draft mirror's shape (#1360):
// validate, store last-write-wins by arrival, broadcastExcept. Conflict
// resolution between two devices ticking the same list lives on the clients,
// as it does for drafts. Fire-and-forget: CheckboxPut is not
// conversation-reliability-scoped (see inboundConvID), so there is no ack.
//
// The one deliberate divergence from the draft/scroll handlers is the failure
// policy (review finding A): a put whose persist FAILED is not fanned out.
// Fanning it out would put the new state on the live devices while every later
// hello replays the older STORED state — rolling back every device that
// reconnects, including the one that ticked. Instead the sender gets a
// retryable error (the rename/archive idiom) so its UI keeps the local state
// and retries; the stored older state stays what replays.
func (h *Hub) handleCheckboxPut(client *wsClient, f fap.CheckboxPut) {
	if err := validateCheckboxPut(f.DocumentID, f.Lines); err != nil {
		appLog.Warnf("checkbox.put %q from device %s: %v", f.DocumentID, client.device(), err)
		return
	}
	lines := normaliseCheckboxLines(f.Lines)
	if h.checkboxes != nil {
		switch err := h.checkboxes.put(f.DocumentID, encodeCheckboxState(lines), time.Now().UnixMilli()); {
		case err == nil:
		case errors.Is(err, errCheckboxCap):
			// A full store is not something the sender can fix by retrying:
			// drop with a WARN naming device and key, and stay un-mirrored
			// until the sweep frees room.
			appLog.Warnf("checkbox.put %q from device %s: %v", f.DocumentID, client.device(), err)
			return
		default:
			appLog.Warnf("checkbox.put %q from device %s: persist: %v", f.DocumentID, client.device(), err)
			client.sendRaw(fap.ErrorFrame{
				Code:      "checkbox_put_failed",
				Message:   "Couldn't save the checkbox state for " + f.DocumentID + ".",
				Retryable: true,
			})
			return
		}
	}
	// Reaching here means the state is stored — or this hub has NO store (no
	// data_dir, or its open failed): such a put still fans out live, but it is
	// not persisted and not replayed after reconnect.
	h.broadcastExcept(client, fap.CheckboxSync{DocumentID: f.DocumentID, Lines: lines})
}

// pushCheckboxes replays each stored document's checkbox state to a
// just-connected client, most recently updated first, so a device that was
// offline while the user ticked on another device catches up (#1360).
//
// Deliberately NOT pushChatScalar — the one mirror whose replay is not: a
// document is not a conversation, there is no binding to walk, and the state
// lives in its own store (checkbox_store.go) with the updated_ms the ordering
// needs. Bounded by the hub's resolved replay knobs ([platforms.app]
// checkbox_replay_docs / checkbox_replay_ttl; zero fields mean the code
// defaults — the trimBufferLocked discipline — so a directly-constructed hub
// (tests) is bounded the same as a configured one). A corrupt row is skipped,
// not sent and not fatal (finding B; it still spent one of the SQL limit's
// slots — see checkboxStore.replay): corrupt means the state does not decode,
// is null, or breaks validateCheckboxPut — the write path's own limits gate
// the read path too, so the replay can never send a device what a put could
// not have stored (a `null` row is NOT a clear; a clear is an absent row).
//
// The burst goes through enqueue, which blocks (then closes the socket) rather
// than drop a frame — acceptable here for the same reason as every other
// hello-time push: the socket's writePump drains the queue concurrently, so
// the frames flow as fast as the socket writes, and only a truly stalled
// client hits the block, whose close-forces-resume is the designed heal. The
// replay bounds are what keep that burst finite.
func (h *Hub) pushCheckboxes(client *wsClient) {
	docs, age := h.checkboxReplayDocs, h.checkboxReplayAge
	if docs <= 0 {
		docs = defaultCheckboxReplayDocs
	}
	if age <= 0 {
		age = defaultCheckboxReplayTTL
	}
	cutoff := time.Now().Add(-age).UnixMilli()
	for _, doc := range h.checkboxes.replay(docs, cutoff) {
		lines, ok := decodeCheckboxState(doc.state)
		if !ok || validateCheckboxPut(doc.key, lines) != nil {
			continue
		}
		client.sendRaw(fap.CheckboxSync{DocumentID: doc.key, Lines: normaliseCheckboxLines(lines)})
	}
}

package app

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"foci/internal/fap"
	"foci/internal/session"
)

// chatMetaPins is the chat_metadata key holding a conversation's pinned-message
// set, alongside the existing "draft" and "last_read" keys. The value is a JSON
// array of message IDs — a set, stored sorted so an unchanged set round-trips to
// an identical string (encode/decode via the shared session chat-set codec).
//
// This is MESSAGE pin (#893's bubble bookmark, surfaced in the media view's
// Pinned tab). The roster's CONVERSATION pin is a separate scalar mirror under
// chatMetaConvPinned (convpin_sync.go, #2313).
const chatMetaPins = "pins"

// chatMetaPinMeta is the chat_metadata key holding what the server knows about
// each pin beyond its membership in chatMetaPins (#2670): the label the user
// gave it, and a snapshot of the message (snippet + send time) taken at pin
// time. A JSON object keyed by message id. It is a separate key so the "pins"
// set keeps its old format: a pin stored before labels existed has no entry
// here and is simply a pin with no label.
//
// chatMetaPins stays the authority on WHICH messages are pinned. An entry here
// for a message not in the set is stale and is pruned on the next write.
const chatMetaPinMeta = "pin_meta"

// maxPinLabelRunes caps a stored label. The app's dialog is a single short
// line; the cap only stops an oversized frame from bloating every PinSync.
const maxPinLabelRunes = 200

// pinMeta is one pin's entry under chatMetaPinMeta.
type pinMeta struct {
	Label string `json:"label,omitempty"`
	// Snippet and SentMs are the message as the frame store held it when it
	// was pinned. The All pins view prefers a live frame-store lookup; this
	// snapshot keeps a pin listable after the frame store's TTL trims the
	// message's frames.
	Snippet string `json:"snippet,omitempty"`
	SentMs  int64  `json:"sentMs,omitempty"`
}

// applyPin folds one message's new pinned state into a set, returning the new
// set. Idempotent: pinning an already-pinned message, or unpinning one that
// is not in the set, returns an equivalent set. Never returns nil.
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
	return session.NormaliseChatSet(out)
}

// normalisePinLabel trims a label and caps its length. "" means no label.
func normalisePinLabel(label string) string {
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) > maxPinLabelRunes {
		label = strings.TrimSpace(string([]rune(label)[:maxPinLabelRunes]))
	}
	return label
}

// decodePinMeta parses a stored chatMetaPinMeta value. Never nil: an absent or
// unparseable value is an empty map. Garbage degrades to "no labels" for the
// same reason DecodeChatSet degrades to "no pins": the pins stay pinned, and
// refusing to decode would wedge every pin write in the chat.
func decodePinMeta(stored string) map[string]pinMeta {
	out := map[string]pinMeta{}
	if stored == "" {
		return out
	}
	if err := json.Unmarshal([]byte(stored), &out); err != nil {
		appLog.Warnf("pin meta: unparseable stored value %q, treating as empty: %v", stored, err)
		return map[string]pinMeta{}
	}
	return out
}

// encodePinMeta renders a chatMetaPinMeta value. encoding/json sorts map keys,
// so an unchanged map round-trips to an identical string.
func encodePinMeta(m map[string]pinMeta) string {
	b, err := json.Marshal(m)
	if err != nil {
		// Marshalling map[string]pinMeta cannot fail; keep the arm honest.
		appLog.Warnf("pin meta: encode %d entries: %v", len(m), err)
		return "{}"
	}
	return string(b)
}

// prunePinMeta drops every entry whose message is not in the pinned set ids,
// and reports whether it dropped any.
func prunePinMeta(meta map[string]pinMeta, ids []string) bool {
	in := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		in[id] = struct{}{}
	}
	changed := false
	for id := range meta {
		if _, ok := in[id]; !ok {
			delete(meta, id)
			changed = true
		}
	}
	return changed
}

// pinLabels is the PinSync.Labels map for a pinned set: the label of each
// pinned message that has one. nil when no pin is labelled, so the field is
// omitted on the wire.
func pinLabels(ids []string, meta map[string]pinMeta) map[string]string {
	var out map[string]string
	for _, id := range ids {
		if l := meta[id].Label; l != "" {
			if out == nil {
				out = make(map[string]string)
			}
			out[id] = l
		}
	}
	return out
}

// pinSyncFor builds the PinSync for b's stored pin set, with labels. ok is
// false when the chat has never had its pin set written (see pushPins for why
// that state is not replayed) or the set cannot be read.
func (h *Hub) pinSyncFor(b *convBinding) (fap.PinSync, bool) {
	idx := h.deps.SessionIndex
	if idx == nil {
		return fap.PinSync{}, false
	}
	stored, err := idx.GetChatMetadata(b.agentID, "app", b.chatID, chatMetaPins)
	if err != nil || stored == "" {
		return fap.PinSync{}, false
	}
	ids := session.DecodeChatSet(stored)
	f := fap.PinSync{ConversationID: b.convID, MessageIDs: ids}
	if len(ids) > 0 {
		if m, err := idx.GetChatMetadata(b.agentID, "app", b.chatID, chatMetaPinMeta); err == nil {
			f.Labels = pinLabels(ids, decodePinMeta(m))
		} else {
			// The set is readable and authoritative; send it unlabelled rather
			// than not at all (a missing PinSync would leave a stale pin set).
			appLog.Warnf("pin sync %s: read pin meta: %v", b.convID, err)
		}
	}
	return f, true
}

// handlePinsList answers a PinsList with every pinned message across all of
// the user's app chats (#2670), to the asking socket only. It reads only
// server-side state — the pin sets, their meta, and the frame store — so a
// chat whose messages this device never synced is listed like any other.
//
// A pin's snippet and time come from the frame store when it still holds the
// message, else from the snapshot taken at pin time. A pin found in neither
// (pinned before this feature, and its frames since trimmed) is left out:
// there is nothing to show for it but an empty row.
func (h *Hub) handlePinsList(client *wsClient) {
	out := fap.PinsAll{Conversations: []fap.PinGroup{}}
	idx := h.deps.SessionIndex
	if idx == nil {
		client.sendRaw(out)
		return
	}
	h.mu.RLock()
	bindings := make([]*convBinding, 0, len(h.convs))
	for _, b := range h.convs {
		bindings = append(bindings, b)
	}
	h.mu.RUnlock()

	newest := map[string]int64{}
	for _, b := range bindings {
		stored, err := idx.GetChatMetadata(b.agentID, "app", b.chatID, chatMetaPins)
		if err != nil {
			appLog.Warnf("pins list %s: read pins: %v", b.convID, err)
			continue
		}
		ids := session.DecodeChatSet(stored)
		if len(ids) == 0 {
			continue
		}
		metaStored, err := idx.GetChatMetadata(b.agentID, "app", b.chatID, chatMetaPinMeta)
		if err != nil {
			appLog.Warnf("pins list %s: read pin meta: %v", b.convID, err)
		}
		meta := decodePinMeta(metaStored)
		want := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			want[id] = struct{}{}
		}
		live := h.frames.MessagePreviews(b.convID, want)

		g := fap.PinGroup{ConversationID: b.convID, AgentID: b.agentID, Title: h.aliasFor(b)}
		for _, id := range ids {
			m := meta[id]
			p := fap.PinnedMessage{MessageID: id, Label: m.Label, Snippet: m.Snippet, SentMs: m.SentMs}
			if lp, ok := live[id]; ok {
				p.Snippet, p.SentMs = lp.Snippet, lp.SentMs
			}
			if p.Snippet == "" {
				continue // message found nowhere: no blank row
			}
			g.Pins = append(g.Pins, p)
		}
		if len(g.Pins) == 0 {
			continue
		}
		sort.SliceStable(g.Pins, func(i, j int) bool {
			if g.Pins[i].SentMs != g.Pins[j].SentMs {
				return g.Pins[i].SentMs > g.Pins[j].SentMs
			}
			return g.Pins[i].MessageID < g.Pins[j].MessageID
		})
		newest[b.convID] = g.Pins[0].SentMs
		out.Conversations = append(out.Conversations, g)
	}
	sort.SliceStable(out.Conversations, func(i, j int) bool {
		a, c := out.Conversations[i], out.Conversations[j]
		if newest[a.ConversationID] != newest[c.ConversationID] {
			return newest[a.ConversationID] > newest[c.ConversationID]
		}
		return a.ConversationID < c.ConversationID
	})
	client.sendRaw(out)
}

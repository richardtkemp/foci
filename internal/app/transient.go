package app

import (
	"encoding/json"
	"slices"
	"time"

	"foci/internal/fap"
)

// Transient frames (#1064).
//
// The status and streaming frames below describe a state that only the
// current value matters for. They go to the sockets attached when they happen
// and nowhere else: no seq, no replay buffer, no durable store, so neither a
// WebSocket resume nor GET /app/replay ever re-sends them (the app replayed
// them as history before: #1062). Seqs stay contiguous because a transient
// frame never takes one. A device that was not attached learns the current
// value from the hello roster (ConversationInfo: activity, cacheExpiryMs,
// reminders, waitingOnOther, meta, promptSuggestion), plus catchUpTransient
// for what changed while it connected.
//
// Activity frames are also rate-limited per conversation to one every
// activityMinInterval (Dick 2026-11-14, Q4/Q5); every server-side reader
// still sees each change at once (applyActivity).

// transientTypes are the conversation frame types sent live only.
var transientTypes = map[string]struct{}{
	fap.TypeActivity:         {},
	fap.TypeTurnStart:        {},
	fap.TypeTextDelta:        {},
	fap.TypeMeta:             {},
	fap.TypePromptSuggestion: {},
	fap.TypeCommands:         {},
	fap.TypeCacheExpiry:      {},
	fap.TypeReminders:        {},
	fap.TypeWaitingOnOther:   {},
}

// isTransientType reports whether frames of wire type t are transient.
func isTransientType(t string) bool {
	_, ok := transientTypes[t]
	return ok
}

// isTransientFrame reports whether frame is sent live only.
func isTransientFrame(frame fap.ServerFrame) bool { return isTransientType(frame.Type()) }

// activityMinInterval is the shortest gap between two Activity frames a
// device receives for one conversation.
const activityMinInterval = 500 * time.Millisecond

// Test seams for the activity rate limit: the interval, the clock and the
// timer. The package's TestMain sets activityInterval to 0 so tests that
// assert each resolver transition see every frame; the rate-limit tests set
// it back to activityMinInterval.
var (
	activityInterval = activityMinInterval
	activityNow      = time.Now
	activityAfter    = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
)

// sendTransient fans a transient frame out to the attached sockets and lets a
// depth leaf observe it. Activity frames reach here only when something calls
// send with one directly; applyActivity uses queueActivity instead.
func (b *convBinding) sendTransient(frame fap.ServerFrame) {
	b.fanOutLive(frame)
	b.observeLeaf(frame)
}

// fanOutLive encodes frame with no seq and enqueues it on every attached
// socket, each stamped with its own inbound ack.
func (b *convBinding) fanOutLive(frame fap.ServerFrame) {
	wire, err := fap.Encode(frame, 0, 0, "", "")
	if err != nil {
		appLog.Errorf("encode %s (conv=%s): %v", frame.Type(), b.convID, err)
		return
	}
	type target struct {
		c   *wsClient
		ack int64
	}
	b.mu.Lock()
	targets := make([]target, 0, len(b.clients))
	for c := range b.clients {
		var ack int64
		if st := b.clientStates[c]; st != nil {
			ack = st.seqHW
		}
		targets = append(targets, target{c, ack})
	}
	b.mu.Unlock()
	for _, t := range targets {
		t.c.enqueue(stampAck(wire, t.ack))
	}
}

// sendTransientTo sends a transient frame to one socket only (the connect
// catch-up).
func (b *convBinding) sendTransientTo(client *wsClient, frame fap.ServerFrame) {
	wire, err := fap.Encode(frame, 0, 0, "", "")
	if err != nil {
		appLog.Errorf("encode %s (conv=%s): %v", frame.Type(), b.convID, err)
		return
	}
	var ack int64
	b.mu.Lock()
	if st := b.clientStates[client]; st != nil {
		ack = st.seqHW
	}
	b.mu.Unlock()
	client.enqueue(stampAck(wire, ack))
}

// observeLeaf hands frame to the binding's depth leaf view, if it is one.
func (b *convBinding) observeLeaf(frame fap.ServerFrame) {
	b.mu.Lock()
	leaf := b.leaf
	b.mu.Unlock()
	if leaf != nil {
		leaf.observe(b, frame)
	}
}

// queueActivity sends the current activity (b.activitySent) to the attached
// sockets now, or, when one went out less than activityMinInterval ago,
// schedules one send for the end of the interval. The scheduled send carries
// whatever the state is then, so a state replaced within the window may never
// be sent, and the last frame a device holds after a quiet interval is the
// current state.
func (b *convBinding) queueActivity() {
	b.mu.Lock()
	if b.activityTimer {
		b.mu.Unlock()
		return
	}
	now := activityNow()
	wait := activityInterval - now.Sub(b.activityWireAt)
	if b.activityWireAt.IsZero() || wait <= 0 {
		a := b.activitySent
		b.activityWire = a
		b.activityWireAt = now
		b.mu.Unlock()
		b.fanOutLive(a)
		return
	}
	b.activityTimer = true
	b.mu.Unlock()
	activityAfter(wait, b.flushActivity)
}

// flushActivity is the scheduled half of queueActivity.
func (b *convBinding) flushActivity() {
	b.mu.Lock()
	b.activityTimer = false
	a := b.activitySent
	if activityEqual(a, b.activityWire) {
		// Back where the devices already are: nothing to send.
		b.mu.Unlock()
		return
	}
	b.activityWire = a
	b.activityWireAt = activityNow()
	b.mu.Unlock()
	b.fanOutLive(a)
}

// noteSnapshot keeps the hello snapshot of meta and the prompt suggestion in
// step with the frames sent (#1064 req 4). The suggestion follows the app's
// own rule: a new suggestion sets it, an empty one clears it, and the
// conversation's next visible reply (turn start, message, media, ask) makes
// it stale. A silent turn sends none of those, so it keeps the suggestion.
// Changes are written to the store so they survive a restart.
func (b *convBinding) noteSnapshot(frame fap.ServerFrame) {
	var (
		setMeta  *fap.ConversationMeta
		setSugg  bool
		suggText string
	)
	switch f := frame.(type) {
	case fap.Meta:
		setMeta = &fap.ConversationMeta{
			Model: f.Model, PrevCostUsd: f.PrevCostUsd, Tokens: f.Tokens, Gap: f.Gap,
			CompactionLimitTokens: f.CompactionLimitTokens,
		}
	case fap.PromptSuggestion:
		setSugg, suggText = true, f.Text
	case fap.TurnStart, fap.ServerMessage, fap.Media, fap.Interactive:
		setSugg = true
	default:
		return
	}
	b.mu.Lock()
	changed := false
	if setMeta != nil && !metaEqual(b.snapMeta, setMeta) {
		b.snapMeta = setMeta
		changed = true
	}
	if setSugg && b.snapSuggestion != suggText {
		b.snapSuggestion = suggText
		changed = true
	}
	meta, sugg, store, convID := b.snapMeta, b.snapSuggestion, b.store, b.convID
	b.mu.Unlock()
	if changed {
		store.PutConvSnapshot(convID, meta, sugg)
	}
}

// hasSuggestion reports whether the conversation's snapshot holds a prompt
// suggestion.
func (b *convBinding) hasSuggestion() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapSuggestion != ""
}

// metaEqual compares two snapshot metas by value.
func metaEqual(x, y *fap.ConversationMeta) bool {
	if x == nil || y == nil {
		return x == y
	}
	xj, _ := json.Marshal(x)
	yj, _ := json.Marshal(y)
	return string(xj) == string(yj)
}

// catchUpTransient runs after a hello's conversations are attached. A change
// made between the hello's roster build (before) and the attach went only to
// the sockets attached then, so it re-reads the roster and sends this socket
// every value that moved, live only. It also sends each conversation's
// command palette: palettes are transient now, so a device no longer gets one
// by replay, and the hub's change detection would skip an unchanged one.
func (h *Hub) catchUpTransient(client *wsClient, before []fap.AgentInfo) {
	prev := make(map[string]fap.ConversationInfo)
	for _, a := range before {
		for _, c := range a.Conversations {
			prev[c.ID] = c
		}
	}
	h.mu.RLock()
	bindings := make(map[string]*convBinding, len(h.convs))
	for id, b := range h.convs {
		bindings[id] = b
	}
	h.mu.RUnlock()
	for _, a := range h.agentRoster() {
		for _, c := range a.Conversations {
			b := bindings[c.ID]
			if b == nil || !b.isAttached(client) {
				continue
			}
			old, seen := prev[c.ID]
			for _, f := range transientDiff(c, old, seen) {
				b.sendTransientTo(client, f)
			}
		}
	}
	h.pushCommandsTo(client)
}

// transientDiff returns the frames that move a device holding old to cur.
// !seen (the conversation was not in the hello) sends every value.
func transientDiff(cur, old fap.ConversationInfo, seen bool) []fap.ServerFrame {
	var out []fap.ServerFrame
	id := cur.ID
	curAct := activityOf(cur)
	if !seen || !activityEqual(curAct, activityOf(old)) {
		out = append(out, curAct)
	}
	if !seen || cur.CacheExpiryMs != old.CacheExpiryMs {
		out = append(out, fap.CacheExpiry{ConversationID: id, ExpiryMs: cur.CacheExpiryMs})
	}
	if !seen || !slices.Equal(cur.Reminders, old.Reminders) {
		out = append(out, fap.Reminders{ConversationID: id, Reminders: cur.Reminders})
	}
	if !seen || cur.WaitingOnOther != old.WaitingOnOther {
		out = append(out, fap.WaitingOnOther{ConversationID: id, WaitingOnOther: cur.WaitingOnOther})
	}
	if cur.Meta != nil && (!seen || !metaEqual(cur.Meta, old.Meta)) {
		m := cur.Meta
		out = append(out, fap.Meta{ConversationID: id, Model: m.Model, PrevCostUsd: m.PrevCostUsd, Tokens: m.Tokens, Gap: m.Gap, CompactionLimitTokens: m.CompactionLimitTokens})
	}
	if !seen || cur.PromptSuggestion != old.PromptSuggestion {
		out = append(out, fap.PromptSuggestion{ConversationID: id, Text: cur.PromptSuggestion})
	}
	return out
}

// activityOf rebuilds the Activity frame a roster entry describes.
func activityOf(c fap.ConversationInfo) fap.Activity {
	return fap.Activity{
		ConversationID: c.ID, Kind: c.Activity, Detail: c.ActivityDetail,
		AgentKind: c.ActivityAgentKind, AgentDetail: c.ActivityAgentDetail, AgentCommand: c.ActivityAgentCommand,
		AgentShell: c.ActivityAgentShell, AgentStartedMs: c.ActivityAgentStartedMs, Subagents: c.Subagents,
	}
}

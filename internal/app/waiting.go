package app

import (
	"time"

	"foci/internal/clock"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/fap"
)

// Waiting on something other than the user (#3112).
//
// Stay-busy (an app feature) alerts loudly while a chat needs the user. Dick
// (2026-11-14): "stay busy asks the question: Is the agent waiting for the
// user, or is it waiting for something else? It should alert the user loudly
// if it is waiting for the user." The hub answers that question per
// conversation and tells devices: at hello (ConversationInfo.WaitingOnOther)
// and live (the WaitingOnOther frame) whenever the value changes.
//
// The value is TRUE while at least one of these holds for the conversation's
// session:
//   - a turn is in flight (including warming and a rate-limit retry);
//   - an Agent-tool subagent is running;
//   - a background spawn started by the session is running;
//   - the session waits for another agent's reply;
//   - a background shell command is running and is younger than the threshold;
//   - an async tool result is pending and started less than the threshold ago;
//   - a wake reminder of the session is pending, held or fired, and is due
//     within the threshold;
//   - the usage hold holds input of the session;
//   - the conversation is a depth root and one of its leaves is TRUE by the
//     rows above.
//
// Otherwise it is FALSE: the agent waits for the user. The threshold is
// behavior.stay_busy_wait_threshold (default 1h, per agent, hot). The rule is
// NOT a display rule: the Activity frame, activityIdle and the auto-close
// sweep are unchanged (an old background command is still shown running).
//
// A change caused only by time passing is found by a sweep every
// waitingSweepInterval, so a connected device hears it within a minute.

// featureWaitingOnOther is the ClientHello capability a client advertises to
// receive WaitingOnOther frames. A client without it (every client before
// #3112) gets none, so it sees exactly the frames it saw before; the roster's
// waitingOnOther field goes to every client (unknown keys are ignored).
const featureWaitingOnOther = "waitingOnOther"

// waitingSweepInterval is how often every conversation's value is re-checked
// for a change that time alone causes (a threshold passing).
const waitingSweepInterval = 30 * time.Second

// waitInputs is what the rule reads from the conversation's agent beyond the
// binding: the threshold, the youngest pending async result and whether the
// usage hold holds the session's input. *agent.Agent implements it.
type waitInputs interface {
	StayBusyWaitThreshold() time.Duration
	AsyncPendingSince(sessionKey string) (time.Time, bool)
	UsageHeldInput(sessionKey string) bool
}

// now is the hub's clock (the wall clock unless a test set waitClock).
func (h *Hub) now() time.Time { return h.clock().Now() }

func (h *Hub) clock() clock.Clock {
	if h.waitClock != nil {
		return h.waitClock
	}
	return clock.Real()
}

// waitInputsOf returns the inputs of agentID, whose connection is conn (nil =
// not registered). It takes no lock.
func (h *Hub) waitInputsOf(agentID string, conn *appConn) waitInputs {
	if h.waitInputsFor != nil {
		return h.waitInputsFor(agentID)
	}
	if conn == nil || conn.agentRef == nil {
		return nil
	}
	in, _ := conn.agentRef.(waitInputs)
	return in
}

// waitInputs returns agentID's inputs, or nil when the agent has none (no
// registered connection, or a handler that is not an agent core). It holds
// h.mu only for the lookup: the caller calls the inputs with it released.
func (h *Hub) waitInputs(agentID string) waitInputs {
	h.mu.RLock()
	conn := h.agents[agentID]
	h.mu.RUnlock()
	return h.waitInputsOf(agentID, conn)
}

// waitSnap is the part of the rule the binding decides from its own state.
type waitSnap struct {
	busy        bool    // a row that holds without a threshold
	commandsMs  []int64 // start (unix ms) of each running background command
	agentID     string
	sessionKey  string
	depthRootID string
}

// waitSnapLocked reads the binding's rows. Caller holds b.mu.
func (b *convBinding) waitSnapLocked() waitSnap {
	agentID, sk := b.identity()
	s := waitSnap{agentID: agentID, sessionKey: sk, depthRootID: b.depthRoot}
	turn := b.turnKind != "" && b.turnKind != fap.ActivityKindIdle
	s.busy = turn || b.retryDetail != "" || b.waitingDetail != "" || len(b.spawns) > 0
	if len(b.subagents) == 0 && b.subagentDetail != "" {
		// A detail with no structured list yet: its kind is unknown, so it
		// counts as a subagent until the list arrives.
		s.busy = true
	}
	for _, r := range b.subagents {
		if r.Kind != delegator.SubagentKindCommand {
			s.busy = true
			continue
		}
		started := r.StartedMs
		if started == 0 {
			started = b.commandSeenMs[r.ID]
		}
		s.commandsMs = append(s.commandsMs, started)
	}
	return s
}

// noteCommandsSeenLocked records when each running background command with
// no start time of its own was first seen, so the threshold can age it.
// Caller holds b.mu.
func (b *convBinding) noteCommandsSeenLocked(now time.Time) {
	var keep map[string]int64
	for _, r := range b.subagents {
		if r.Kind != delegator.SubagentKindCommand || r.StartedMs != 0 {
			continue
		}
		if keep == nil {
			keep = map[string]int64{}
		}
		if ms, ok := b.commandSeenMs[r.ID]; ok {
			keep[r.ID] = ms
		} else {
			keep[r.ID] = now.UnixMilli()
		}
	}
	b.commandSeenMs = keep
}

// ownWaiting applies the rule's rows, all but the depth one, to a snapshot.
// rs is the session's pending wake list; in is the agent's inputs (nil =
// none). It takes no hub or binding lock.
func ownWaiting(s waitSnap, rs []fap.PendingReminder, in waitInputs, now time.Time) bool {
	if s.busy {
		return true
	}
	threshold := config.DefaultStayBusyWaitThreshold
	if in != nil {
		if d := in.StayBusyWaitThreshold(); d > 0 {
			threshold = d
		}
	}
	nowMs := now.UnixMilli()
	for _, started := range s.commandsMs {
		if time.Duration(nowMs-started)*time.Millisecond < threshold {
			return true
		}
	}
	if in != nil {
		if since, ok := in.AsyncPendingSince(s.sessionKey); ok && now.Sub(since) < threshold {
			return true
		}
		if in.UsageHeldInput(s.sessionKey) {
			return true
		}
	}
	for _, r := range rs {
		if r.DueAtMs == 0 {
			continue // no due time: not a wake the agent is waiting for soon
		}
		if time.Duration(r.DueAtMs-nowMs)*time.Millisecond <= threshold {
			return true
		}
	}
	return false
}

// ownWaitingOf evaluates one binding's own rows (no depth row). It reads the
// binding under b.mu, then the agent's inputs and wake list without it. Caller
// must not hold h.mu: the inputs lookup takes it.
func (h *Hub) ownWaitingOf(b *convBinding, now time.Time) (bool, waitSnap) {
	b.mu.Lock()
	s := b.waitSnapLocked()
	b.mu.Unlock()
	if s.busy {
		return true, s
	}
	return ownWaiting(s, remindersFor(s.agentID, s.sessionKey), h.waitInputs(s.agentID), now), s
}

// depthLeaves returns the leaves of the depth tree whose root conversation is
// rootID.
func (h *Hub) depthLeaves(rootID string) []*convBinding {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []*convBinding
	for _, b := range h.convs {
		b.mu.Lock()
		leaf := b.depthRoot == rootID
		b.mu.Unlock()
		if leaf {
			out = append(out, b)
		}
	}
	return out
}

// waitingOnOther evaluates the whole rule for b, the depth row included.
func (h *Hub) waitingOnOther(b *convBinding, now time.Time) bool {
	own, s := h.ownWaitingOf(b, now)
	if own {
		return true
	}
	if s.depthRootID != "" {
		return false // a leaf has no leaves of its own
	}
	for _, leaf := range h.depthLeaves(b.convID) {
		if v, _ := h.ownWaitingOf(leaf, now); v {
			return true
		}
	}
	return false
}

// refreshWaiting re-evaluates b's value and, on a change from what the
// devices hold, sends a WaitingOnOther frame. A depth leaf's change can move
// its root's value, so the root is refreshed after it.
//
// The evaluation holds no lock (#3170): it calls into the agent and the
// reminder store, and one stuck call must not stop the conversation's other
// refreshes. Two racing refreshes still cannot send out of order: each is
// numbered when it starts reading, and waitMu applies only an evaluation
// newer than the last one applied. Every input a later-numbered evaluation
// reads is read after the earlier one started, so dropping the older one
// loses nothing.
func (h *Hub) refreshWaiting(b *convBinding) {
	if h == nil || b == nil {
		return
	}
	seq := b.waitEvalSeq.Add(1)
	v := h.waitingOnOther(b, h.now())
	b.waitMu.Lock()
	if seq < b.waitSentSeq {
		b.waitMu.Unlock()
		return // a newer evaluation was applied (and refreshed the root)
	}
	b.waitSentSeq = seq
	b.mu.Lock()
	changed := v != b.waitingOther
	b.waitingOther = v
	convID, root := b.convID, b.depthRoot
	b.mu.Unlock()
	if changed && b.supportsFeature(featureWaitingOnOther) {
		b.send(fap.WaitingOnOther{ConversationID: convID, WaitingOnOther: v})
	}
	b.waitMu.Unlock()
	if root != "" {
		h.mu.RLock()
		rb := h.convs[root]
		h.mu.RUnlock()
		h.refreshWaiting(rb)
	}
}

// refreshWaitingAll re-evaluates every conversation: the sweep's pass.
func (h *Hub) refreshWaitingAll() {
	h.mu.RLock()
	bindings := make([]*convBinding, 0, len(h.convs))
	for _, b := range h.convs {
		bindings = append(bindings, b)
	}
	h.mu.RUnlock()
	for _, b := range bindings {
		h.refreshWaiting(b)
	}
}

// startWaitingSweep starts the time-based re-check, once per hub, on the
// hub's clock. It stops when the gateway context ends; a hub with no context
// (tests) runs it until the process ends unless the test stops it.
func (h *Hub) startWaitingSweep() {
	h.waitSweepOnce.Do(func() {
		var tick func()
		tick = func() {
			if ctx := h.deps.Ctx; ctx != nil && ctx.Err() != nil {
				return
			}
			h.refreshWaitingAll()
			h.clock().AfterFunc(waitingSweepInterval, tick)
		}
		h.clock().AfterFunc(waitingSweepInterval, tick)
	})
}

// rosterWaiting computes every conversation's value for a roster build.
// bindings and conns are the roster's copy of the hub's maps; rs maps a
// conversation to its pending wake list (the roster already read it). It
// does not change what the devices are recorded to hold: the roster may go
// to one socket only, and the others hold the last frame. A conversation
// whose value moved since that frame is refreshed off the caller's
// goroutine, so every device gets the change. Caller must not hold h.mu: the
// rule calls into the agents (#3170).
func (h *Hub) rosterWaiting(bindings []*convBinding, conns map[string]*appConn, rs map[string][]fap.PendingReminder) map[string]bool {
	now := h.now()
	own := make(map[string]bool, len(bindings))
	roots := make(map[string]string, len(bindings))
	for _, b := range bindings {
		b.mu.Lock()
		s := b.waitSnapLocked()
		b.mu.Unlock()
		own[b.convID] = s.busy || ownWaiting(s, rs[b.convID], h.waitInputsOf(s.agentID, conns[s.agentID]), now)
		roots[b.convID] = s.depthRootID
	}
	out := make(map[string]bool, len(own))
	for id, v := range own {
		out[id] = out[id] || v
		if root := roots[id]; root != "" && v {
			out[root] = true
		}
	}
	var moved []*convBinding
	for _, b := range bindings {
		b.mu.Lock()
		if b.waitingOther != out[b.convID] {
			moved = append(moved, b)
		}
		b.mu.Unlock()
	}
	if len(moved) > 0 {
		go func() {
			for _, b := range moved {
				h.refreshWaiting(b)
			}
		}()
	}
	return out
}

// NotifyWaitInputsChanged re-evaluates the conversation bound to sessionKey
// after one of the agent-side inputs changed (a pending async result started
// or ended, the usage hold took or released its input). Off the caller's
// goroutine: callers include the session's inbox worker. No-op when the app
// provider is not running or no conversation is bound.
func NotifyWaitInputsChanged(sessionKey string) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil {
		return
	}
	b := h.bindingForSession(sessionKey)
	if b == nil {
		return
	}
	go h.refreshWaiting(b)
}

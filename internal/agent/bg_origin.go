package agent

import (
	"context"
	"time"

	"foci/internal/session"
	"foci/internal/turnevent"
)

// Background work started by a non-delivered turn (#2093).
//
// A reflection, keepalive or memory turn runs on the chat session's own CC
// process. When it starts a background subagent or command, the turn can end
// while the work is still running: CC goes idle and the orchestrator clears the
// turn's sink. Without more, the work's later output fell to the router's
// late-delivery fallback, reaching the chat and being recorded untagged; and
// when it finished, CC's reaction to the result ran as an autonomous run that
// delivered to the chat too.
//
// So every tool call a non-delivered turn makes is stamped here with the
// turn's kind, keyed by its tool_use id, which is also the group key of the
// background task it may become (ccstream's tracker, SubagentStart and
// task_notification all key on it). Two readers use the stamp:
//   - the session router diverts a stamped group's subagent events to a
//     record-only sink tagged with the kind, unless the originating turn's own
//     sink is still the current one (bgOriginSink);
//   - an autonomous run whose triggers (the tasks that ended while CC was idle)
//     are all stamped is adopted record-only, tagged with the kind
//     (takeBgOriginKind, OpenAutonomousTurn).
//
// By ruling (Dick, 2026-10-05) both are recorded with the originating turn's
// kind and not delivered, for keepalive too, whose own turn is not recorded.

// bgOriginMaxAge bounds how long a stamp is kept. Most stamped calls are
// ordinary foreground tools that never become a task, so stamps are pruned by
// age rather than by a completion that never comes. It is well past the
// tracker's default background_task_max_age (2h), after which the task has
// been given up on anyway.
const bgOriginMaxAge = 6 * time.Hour

// bgOrigin is one stamped tool call.
type bgOrigin struct {
	kind     string
	turnSink turnevent.Sink // the sink the originating turn registered on the router
	record   turnevent.Sink // records tagged with kind, delivers nothing
	at       time.Time
}

// nonDeliveredOriginKind returns the kind a turn's background work is tagged
// with: the trigger, when it names a non-delivered turn kind (listed in
// nonDeliveredTurnKinds, recorded or not) and the turn's sink does not deliver.
// "" means the turn delivers, and its background work delivers as before.
func nonDeliveredOriginKind(trigger string, sink turnevent.Sink) string {
	if _, listed := nonDeliveredTurnKinds[trigger]; !listed {
		return ""
	}
	if sink != nil && sink.DeliversToPlatform() {
		return ""
	}
	return trigger
}

// originStampSink wraps the sink a non-delivered turn registers on its session
// router and stamps each tool call that passes through it.
type originStampSink struct {
	inner turnevent.Sink
	a     *Agent
	sk    string
	kind  string
}

// withOriginStamp wraps sink so the tool calls it sees are stamped with kind.
// kind "" returns sink unchanged.
func (a *Agent) withOriginStamp(sink turnevent.Sink, sk, kind string) turnevent.Sink {
	if kind == "" || sink == nil {
		return sink
	}
	return &originStampSink{inner: sink, a: a, sk: sk, kind: kind}
}

func (s *originStampSink) Emit(ctx context.Context, ev turnevent.Event) {
	if tc, ok := ev.(turnevent.ToolCall); ok && tc.ID != "" {
		s.a.stampBgOrigin(s.sk, tc.ID, s.kind, s)
	}
	s.inner.Emit(ctx, ev)
}

func (s *originStampSink) DeliversToPlatform() bool { return s.inner.DeliversToPlatform() }

// Unwrap implements turnevent.Unwrapper (router identity checks, logging
// detection).
func (s *originStampSink) Unwrap() turnevent.Sink { return s.inner }

// stampBgOrigin records that tool call id on sk was made by a non-delivered
// turn of kind, whose registered sink is turnSink. Prunes expired stamps.
func (a *Agent) stampBgOrigin(sk, id, kind string, turnSink turnevent.Sink) {
	now := time.Now()
	a.bgOriginsMu.Lock()
	defer a.bgOriginsMu.Unlock()
	if a.bgOrigins == nil {
		a.bgOrigins = make(map[string]map[string]*bgOrigin)
	}
	m := a.bgOrigins[sk]
	if m == nil {
		m = make(map[string]*bgOrigin)
		a.bgOrigins[sk] = m
	}
	for k, o := range m {
		if now.Sub(o.at) > bgOriginMaxAge {
			delete(m, k)
		}
	}
	if _, ok := m[id]; ok {
		return
	}
	m[id] = &bgOrigin{
		kind:     kind,
		turnSink: turnSink,
		record:   newTurnKindLoggingSink(turnevent.NopSink{}, a, session.ChatIDFromKey(sk), &TurnMetadata{}, sk, kind),
		at:       now,
	}
}

// bgOriginSink returns where a subagent event on sk must go instead of the
// router's normal dispatch: the record-only sink of the non-delivered turn that
// started the event's group. nil (dispatch normally) for a non-subagent event,
// a group with no stamp, or when current, the router's registered sink, is (a
// decoration of) that turn's own sink: the turn is still running and records
// its subagents itself.
//
// A delivering turn that resumes the subagent (SendMessage: a new run of the
// same group, whose start arrives while that turn is current) takes the group
// over: the stamp is dropped, so the resumed run and CC's reaction to its
// result deliver like the rest of that turn.
func (a *Agent) bgOriginSink(sk string, ev turnevent.Event, current turnevent.Sink) turnevent.Sink {
	group := subagentGroup(ev)
	if group == "" {
		return nil
	}
	a.bgOriginsMu.Lock()
	defer a.bgOriginsMu.Unlock()
	o := a.bgOrigins[sk][group]
	if o == nil {
		return nil
	}
	if sinkChainContains(current, o.turnSink) {
		return nil
	}
	if st, ok := ev.(turnevent.SubagentStart); ok && st.RunIndex > 1 && current != nil && current.DeliversToPlatform() {
		delete(a.bgOrigins[sk], group)
		return nil
	}
	return o.record
}

// sinkChainContains reports whether target is sink or any sink it decorates.
// target is always an *originStampSink, so the compare cannot panic on an
// uncomparable dynamic type. Depth-capped like turnevent.Unwrap.
func sinkChainContains(sink, target turnevent.Sink) bool {
	for i := 0; i < 32 && sink != nil; i++ {
		if sink == target {
			return true
		}
		u, ok := sink.(turnevent.Unwrapper)
		if !ok {
			return false
		}
		sink = u.Unwrap()
	}
	return false
}

// takeBgOriginKind classifies an autonomous run by its triggers, the group keys
// of the background tasks whose results CC opened the run to react to. It
// returns the originating kind when every trigger was stamped by a
// non-delivered turn (the first one's kind if they differ), else "" (no
// triggers, or any unstamped one: the run delivers, as before). The triggers'
// stamps are dropped either way: their tasks have ended.
func (a *Agent) takeBgOriginKind(sk string, triggers []string) string {
	if len(triggers) == 0 {
		return ""
	}
	a.bgOriginsMu.Lock()
	defer a.bgOriginsMu.Unlock()
	m := a.bgOrigins[sk]
	kind := ""
	all := true
	for _, g := range triggers {
		o := m[g]
		if o == nil {
			all = false
			continue
		}
		if kind == "" {
			kind = o.kind
		}
		delete(m, g)
	}
	if !all {
		return ""
	}
	return kind
}

// subagentGroup returns the group key of a subagent lifecycle/text event, ""
// for any other event.
func subagentGroup(ev turnevent.Event) string {
	switch e := ev.(type) {
	case turnevent.SubagentStart:
		return e.GroupKey
	case turnevent.SubagentText:
		return e.GroupKey
	case turnevent.SubagentPrompt:
		return e.GroupKey
	case turnevent.SubagentEnd:
		return e.GroupKey
	}
	return ""
}

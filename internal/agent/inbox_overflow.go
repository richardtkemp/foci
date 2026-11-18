package agent

// Overflow for messages within the agent (#3163, Dick 2026-11-18: a chat
// parent receiving a burst of more than 64 child reports during a long
// protected turn "must drop nothing either").
//
// A SkipHolds injection (#3114: a spawn's [SPAWN RESULT], its send_to_spawner
// message, an async tool result) that finds the session's channel full
// (inboxChanSize) waits in the inbox's overflow list instead of being
// dropped. One pump goroutine per inbox moves the list onto the channel, in
// arrival order, as the worker frees room; a SkipHolds injection that arrives
// while the list is not empty joins its tail, so those injections keep their
// order. While the list holds anything, the inbox is not evicted. Other
// injections keep the refusal they had (their callers handle it: #2847).

// sendInjection puts env on inb's channel, or, for a SkipHolds injection
// that finds it full, on the overflow list. False when env was refused.
func (a *Agent) sendInjection(inb *sessionInbox, env Envelope) bool {
	if !env.Inject.SkipHolds {
		select {
		case inb.ch <- env:
			return true
		default:
			a.logger().Warnf("inbox: queue full for sk=%s, dropping injection trigger=%s", env.SessionKey, env.Inject.Trigger)
			return false
		}
	}
	inb.overflowMu.Lock()
	if len(inb.overflow) == 0 {
		select {
		case inb.ch <- env:
			inb.overflowMu.Unlock()
			return true
		default:
		}
	}
	inb.overflow = append(inb.overflow, env)
	n, start := len(inb.overflow), !inb.pumping
	inb.pumping = true
	inb.overflowMu.Unlock()
	a.logger().Warnf("inbox: queue full for sk=%s, injection trigger=%s waits in the overflow (%d waiting)", env.SessionKey, env.Inject.Trigger, n)
	if start {
		go a.pumpOverflow(inb)
	}
	return true
}

// pumpOverflow moves inb's overflow list onto its channel, head first, until
// the list is empty. When the inbox stops, the rest is refused (each
// injection's Refused hook fires, as for an injection the stop dropped).
func (a *Agent) pumpOverflow(inb *sessionInbox) {
	ctx := a.inboxCtx
	for {
		inb.overflowMu.Lock()
		if len(inb.overflow) == 0 {
			inb.pumping = false
			inb.overflowMu.Unlock()
			return
		}
		env := inb.overflow[0]
		inb.overflowMu.Unlock()
		// The inbox is not evicted while the list holds anything
		// (tryEvictInbox), so its worker reads this channel.
		var done <-chan struct{}
		if ctx != nil {
			done = ctx.Done()
		}
		select {
		case inb.ch <- env:
		case <-done:
			a.refuseOverflow(inb)
			return
		}
		inb.overflowMu.Lock()
		inb.overflow = inb.overflow[1:]
		inb.overflowMu.Unlock()
	}
}

// refuseOverflow empties inb's overflow list, firing each Refused hook.
func (a *Agent) refuseOverflow(inb *sessionInbox) {
	inb.overflowMu.Lock()
	rest := inb.overflow
	inb.overflow, inb.pumping = nil, false
	inb.overflowMu.Unlock()
	for _, env := range rest {
		a.logger().Warnf("inbox: stopped with an overflowed injection for sk=%s trigger=%s", env.SessionKey, env.Inject.Trigger)
		if env.Inject.Refused != nil {
			env.Inject.Refused()
		}
	}
}

// overflowLen is how many injections wait in inb's overflow list.
func (inb *sessionInbox) overflowLen() int {
	inb.overflowMu.Lock()
	defer inb.overflowMu.Unlock()
	return len(inb.overflow)
}

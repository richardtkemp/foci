// jit_memory.go — just-in-time memory search on chat messages (#3175).
//
// Dick 2026-11-19: on every user message and every agent message to the
// user's chat, foci searches the agent's memory; the fast model judges the
// hits; the relevant notes go to the agent. The search itself is
// internal/jitmemory; this file decides WHEN it runs and HOW its result
// reaches the agent:
//
//   - a user message (an interactive turn: a platform message or voice) starts
//     its run when the turn starts and waits at most [jit_memory] wait for it
//     (Q110). A result in time is appended to the turn's prompt as labelled
//     foci-added context. A later one is sent as for an agent message;
//   - a user message steered into a running turn has no turn start to wait
//     at, so its result is sent as for an agent message;
//   - an agent message on a chat session (its text reaching the chat) runs
//     only on a backend that steers mid-turn (CapMidTurnSteer, Q109); its
//     result goes to the agent at once through JITDeliverFn, a SkipHolds
//     injection under trigger jit_memory: it steers into a running turn,
//     else runs as a work turn of its own. On a backend that does not steer
//     (opencode) a late user-message result is dropped (logged as
//     late_dropped), and agent messages are only recorded as context.
//
// A message from a jit_memory turn is never searched, so a result cannot
// start a chain. Every failure is one WARN inside the engine; the message
// flows unchanged.

package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"foci/internal/jitmemory"
	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
)

// TriggerJITMemory is the trigger of a JIT memory result sent to the agent.
const TriggerJITMemory = "jit_memory"

// jitRunTimeout bounds one run (two model calls and the searches).
const jitRunTimeout = 60 * time.Second

// jitSettings returns the live [jit_memory] settings and whether the
// feature is on for this agent.
func (a *Agent) jitSettings() (jitmemory.Settings, bool) {
	if a.JIT == nil || a.LiveConfigFn == nil {
		return jitmemory.Settings{}, false
	}
	r := a.LiveConfigFn().JITMemory
	s := jitmemory.Settings{
		Enabled: r.Enabled, SkipChars: r.SkipChars, ContextChars: r.ContextChars, ContextMessages: r.ContextMessages,
		TopN: r.TopN, Wait: r.Wait, DedupeCompactions: r.DedupeCompactions, InjectMaxChars: r.InjectMaxChars,
		SectionMaxChars: r.SectionMaxChars, QueryStrategy: r.QueryStrategy, QueryModel: r.QueryModel, JudgeModel: r.JudgeModel,
	}
	return s, s.Enabled
}

// jitChatSession reports whether sk is a chat's own session (not a spawn,
// branch or independent session): the "user's chat" of the ticket.
func jitChatSession(sk string) bool {
	k, err := session.ParseSessionKey(sk)
	return err == nil && k.Type == 'c' && k.IsRoot()
}

// jitPending is one user message's run, which the turn waits for.
type jitPending struct {
	a    *Agent
	sk   string
	done chan struct{}

	mu    sync.Mutex
	out   jitmemory.Outcome
	ready bool // out is set
	taken bool // the turn attached the result
	late  bool // the turn stopped waiting: the result goes as an injection
}

// startUserJIT starts the run for ts's user message, or returns nil when the
// feature is off or the turn is not a user message on a chat session.
func (a *Agent) startUserJIT(ts *TurnState) *jitPending {
	s, on := a.jitSettings()
	if !on || !isInteractiveTrigger(ts.Trigger) || !jitChatSession(ts.SessionKey) {
		return nil
	}
	text := strings.TrimSpace(strings.Join(ts.Texts, "\n"))
	if text == "" {
		return nil
	}
	p := &jitPending{a: a, sk: ts.SessionKey, done: make(chan struct{})}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), jitRunTimeout)
		defer cancel()
		out := a.JIT.Run(ctx, jitmemory.Request{SessionKey: ts.SessionKey, Role: jitmemory.RoleUser, Text: text, Settings: s})
		p.mu.Lock()
		p.out, p.ready = out, true
		late := p.late
		close(p.done)
		p.mu.Unlock()
		if late {
			a.deliverLateJIT(p.sk, out)
		}
	}()
	return p
}

// take waits up to wait for the result and returns it for attaching; ok is
// false when the wait ran out (the result, when it comes, is then sent as an
// injection).
func (p *jitPending) take(wait time.Duration) (jitmemory.Outcome, bool) {
	if p == nil {
		return jitmemory.Outcome{}, false
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready && !p.late {
		p.taken = true
		return p.out, true
	}
	p.late = true
	return jitmemory.Outcome{}, false
}

// abandon hands the result to the injection path when the turn ended before
// it attached one (an error before the attach point). Deferred by the turn.
func (p *jitPending) abandon() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.taken || p.late {
		p.mu.Unlock()
		return
	}
	p.late = true
	ready, out := p.ready, p.out
	p.mu.Unlock()
	if ready {
		p.a.deliverLateJIT(p.sk, out)
	}
}

// attachUserJIT waits for p (at most [jit_memory] wait from now; the run
// started earlier in the turn, so most of its time overlaps the turn's
// preparation) and appends a result that arrived in time to the turn's
// input, after the user's own text.
func (a *Agent) attachUserJIT(ts *TurnState, p *jitPending) {
	if p == nil {
		return
	}
	s, _ := a.jitSettings()
	out, ok := p.take(s.Wait)
	if !ok {
		a.logger().Infof("jit-memory: session=%s result not ready within %s; the message goes alone", ts.SessionKey, s.Wait)
		return
	}
	if out.Injection == "" {
		return
	}
	if a.DelegatedManager != nil {
		ts.Prompt = ts.Prompt + "\n\n" + out.Injection
	} else if len(ts.Messages) > 0 && len(ts.NewMessages) > 0 {
		ts.UserMsg.Content = append(ts.UserMsg.Content, provider.ContentBlock{Type: "text", Text: out.Injection})
		ts.Messages[len(ts.Messages)-1] = ts.UserMsg
		ts.NewMessages[len(ts.NewMessages)-1] = ts.UserMsg
	} else {
		a.deliverLateJIT(ts.SessionKey, out)
		return
	}
	a.JIT.SetDelivery(out.RunID, jitmemory.DeliveryAttached)
	a.logger().Infof("jit-memory: session=%s attached %d chars to the user message (run %d)", ts.SessionKey, len(out.Injection), out.RunID)
}

// deliverLateJIT sends a user-message result that missed its turn to the
// agent, on a backend that takes a message mid-turn; on others it is dropped
// (Q109: there the feature covers user messages only, attached).
func (a *Agent) deliverLateJIT(sk string, out jitmemory.Outcome) {
	if out.Injection == "" {
		return
	}
	if !a.backendSteersMidTurn() || a.JITDeliverFn == nil {
		a.JIT.SetDelivery(out.RunID, jitmemory.DeliveryLateDropped)
		a.logger().Infof("jit-memory: session=%s late result dropped: the backend takes no message mid-turn (run %d)", sk, out.RunID)
		return
	}
	a.JITDeliverFn(sk, out.Injection)
	a.JIT.SetDelivery(out.RunID, jitmemory.DeliveryLateInjected)
}

// jitOnSteeredUserMessage runs a user message that steered into a running
// turn: there is no turn start to attach at, so the result is sent as an
// injection (it steers into the same turn when that is still running).
func (a *Agent) jitOnSteeredUserMessage(sk, text string) {
	s, on := a.jitSettings()
	if !on || !jitChatSession(sk) || strings.TrimSpace(text) == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), jitRunTimeout)
		defer cancel()
		out := a.JIT.Run(ctx, jitmemory.Request{SessionKey: sk, Role: jitmemory.RoleUser, Text: text, Settings: s})
		a.deliverLateJIT(sk, out)
	}()
}

// jitOnAgentMessage runs one agent message that reached the user's chat.
// delivers is whether the sink carrying the text delivers to a platform chat,
// and trigger is the trigger of the turn that wrote it ("" = unknown). Both
// come from the caller, not from a ctx: on a delegated backend the text
// reaches the logging sink through the session router with
// context.Background(), which carries neither (#3176).
func (a *Agent) jitOnAgentMessage(sk, text string, delivers bool, trigger string) {
	s, run := a.jitAgentMessageRuns(sk, text, delivers, trigger)
	if !run {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), jitRunTimeout)
		defer cancel()
		out := a.JIT.Run(ctx, jitmemory.Request{SessionKey: sk, Role: jitmemory.RoleAgent, Text: text, Settings: s})
		if out.Injection == "" {
			return
		}
		a.JITDeliverFn(sk, out.Injection)
		a.JIT.SetDelivery(out.RunID, jitmemory.DeliveryInjected)
	}()
}

// Why an agent message on a chat session was not searched (runs.skipped of
// its row; #3176: every such skip is recorded, so a gate that drops every
// message shows in jit_memory.db instead of passing unseen).
const (
	jitSkipSilent     = "agent: silent reply"
	jitSkipJITTurn    = "agent: reply in a jit_memory turn"
	jitSkipNoDelivery = "agent: not delivered to a chat"
	jitSkipNoSteer    = "agent: backend takes no message mid-turn"
	jitSkipNoDeliver  = "agent: no JIT delivery wired"
)

// jitAgentMessageRuns decides whether an agent message is searched, and
// returns the settings for the run. Never a message from a jit_memory turn (no
// chains), a silent one, or one that reaches no chat. On a backend without
// mid-turn steer (Q109) the message is only kept as context for the next
// short user message. Each of these skips writes a skipped row with its
// reason. The feature off, empty text or a session that is not a chat's own
// (a spawn, a branch) returns without a row: those are not messages to the
// user's chat.
func (a *Agent) jitAgentMessageRuns(sk, text string, delivers bool, trigger string) (jitmemory.Settings, bool) {
	s, on := a.jitSettings()
	if !on || !jitChatSession(sk) || strings.TrimSpace(text) == "" {
		return s, false
	}
	skip := ""
	switch {
	case platform.IsSilent(text):
		skip = jitSkipSilent
	case trigger == TriggerJITMemory:
		skip = jitSkipJITTurn
	case !delivers:
		skip = jitSkipNoDelivery
	case !a.backendSteersMidTurn():
		skip = jitSkipNoSteer
	case a.JITDeliverFn == nil:
		skip = jitSkipNoDeliver
	}
	if skip == "" {
		return s, true
	}
	if skip == jitSkipNoSteer || skip == jitSkipNoDeliver {
		a.JIT.Observe(sk, jitmemory.RoleAgent, text)
	}
	a.JIT.RecordSkip(sk, jitmemory.RoleAgent, text, skip)
	a.logger().Debugf("jit-memory: session=%s agent message not searched: %s", sk, skip)
	return s, false
}

// noteJITCompaction counts a compaction of sk for the dedupe lifetime
// ([jit_memory] dedupe_compactions). Counted whether or not the feature is
// on now, so turning it on later starts from the true count.
func (a *Agent) noteJITCompaction(sk string) {
	if a.JIT != nil {
		a.JIT.NoteCompaction(sk)
	}
}

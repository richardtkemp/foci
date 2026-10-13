package main

// Per-message delivery mode (steer/queue) for HTTP-originated sends (#1318).
//
// A choice-carrying /send (the `steer` field; foci send --steer/--queue)
// cannot ride today's injection paths: deliverBufferedQueued / runAgentQueued
// enqueue an Envelope.Inject, and Enqueue skips ALL user-message routing for
// injects (inbox.go) — the fold-or-queue decision lives in that routing, so
// the choice would be a silent no-op. Instead the request dispatches as a
// USER envelope through the same inbox routing a platform message takes,
// which requires a non-nil Driver (a nil-Driver envelope is dropped).

import (
	"context"
	"net/http"

	"foci/internal/agent"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/session"
	"foci/internal/timeutil"
	"foci/internal/turnevent"
)

// httpDriver is the agent.Driver for HTTP-originated user envelopes: it owns
// nothing platform-side (no typing indicator, no notification drain) and only
// resolves a rendering sink at turn time, so a choice-carrying /send's reply
// still reaches the session's chat.
//
// Connection returns nil DELIBERATELY. A nil connection resolves the turn's
// platformName to "" — non-interactive, exactly like today's injected /send
// turns (trigger ""/"user", [meta] via=external, never transport-folds, no
// app ask-capture carve-out). A real-looking platform name would leak into
// isInteractiveTrigger (transport-level fold in RunInference's race path) and
// the platformApp carve-outs, both wrong for an HTTP send.
type httpDriver struct {
	inst    *agentInstance
	connMgr platform.ConnectionManager
	policy  route.Policy
	logTag  string
}

// Connection implements agent.Driver; always nil — see the type doc.
func (d *httpDriver) Connection() platform.Connection { return nil }

// NewTurnSink implements agent.Driver. The delivering connection is resolved
// at TURN time (the same moment deliverBufferedQueued's Run closure resolves
// it), so a session that connects or reconnects after the request arrives
// still gets its reply. The sink stays RAW (unwrapped): RunTurn adds the
// conversation-logging wrapper itself with the batch's real TurnMetadata.
func (d *httpDriver) NewTurnSink(env agent.Envelope) (turnevent.Sink, func()) {
	if d.connMgr == nil {
		return turnevent.NopSink{}, nil
	}
	policy := policyOrFallback(d.policy)
	conn, outcome := route.ConnFor(d.connMgr, d.inst.id, env.SessionKey, policy)
	if conn == nil {
		log.NewComponentLogger(d.logTag).Warnf("no connection for session %s (policy=%s), /send response not delivered", env.SessionKey, policy)
		return turnevent.NopSink{}, nil
	}
	if outcome == route.DeliveredViaPrimary {
		log.NewComponentLogger(d.logTag).Infof("session %s has no live connection — delivering via agent %s primary", env.SessionKey, d.inst.id)
	}
	return rawTurnSinkForConn(conn, env.SessionKey, d.logTag)
}

// WrapTurn implements agent.Driver: run the turn, log a genuine (non-cancel)
// error, and return it so the agent logs it too. No platform-side lifecycle
// to wrap — that is the point of this driver.
func (d *httpDriver) WrapTurn(ctx context.Context, fn func() error) error {
	err := fn()
	if err != nil && ctx.Err() == nil {
		log.NewComponentLogger(d.logTag).Errorf("agent error: %v", err)
	}
	return err
}

// enqueueSteerChoice enqueues a choice-carrying /send as a USER envelope
// (#1318): Steer is honoured by Inbox.Enqueue's existing user routing (fold
// via steerToBackend for SteerAlways, queue for SteerNever), Human carries
// the #1130 stamp (RunTurn applies WithHumanSource), and httpDriver renders
// and delivers via the resolved connection. Returns false when the session
// inbox is full.
func enqueueSteerChoice(inst *agentInstance, d httpHandlerDeps, sessionKey, text string, pref platform.SteerPreference, human bool, policy route.Policy) bool {
	return inst.ag.Enqueue(agent.Envelope{
		SessionKey: sessionKey,
		Text:       text,
		ChatID:     session.ChatIDFromKey(sessionKey),
		ReceivedAt: timeutil.Now(),
		Steer:      pref,
		Human:      human,
		Driver:     &httpDriver{inst: inst, connMgr: d.connMgr, policy: policy, logTag: "http"},
	})
}

// writeSteerChoiceReceipt writes enqueueSteerChoice's HTTP outcome: the 202
// accepted receipt on success (async fire-and-forget AND sync-while-in-flight
// — a folded/queued turn's reply belongs to that turn, so the caller gets the
// acceptance instead of blocking on a reply that may never be this request's),
// or 503 when the inbox refused the envelope.
func writeSteerChoiceReceipt(w http.ResponseWriter, ok bool, rcpt route.Receipt) {
	if !ok {
		http.Error(w, "session inbox full", http.StatusServiceUnavailable)
		return
	}
	writeAccepted(w, rcpt)
}

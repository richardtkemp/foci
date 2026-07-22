package askgw

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// HTTPTransport adapts the askgw ask/answer machinery onto request/response
// HTTP, for remote callers that can't reach the local Unix socket (e.g. a
// Mac running aisudo, reaching foci over the network instead of ssh
// -forwarding a socket). It is a second front door onto the SAME *Server —
// same Registry, same PresentFn/CancelFn/ResolveSessionFn closures — not a
// parallel implementation of the ask machinery.
//
// # DESIGN FORK (read this before changing the shape below)
//
// The socket protocol is async/persistent-duplex: one long-lived connection,
// `ask` submitted with an id, the human answers whenever (seconds to
// minutes), the `answer` frame arrives correlated by that id on the same
// connection. HTTP is fundamentally request/response, and a decision can
// take minutes — so something has to give. Three shapes were on the table:
//
//  1. Hold a single POST open until the answer arrives (pure long-poll).
//  2. Submit-returns-id, then GET to poll for the result.
//  3. Submit-returns-id, then foci calls a webhook when answered.
//
// CHOSEN: (2), with the GET itself long-polling (blocks up to a bounded
// `wait`, default/max clamped well under a minute) so a caller doesn't have
// to busy-poll — it just re-issues the GET, which is (1)'s ergonomics without
// its failure mode. Rationale:
//
//   - (1) is not viable as a multi-minute hold: foci's shared HTTP server
//     (cmd/foci-gw/main.go) sets ReadTimeout/WriteTimeout to 30s for EVERY
//     endpoint on this mux. Raising that globally to accommodate one
//     endpoint's multi-minute wait would weaken the timeout protecting every
//     other handler (/send, /command, /branch, ...). A per-endpoint override
//     needs a second http.Server/listener, which is more moving parts for a
//     minute-scale human decision than the alternative below.
//   - (1) also has a worse failure mode over a REMOTE, possibly-flaky link
//     (the whole point of this feature): if the TCP connection drops while
//     the decision is still pending, the ask itself is unrecoverable — same
//     as the Unix-socket transport's documented limitation ("answers can
//     only reach the original connection"). A remote caller is materially
//     more likely to see a mid-wait connection drop than a local Unix-socket
//     client.
//   - (2) decouples the ask's lifetime from any one HTTP connection: the
//     registry entry lives independently, so a dropped poll just means the
//     next GET with the same id resumes waiting where the last one left off
//     — nothing is lost, no fresh `ask` frame needs resubmitting. This is
//     the "resumable re-poll" property that (1) cannot offer without
//     inventing its own resume protocol on top.
//   - (3) (webhook) was rejected: it requires the remote caller to run its
//     own inbound HTTP listener/public endpoint, which defeats the point for
//     a client like aisudo running unattended behind NAT/no inbound port —
//     exactly the "no notification channel of their own" problem askgw
//     exists to solve (see docs/ASKGW.md "Why").
//
// This mirrors the socket's own "ack now, answer later" shape closely: POST
// gets you the ack (a fast 202), GET is where you wait for the answer — it's
// just that "wait" is now a bounded, resumable poll instead of a blocking
// read on a socket already held open.
//
// Net-new limitation vs the socket transport: an ask answered while nobody
// is polling sits in memory until the next poll collects it (or is swept
// after staleAnswerTTL — see RunSweeper) rather than being delivered
// instantly to a listener. Accepted tradeoff: HTTP has no persistent
// listener to deliver to in the first place.
type HTTPTransport struct {
	srv *Server

	mu   sync.Mutex
	asks map[string]*httpAsk
}

// httpAsk tracks one HTTP-submitted ask's private registry connection and
// its eventual terminal AnswerFrame. Unlike the socket transport (where
// connID identifies a long-lived TCP connection that may host many
// concurrent asks), each HTTP ask gets its OWN connID: HTTP has no
// persistent connection to reuse across asks, so "one ask per synthetic
// connection" is the natural mapping onto the existing Registry, which was
// already designed to key entries by (connID, askID).
type httpAsk struct {
	connID     uint64
	done       chan struct{}
	result     AnswerFrame
	once       sync.Once
	resolvedAt time.Time
}

// HTTP-only terminal statuses. The wire protocol's four statuses
// (answered/timeout/dismissed/unavailable — see docs/ASKGW-PROTOCOL.md) all
// arrive as a real AnswerFrame from the registry and pass through unchanged.
// "cancelled" and "pending" are synthesized here because a poller needs an
// immediate, explicit status in cases the socket protocol doesn't (cancel
// sends no answer frame at all — a poll shouldn't just hang until timeout
// for a state we already know) or wasn't designed for (a poll returning
// before the human has answered is a normal outcome, not an error, and the
// caller needs to tell "still pending — poll again" apart from "unknown
// id").
const (
	StatusPending   = "pending"
	StatusCancelled = "cancelled"
)

// NewHTTPTransport wraps srv for HTTP submission. srv's registry, present/
// cancel/resolve closures are reused as-is.
func NewHTTPTransport(srv *Server) *HTTPTransport {
	return &HTTPTransport{srv: srv, asks: make(map[string]*httpAsk)}
}

// Submit accepts a raw `ask` frame body — byte-for-byte the same JSON shape
// the Unix-socket transport accepts (protocol/type/id/questions/...) — and
// presents it to the resolved chat, mirroring handleAsk. It returns as soon
// as presentation succeeds or fails, same as the socket's `ack`/immediate
// `unavailable` — it does NOT wait for a human answer; call Poll for that.
//
// Only the envelope (protocol/type/id) is inspected here, to key the HTTP
// bookkeeping map and to reject a duplicate/wrong-type submission before
// touching the registry. All frame-content validation (missing id, empty
// questions, duplicate option labels, ...) is delegated to the same
// handleFrame/handleAsk/AskFrame.Validate() path the socket transport uses
// — zero duplicated validation logic.
func (t *HTTPTransport) Submit(body []byte) (id string, ok bool, code, msg string) {
	_, typ, envID, err := DecodeEnvelope(body)
	if err != nil {
		return "", false, "malformed", err.Error()
	}
	if typ != TypeAsk {
		return envID, false, "unknown_type", fmt.Sprintf("expected type %q, got %q", TypeAsk, typ)
	}
	if envID == "" {
		return "", false, "malformed", "ask frame missing id"
	}

	t.mu.Lock()
	if _, exists := t.asks[envID]; exists {
		t.mu.Unlock()
		return envID, false, "duplicate_id", fmt.Sprintf("ask id %q already pending over HTTP", envID)
	}
	connID := t.srv.registry.RegisterConn()
	ha := &httpAsk{connID: connID, done: make(chan struct{})}
	t.asks[envID] = ha
	t.mu.Unlock()

	if err := t.srv.handleFrame(connID, &httpConnWriter{t: t}, body); err != nil {
		t.mu.Lock()
		delete(t.asks, envID)
		t.mu.Unlock()
		t.srv.registry.UnregisterConn(connID)
		if fe, ok2 := err.(*frameError); ok2 {
			return envID, false, fe.code, fe.message
		}
		return envID, false, "error", err.Error()
	}
	return envID, true, "", ""
}

// Poll waits up to `wait` for id's answer, returning the terminal
// AnswerFrame (status answered/timeout/dismissed/unavailable/cancelled) once
// resolved, or a synthetic {Status: StatusPending} if `wait` elapses first —
// the caller should re-issue Poll with the same id (see package doc:
// resumable long-poll, not a single held-open call).
//
// found=false means id is not a currently-pending HTTP ask: never submitted,
// already collected by a prior terminal Poll, or evicted by RunSweeper's TTL
// after resolving unpolled. All are indistinguishable from the caller's
// side — the ask is simply gone.
func (t *HTTPTransport) Poll(id string, wait time.Duration) (af AnswerFrame, found bool) {
	t.mu.Lock()
	ha := t.asks[id]
	t.mu.Unlock()
	if ha == nil {
		return AnswerFrame{}, false
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ha.done:
		t.mu.Lock()
		delete(t.asks, id)
		t.mu.Unlock()
		t.srv.registry.UnregisterConn(ha.connID)
		return ha.result, true
	case <-timer.C:
		return AnswerFrame{Protocol: ProtocolVersion, Type: TypeAnswer, ID: id, Status: StatusPending}, true
	}
}

// Cancel withdraws a pending HTTP-submitted ask: tears down its chat prompt
// via the same cancelFn the socket path wires (Registry.Cancel), and
// unblocks any in-flight Poll with a synthetic StatusCancelled result rather
// than leaving it to wait out its full timeout. Returns false if id is
// unknown (already resolved/collected, or never existed).
func (t *HTTPTransport) Cancel(id string) bool {
	t.mu.Lock()
	ha := t.asks[id]
	if ha != nil {
		delete(t.asks, id)
	}
	t.mu.Unlock()
	if ha == nil {
		return false
	}
	ok := t.srv.registry.Cancel(ha.connID, id)
	ha.once.Do(func() {
		ha.resolvedAt = time.Now()
		ha.result = AnswerFrame{Protocol: ProtocolVersion, Type: TypeAnswer, ID: id, Status: StatusCancelled}
		close(ha.done)
	})
	t.srv.registry.UnregisterConn(ha.connID)
	return ok
}

// resolve is called from httpConnWriter.WriteFrame when the registry
// produces a terminal AnswerFrame (answered/timeout/dismissed/unavailable).
// It only stores the result and unblocks Poll — it does NOT remove the
// entry from t.asks (Poll/sweepAbandoned own that), so an ask that resolves
// with nobody currently polling is still there for the next Poll to collect.
func (t *HTTPTransport) resolve(af AnswerFrame) {
	t.mu.Lock()
	ha := t.asks[af.ID]
	t.mu.Unlock()
	if ha == nil {
		return
	}
	ha.once.Do(func() {
		ha.resolvedAt = time.Now()
		ha.result = af
		close(ha.done)
	})
}

// sweepAbandoned removes entries that resolved more than staleAfter ago but
// were never collected by a Poll (the submitting caller crashed, or simply
// never came back) — otherwise an abandoned-but-answered ask lives forever.
// Entries still pending (not yet resolved) are left alone regardless of age
// — those are bounded by the ask's own registry timeout, not this sweep.
func (t *HTTPTransport) sweepAbandoned(staleAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, ha := range t.asks {
		select {
		case <-ha.done:
			if time.Since(ha.resolvedAt) > staleAfter {
				delete(t.asks, id)
				t.srv.registry.UnregisterConn(ha.connID)
			}
		default:
		}
	}
}

// RunSweeper periodically evicts abandoned-but-resolved asks (see
// sweepAbandoned) until ctx is done. Call it once, in a goroutine, alongside
// registering the HTTP handlers.
func (t *HTTPTransport) RunSweeper(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sweepAbandoned(15 * time.Minute)
		}
	}
}

// httpConnWriter is the connWriter HTTPTransport hands to handleFrame/
// handleAsk in place of a socket connection. It only cares about the
// terminal AnswerFrame (routed to HTTPTransport.resolve, keyed by the
// frame's own ID); the AckFrame handleAsk writes on successful presentation
// is a no-op here because Submit's nil error already IS that signal for the
// HTTP caller (see Submit's doc comment).
type httpConnWriter struct{ t *HTTPTransport }

func (w *httpConnWriter) WriteFrame(v any) error {
	if af, ok := v.(AnswerFrame); ok {
		w.t.resolve(af)
	}
	return nil
}

func (w *httpConnWriter) Close() error { return nil }

package slack

// socket_test.go: the Socket Mode contract (#2459 req 1) — every envelope is
// acked, the ack precedes handling, and slash commands/unknown types are
// acked and ignored.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/internal/log"
)

// runSocket starts a socketClient against the fake and returns a stop func.
func runSocket(t *testing.T, f *fakeSlack, handle func(env envelope)) func() {
	t.Helper()
	sock := newSocketClient(f.apiBase(), "xapp-fake", f.httpc, 5*time.Second, func() *log.ComponentLogger {
		return log.NewComponentLogger("slack-test")
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sock.run(ctx, handle) }()
	f.waitSocket(t)
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("socket run did not stop after cancel")
		}
	}
}

// TestSocket_AcksEveryEnvelopeType proves each envelope type — events_api,
// interactive, slash_commands and an unknown type — is acked exactly once by
// envelope_id and forwarded; the manager-level drop of slash commands and
// unknown types is proven by TestSocket_SlashAndUnknownStartNothing below.
func TestSocket_AcksEveryEnvelopeType(t *testing.T) {
	f := newFakeSlack(t)

	// The handler runs on the socket goroutine. The channel hands each type to
	// the test, so nothing is shared.
	handled := make(chan string, 16)
	stop := runSocket(t, f, func(env envelope) { handled <- env.Type })
	defer stop()

	envelopes := []envelope{
		{EnvelopeID: "e-events", Type: "events_api", Payload: json.RawMessage(`{"event":{"type":"app_mention"}}`)},
		{EnvelopeID: "e-interactive", Type: "interactive", Payload: json.RawMessage(`{"type":"block_actions"}`)},
		// Slash commands arrive URL-encoded in production; the payload is
		// opaque to A1 (acked and dropped), so any decodable blob stands in.
		{EnvelopeID: "e-slash", Type: "slash_commands", Payload: json.RawMessage(`{}`)},
		{EnvelopeID: "e-unknown", Type: "events_api_ext", Payload: json.RawMessage(`{}`)},
	}
	for _, env := range envelopes {
		f.pushEnvelope(t, env)
	}

	acked := map[string]bool{}
	for _, id := range f.ackedIDs() {
		if acked[id] {
			t.Errorf("envelope %s acked more than once", id)
		}
		acked[id] = true
	}
	for _, env := range envelopes {
		if !acked[env.EnvelopeID] {
			t.Errorf("envelope %s (%s) never acked", env.EnvelopeID, env.Type)
		}
	}
	// An ack can reach the fake before the handler runs: wait for each one.
	var got []string
	for range envelopes {
		select {
		case typ := <-handled:
			got = append(got, typ)
		case <-time.After(10 * time.Second):
			t.Fatalf("handled envelope types = %v, want every acked envelope forwarded", got)
		}
	}
}

// TestSocket_SlashAndUnknownStartNothing proves the manager drops slash
// commands and unknown envelope types after the ack: no turn, no API call —
// no foci command can be run from Slack (#2459 part B owns them).
func TestSocket_SlashAndUnknownStartNothing(t *testing.T) {
	h := newHarness(t, "helen", nil)
	before := h.fake.totalCalls()
	for _, env := range []envelope{
		{EnvelopeID: "e-slash", Type: "slash_commands", Payload: json.RawMessage(`{}`)},
		{EnvelopeID: "e-unknown", Type: "events_api_ext", Payload: json.RawMessage(`{}`)},
	} {
		h.mgr.handleEnvelope(env)
	}
	if got := h.fake.totalCalls(); got != before {
		t.Errorf("slash/unknown envelopes made %d API calls, want 0", got-before)
	}
	if envs := h.mgr.core("helen").enq.(*fakeAgent).drained(); len(envs) != 0 {
		t.Errorf("slash/unknown envelopes started %d turns", len(envs))
	}
}

// TestSocket_AckPrecedesHandling proves the ack is on the wire BEFORE the
// envelope is handled: a handler that blocks forever still gets its ack, so
// Slack never retries a payload because an agent turn is slow.
func TestSocket_AckPrecedesHandling(t *testing.T) {
	f := newFakeSlack(t)

	release := make(chan struct{})
	blocking := make(chan struct{}, 1)
	stop := runSocket(t, f, func(env envelope) {
		blocking <- struct{}{}
		<-release // hold the read loop until the ack is observed
	})
	defer stop()
	defer close(release)

	f.pushEnvelope(t, envelope{EnvelopeID: "e-block", Type: "events_api", Payload: json.RawMessage(`{}`)})

	// The ack must arrive while the handler is still blocked.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-blocking:
			// handler started; ack must already be recorded
			f.waitAck(t, "e-block")
			return
		case <-deadline:
			t.Fatal("handler never started; ack ordering untestable")
		}
	}
}

// TestSocket_DisconnectEndsLoop proves a disconnect envelope stops the loop
// cleanly (reconnect is #2459 A2).
func TestSocket_DisconnectEndsLoop(t *testing.T) {
	f := newFakeSlack(t)
	sock := newSocketClient(f.apiBase(), "xapp-fake", f.httpc, 5*time.Second, func() *log.ComponentLogger {
		return log.NewComponentLogger("slack-test")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sock.run(ctx, func(envelope) {}) }()
	f.waitSocket(t)

	f.pushEnvelope(t, envelope{EnvelopeID: "e-dc", Type: "disconnect", Payload: json.RawMessage(`{}`), Reason: "warning"})
	// The disconnect envelope carries an id and is acked first, then the
	// loop ends without an error.
	f.waitAck(t, "e-dc")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("disconnect returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not end on disconnect")
	}
}

// TestSocket_DialErrorOmitsSessionTicket proves a dial failure never echoes
// the wss:// URL: it embeds the session ticket, and Manager.StartAll logs
// the returned error text verbatim. A URL that fails to parse inside the
// dialer is the shape that quotes the full URL (including ?ticket=…) in a
// raw *url.Error.
func TestSocket_DialErrorOmitsSessionTicket(t *testing.T) {
	f := newFakeSlack(t)
	f.SocketURLOverride = "wss://%zz.invalid/socket?ticket=SECRETTICKET"
	sock := newSocketClient(f.apiBase(), "xapp-fake", f.httpc, 5*time.Second, func() *log.ComponentLogger {
		return log.NewComponentLogger("slack-test")
	})
	err := sock.run(context.Background(), func(envelope) {})
	if err == nil {
		t.Fatal("run succeeded against an unparseable socket URL, want a dial error")
	}
	if strings.Contains(err.Error(), "SECRETTICKET") {
		t.Errorf("dial error leaks the session ticket: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "dial") {
		t.Errorf("dial error lost its context: %q", err.Error())
	}
}

// TestSocket_OpenResponseCapped proves the apps.connections.open response
// is read through the shared Web API cap (maxPeerPayloadBytes): the body is
// VALID JSON when read whole but well past the cap, so truncation at the cap
// is what makes the decode fail. A client that buffered the whole body
// would hand out the socket URL and connect — caught here as run never
// returning.
func TestSocket_OpenResponseCapped(t *testing.T) {
	f := newFakeSlack(t)
	pad := strings.Repeat("x", 3*maxPeerPayloadBytes)
	f.SocketOpenRawBody = []byte(fmt.Sprintf(`{"ok":true,"url":%q,"pad":"%s"}`, f.socketURL(), pad))
	sock := newSocketClient(f.apiBase(), "xapp-fake", f.httpc, 5*time.Second, func() *log.ComponentLogger {
		return log.NewComponentLogger("slack-test")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sock.run(ctx, func(envelope) {}) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "apps.connections.open") {
			t.Errorf("oversized apps.connections.open body gave %v, want a decode error naming the method", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run is still going: the oversized apps.connections.open body was accepted in full (no read cap)")
	}
}

package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"foci/internal/log"
)

// maxPeerPayloadBytes caps any single payload read from Slack: a Socket Mode
// WebSocket frame (SetReadLimit in run) and a Web API response body (the
// read cap in api.go) are both small JSON documents; anything larger is a
// broken or hostile peer, never a payload to buffer whole.
const maxPeerPayloadBytes = 1 << 20

// envelope is one Socket Mode payload container (docs.slack.dev). Every
// envelope with an id MUST be acknowledged or Slack retries it, so the read
// loop acks before handling. Types that arrive: events_api, interactive,
// slash_commands, plus the connection-level hello and disconnect.
type envelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	Reason     string          `json:"reason"`
}

// socketClient owns the Socket Mode connection: one apps.connections.open
// (app-level token) returns a wss:// URL, which it dials and then reads
// forever. Reconnects, dedup and redaction beyond the never-log-a-token rule
// here are #2459 A2; A1 stops the loop on disconnect or read error. The
// handshake is bounded by dialTimeout (config), so a server that accepts the
// TCP connection but never completes the WebSocket handshake cannot hang the
// transport; the HTTP client's own timeout bounds apps.connections.open.
type socketClient struct {
	// web is the Web API client this connection authenticates with. It
	// carries the APP-level token (not the bot token): apps.connections.open
	// is the one method that wants it, and routing it through the shared
	// client gives open() the same response cap and error shaping as every
	// other Web API call.
	web         *webAPI
	dialTimeout time.Duration
	logger      func() *log.ComponentLogger
}

func newSocketClient(apiBase, appToken string, hc *http.Client, dialTimeout time.Duration, logger func() *log.ComponentLogger) *socketClient {
	return &socketClient{web: newWebAPI(apiBase, appToken, hc), dialTimeout: dialTimeout, logger: logger}
}

// open exchanges the app-level token for a WebSocket URL. The URL embeds a
// session ticket and is never logged: it lives only in this struct's local
// variable and the dialer's request, and every error below goes through
// webAPI.call / urlCause, which strip URLs.
func (s *socketClient) open(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := s.web.call(ctx, "apps.connections.open", nil, &out); err != nil {
		return "", fmt.Errorf("slack socket: %w", err)
	}
	if out.URL == "" {
		return "", fmt.Errorf("slack socket apps.connections.open: no url in response")
	}
	return out.URL, nil
}

// run connects, then reads envelopes until Slack disconnects the socket, the
// context is cancelled, or a read fails. Each envelope is acknowledged
// BEFORE it is handled, so a slow agent turn never causes Slack to redeliver.
// handle is called from the read loop only, so ack writes are single-writer.
// Cancellation closes the socket — gorilla reads do not watch a context —
// which unblocks the read with an error; the closer goroutine watches the
// context AND run's own return, so it never outlives a normal return.
func (s *socketClient) run(ctx context.Context, handle func(env envelope)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	wsURL, err := s.open(ctx)
	if err != nil {
		return err
	}
	dialer := &websocket.Dialer{HandshakeTimeout: s.dialTimeout}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		// urlCause: a failed dial wraps a *url.Error whose text quotes the
		// FULL wss:// URL — including the session ticket in its query —
		// which must never reach a log line.
		return fmt.Errorf("slack socket: dial: %w", urlCause(err))
	}
	defer conn.Close() //nolint:errcheck
	// Peer-payload read limit (see maxPeerPayloadBytes): gorilla would
	// otherwise buffer a larger frame whole.
	conn.SetReadLimit(maxPeerPayloadBytes)
	watchDone := make(chan struct{})
	watcherReturned := make(chan struct{})
	go func() {
		defer close(watcherReturned)
		select {
		case <-ctx.Done():
			_ = conn.Close() // unblock the read below; the deferred Close is a no-op then
		case <-watchDone:
			// run returned on its own (disconnect, read or ack error): the
			// deferred Close above already owns the socket.
		}
	}()
	defer func() {
		close(watchDone)
		<-watcherReturned
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil // cancelled: the closer goroutine's Close unblocked the read
			}
			return fmt.Errorf("slack socket: read: %w", err)
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			// Malformed payload: nothing to ack, nothing to run.
			s.logger().Warnf("slack socket: unparseable payload (%d bytes) ignored", len(data))
			continue
		}
		if env.Type == "hello" {
			continue // connection greeting; carries no envelope_id
		}
		// Ack first — every envelope, disconnect included, or Slack retries
		// the payload — then handle.
		if env.EnvelopeID != "" {
			ack, err := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err == nil {
				if err := conn.WriteMessage(websocket.TextMessage, ack); err != nil {
					return fmt.Errorf("slack socket: ack: %w", err)
				}
			}
		}
		if env.Type == "disconnect" {
			// Slack asks the app to reconnect (warning / refresh_requested /
			// link_disabled). Reconnection is #2459 A2; A1 ends the loop.
			s.logger().Errorf("slack socket: Slack disconnected the socket (reason=%q) — the Slack transport is down until foci restarts (#2459 A2 adds reconnect)", env.Reason)
			return nil
		}
		handle(env)
	}
}

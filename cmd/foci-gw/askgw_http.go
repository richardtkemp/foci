package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"foci/internal/askgw"
	"foci/internal/config"
)

// askgwPollDefaultWait / askgwPollMaxWait bound the GET /askgw/ask/{id}
// long-poll. Clamped well under the shared HTTP server's 30s ReadTimeout/
// WriteTimeout (cmd/foci-gw/main.go) so a poll always completes as a normal
// request-response cycle instead of racing (and possibly losing to) the
// server's own connection deadline — see the DESIGN FORK note on
// askgw.HTTPTransport for why this is a bounded, resumable poll rather than
// a single multi-minute held-open call.
const (
	askgwPollDefaultWait = 20 * time.Second
	askgwPollMaxWait     = 25 * time.Second
)

// setupAskgwHTTP registers the askgw HTTP transport (POST /askgw/ask, GET
// /askgw/ask/{id}, POST /askgw/ask/{id}/cancel, POST /askgw/notify) on mux,
// reusing srv (the same *askgw.Server the Unix-socket transport already
// runs — same Registry, same present/cancel/resolveSession/editMessage
// closures). Auth is whatever authMiddleware already wraps mux with
// (http.api_key) — no new auth scheme.
//
// Returns nil (and registers nothing) unless BOTH [askgw] enabled=true AND
// [askgw] http_enabled=true: http_enabled is a separate, explicit opt-in
// because http.api_key (a single bearer token) is a materially weaker gate
// than the socket transport's SO_PEERCRED UID allow-list + Unix group — an
// operator who already has askgw enabled for local socket use should not
// silently gain a network-reachable ask endpoint on upgrade.
//
// srv == nil (Unix listener failed to start, or askgw disabled) also
// disables the HTTP transport: additive-only, this never becomes the sole
// way to run askgw.
func setupAskgwHTTP(ctx context.Context, cfg *config.Config, mux *http.ServeMux, srv *askgw.Server) *askgw.HTTPTransport {
	if srv == nil || !cfg.Askgw.Enabled || !cfg.Askgw.HTTPEnabled {
		return nil
	}

	maxBytes := int64(cfg.Askgw.MaxFrameBytes)
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}

	t := askgw.NewHTTPTransport(srv)
	go t.RunSweeper(ctx)

	mux.HandleFunc("/askgw/ask", handleAskgwSubmit(t, maxBytes))
	mux.HandleFunc("/askgw/ask/", handleAskgwPollOrCancel(t))
	mux.HandleFunc("/askgw/notify", handleAskgwNotify(srv, maxBytes))

	askgwLog.Infof("HTTP transport enabled: POST /askgw/ask, GET /askgw/ask/{id}, POST /askgw/ask/{id}/cancel, POST /askgw/notify")
	return t
}

// handleAskgwNotify returns the handler for POST /askgw/notify — the fire-
// and-forget HTTP counterpart to the socket transport's `notify` frame
// (internal/askgw/server.go's handleNotify). Unlike /askgw/ask, this has no
// id/poll bookkeeping of its own: it goes straight to srv.HandleNotifyFrame,
// which looks up the notify's own embedded `id` (correlating to a
// previously-answered ask, over EITHER transport) and renders it — this
// endpoint's response only reports whether the frame itself was well-formed
// and accepted, not whether an answered ask was found to render it against
// (see docs/ASKGW-PROTOCOL.md — an unknown/expired id is logged and dropped
// server-side, there is nothing more specific to report back).
func handleAskgwNotify(srv *askgw.Server, maxBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if bodyTooLarge(err) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}

		id, ok, code, msg := srv.HandleNotifyFrame(body)
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			askgwLog.Warnf("POST /askgw/notify: %s (%s)", msg, code)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "code": code, "error": msg})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "accepted"})
	}
}

// handleAskgwSubmit returns the handler for POST /askgw/ask. Body is the
// same `ask` frame JSON the Unix-socket transport accepts. Responds 202
// immediately once the question is presented to chat (mirrors the socket's
// `ack`) — it does NOT wait for a human answer; poll GET /askgw/ask/{id}
// for that.
func handleAskgwSubmit(t *askgw.HTTPTransport, maxBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if bodyTooLarge(err) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}

		id, ok, code, msg := t.Submit(body)
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			status := http.StatusBadRequest
			if code == "duplicate_id" || code == "rejected" {
				status = http.StatusConflict
			}
			askgwLog.Warnf("POST /askgw/ask: %s (%s)", msg, code)
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "code": code, "error": msg})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": askgw.StatusPending})
	}
}

// handleAskgwPollOrCancel returns the handler for the /askgw/ask/ prefix,
// dispatching GET /askgw/ask/{id} (poll) and POST /askgw/ask/{id}/cancel
// (withdraw) by path suffix — same manual-path-parsing style as
// handleWebhook (http_handlers.go) rather than method-tagged mux patterns,
// for consistency with the rest of this file.
func handleAskgwPollOrCancel(t *askgw.HTTPTransport) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/askgw/ask/")
		id, action, _ := strings.Cut(rest, "/")
		if id == "" {
			http.Error(w, "bad request: path must be /askgw/ask/{id}[/cancel]", http.StatusBadRequest)
			return
		}

		switch {
		case action == "" && r.Method == http.MethodGet:
			handleAskgwPoll(w, r, t, id)
		case action == "cancel" && r.Method == http.MethodPost:
			handleAskgwCancel(w, t, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// handleAskgwPoll serves GET /askgw/ask/{id}?wait=<seconds>. Blocks
// server-side for up to `wait` (default askgwPollDefaultWait, clamped to
// askgwPollMaxWait) for the ask to resolve; returns {"status":"pending"} if
// it hasn't by then — the caller re-issues the same GET to keep waiting.
func handleAskgwPoll(w http.ResponseWriter, r *http.Request, t *askgw.HTTPTransport, id string) {
	wait := askgwPollDefaultWait
	if q := r.URL.Query().Get("wait"); q != "" {
		secs, err := strconv.Atoi(q)
		if err != nil || secs < 0 {
			http.Error(w, "bad wait param: must be a non-negative integer (seconds)", http.StatusBadRequest)
			return
		}
		wait = time.Duration(secs) * time.Second
	}
	if wait > askgwPollMaxWait {
		wait = askgwPollMaxWait
	}

	af, found := t.Poll(id, wait)
	if !found {
		http.Error(w, fmt.Sprintf("unknown ask id %q (never submitted, already delivered, or expired)", id), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(af)
}

// handleAskgwCancel serves POST /askgw/ask/{id}/cancel — mirrors the
// socket's `cancel` frame (withdraws a pending ask, tears down its chat
// prompt). Unlike the socket transport, an in-flight Poll on this id
// unblocks immediately with StatusCancelled rather than waiting out the
// ask's full timeout.
func handleAskgwCancel(w http.ResponseWriter, t *askgw.HTTPTransport, id string) {
	if !t.Cancel(id) {
		http.Error(w, fmt.Sprintf("unknown ask id %q", id), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": askgw.StatusCancelled})
}

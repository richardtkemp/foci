package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"foci/internal/delegator"
)

// batchPollInterval is how often pollBatchReply re-reads the message list.
// Package-level so tests can shrink it.
var batchPollInterval = time.Second

// acquireServerFn is a package-level seam so tests can exercise RunBatch's
// "no pooled server → acquire" branch without spawning a real opencode
// subprocess (stub it to pool a fake Server instead). Defaults to
// acquireServer itself — the exact function Backend.Start uses, so a batch-
// triggered spawn is indistinguishable from an interactive one: same pool,
// same key, same config-building path. Like batchPollInterval, package-level
// rather than a Backend field because RunBatch runs on a freshly-constructed,
// unstarted Backend (see BatchRunner's doc in delegator/backend.go) that has
// no per-instance test hooks.
var acquireServerFn = acquireServer

// RunBatch implements delegator.BatchRunner for opencode: an ephemeral
// session on the agent's shared per-agent server — reused if already pooled,
// or spawned if not (via acquireServerFn, the same pool/key Backend.Start
// uses) — create session → prompt_async → poll for the completed assistant
// message → delete session. There is exactly ONE server per agent shared
// between interactive sessions and batch runs; RunBatch never stands up a
// separate batch-only instance.
//
// RunBatch is a refcounted holder like any interactive session: it acquires
// (spawning the shared server if nothing is pooled — the bug this fixes:
// background nudge/consolidation on an agent with no live session used to
// hard-fail with "no running server") and releaseServers on return. So the
// server survives exactly as long as something needs it — if an interactive
// session is also attached, the batch's release just decrements and the
// server stays; if the batch was the only holder, it is reaped when the run
// completes. "Persistent" means it SHARES the per-agent pool (reuse, not a
// batch-private throwaway), NOT that a batch pins it open forever: overloading
// refcount as a persistence-pin leaked the count upward on every batch run and
// stopped the server ever idling out (corrected per Dick, 2026-07-20).
//
// env is passed as nil: BatchRequest carries no exec-bridge env (a batch run
// has no interactive session to route FOCI_SOCK/BASH_ENV for), matching the
// documented "only the first session's env takes effect" v1 limitation on
// acquireServer — the same limitation that already applies when the first
// *interactive* session pins the shared subprocess's env.
//
// Semantics notes, verified against a live 1.17.15 server (2026-07-16):
//   - The prompt request's `system` field is APPENDED alongside opencode's
//     built-in default prompt, not a replacement (see the
//     opencode-live-openapi skill) — a marker probe confirmed the supplied
//     system text is honoured. Consumers whose prompt says "use ONLY the
//     provided corpus" (nudge extraction) are unaffected by the extra
//     default text.
//   - `/api/session/{id}/wait` returns 503 immediately even mid-turn, so it
//     is not a usable completion barrier; completion is detected by polling
//     the message list for an assistant message with a non-null
//     time.completed.
//   - Model empty → the server's configured default; non-empty must be
//     "providerID/modelID" (opencode's model addressing).
func (b *Backend) RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error) {
	// cfg is built from b.cfg — the same per-agent backend_config an
	// interactive Backend for this agent receives (both come from the same
	// NewBackend factory; see cmd/foci-gw/agents_delegated.go), so a
	// batch-triggered spawn gets the real binary/hostname/port overrides,
	// not a divergent batch-only configuration. Only WorkDir is read from
	// the passed StartOptions; req.WorkDir is RunOnce's m.StartOpts.WorkDir —
	// the same agent workspace an interactive Start would use.
	cfg := b.serverConfigFromOpts(delegator.StartOptions{WorkDir: req.WorkDir})
	srv, err := acquireServerFn(req.AgentID, cfg, nil)
	if err != nil {
		return "", fmt.Errorf("opencode batch: acquire server: %w", err)
	}
	// Release when the run completes — a batch is a holder like any session. If
	// an interactive session is also attached this just decrements and the
	// server stays; if the batch was the sole holder the server is reaped (no
	// refcount leak, no server pinned open forever).
	defer releaseServer(req.AgentID, srv)
	hc := serverHTTP(srv)

	sid, err := createBatchSession(ctx, hc, srv.baseURL)
	if err != nil {
		return "", err
	}
	defer func() {
		// Best-effort reclaim on a background context: ctx may already be
		// done, and an orphaned row only costs store space until a later
		// cleanup.
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpReq, err := http.NewRequestWithContext(dctx, http.MethodDelete, srv.baseURL+"/session/"+sid, nil)
		if err != nil {
			return
		}
		if resp, err := hc.Do(httpReq); err == nil {
			_ = resp.Body.Close()
		}
	}()

	body := map[string]any{
		"parts": []map[string]any{{"type": "text", "text": req.Prompt}},
	}
	if req.SystemPrompt != "" {
		body["system"] = req.SystemPrompt
	}
	if req.Model != "" {
		prov, mid, ok := strings.Cut(req.Model, "/")
		if !ok {
			return "", fmt.Errorf("opencode batch: model %q must be providerID/modelID", req.Model)
		}
		body["model"] = map[string]string{"providerID": prov, "modelID": mid}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.baseURL+"/session/"+sid+"/prompt_async", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("opencode batch: prompt: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("opencode batch: prompt HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return pollBatchReply(ctx, hc, srv.baseURL, sid)
}

func createBatchSession(ctx context.Context, hc *http.Client, baseURL string) (string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/session", strings.NewReader(`{"title":"foci-batch"}`))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("opencode batch: create session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("opencode batch: create session HTTP %d: %s", resp.StatusCode, string(body))
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "", fmt.Errorf("opencode batch: decode session: %w", err)
	}
	if session.ID == "" {
		return "", fmt.Errorf("opencode batch: empty session id")
	}
	return session.ID, nil
}

// pollBatchReply polls the session's message list until the newest assistant
// message reports time.completed, then returns its concatenated text parts.
// The poll interval is coarse — batch runs are rare, seconds-long model
// turns; ctx bounds the total wait.
func pollBatchReply(ctx context.Context, hc *http.Client, baseURL, sid string) (string, error) {
	type part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Info struct {
			Role string `json:"role"`
			Time struct {
				Completed *float64 `json:"completed"`
			} `json:"time"`
		} `json:"info"`
		Parts []part `json:"parts"`
	}

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("opencode batch: %w", ctx.Err())
		case <-time.After(batchPollInterval):
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/session/"+sid+"/message", nil)
		if err != nil {
			return "", err
		}
		resp, err := hc.Do(httpReq)
		if err != nil {
			return "", fmt.Errorf("opencode batch: read messages: %w", err)
		}
		var msgs []message
		decodeErr := json.NewDecoder(resp.Body).Decode(&msgs)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return "", fmt.Errorf("opencode batch: decode messages: %w", decodeErr)
		}

		var last *message
		for i := range msgs {
			if msgs[i].Info.Role == "assistant" {
				last = &msgs[i]
			}
		}
		if last == nil || last.Info.Time.Completed == nil {
			continue
		}
		var b strings.Builder
		for _, p := range last.Parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return strings.TrimSpace(b.String()), nil
	}
}

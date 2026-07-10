package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"foci/internal/delegator"
)

// pooledServer returns the live pooled Server for agentID, or nil if none is
// running. Unlike acquireServer it never spawns one: fork and cleanup only make
// sense against an already-running server, because an opencode conversation
// lives in the server (its SQLite store), not on disk.
func pooledServer(agentID string) *Server {
	serverPoolMu.Lock()
	defer serverPoolMu.Unlock()
	if s, ok := serverPool[agentID]; ok && s.isAlive() {
		return s
	}
	return nil
}

func serverHTTP(s *Server) *http.Client {
	if s.http != nil {
		return s.http
	}
	return http.DefaultClient
}

// ForkSession implements delegator.BackendBrancher for opencode. It forks an
// opencode conversation via POST /session/{id}/fork (empty body = whole
// conversation) on the agent's already-running pooled server, and returns the
// new session id. opencode natively copies the parent's message history into
// the fork (verified against the live /session/{id}/fork endpoint, 1.17.15).
//
// Like the CC implementation it does not require THIS backend to be started —
// it routes to the shared per-agent server found by req.AgentID. It does
// require that server to be running (the parent session lives there); if none
// is pooled, it returns an error and the caller falls back.
func (b *Backend) ForkSession(ctx context.Context, req delegator.ForkRequest) (delegator.ForkResult, error) {
	if req.ParentSessionID == "" {
		return delegator.ForkResult{}, fmt.Errorf("opencode fork: empty parent session id")
	}
	if req.TruncateAfter > 0 {
		// Mid-conversation forks would pass a messageID to the fork endpoint;
		// foci's TruncateAfter is a message COUNT, not an id. Not supported in
		// v1 — fork the whole conversation only (parity with CC).
		return delegator.ForkResult{}, fmt.Errorf("opencode fork: TruncateAfter>0 not supported")
	}
	srv := pooledServer(req.AgentID)
	if srv == nil {
		return delegator.ForkResult{}, fmt.Errorf("opencode fork: no running server for agent %q", req.AgentID)
	}

	url := srv.baseURL + "/session/" + req.ParentSessionID + "/fork"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return delegator.ForkResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := serverHTTP(srv).Do(httpReq)
	if err != nil {
		return delegator.ForkResult{}, fmt.Errorf("opencode fork: POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return delegator.ForkResult{}, fmt.Errorf("opencode fork: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return delegator.ForkResult{}, fmt.Errorf("opencode fork: decode response: %w", err)
	}
	if session.ID == "" {
		return delegator.ForkResult{}, fmt.Errorf("opencode fork: empty forked session id")
	}
	return delegator.ForkResult{SessionID: session.ID}, nil
}

// CleanupSession implements delegator.BackendBrancher for opencode: it deletes
// an opencode session via DELETE /session/{id} on the agent's pooled server,
// reclaiming an ephemeral fork. A 404 (already gone) is treated as success. If
// no server is pooled the delete can't be performed now (the row stays in
// opencode's store until a later run when the server is up).
func (b *Backend) CleanupSession(ctx context.Context, req delegator.CleanupRequest) error {
	if req.SessionID == "" {
		return fmt.Errorf("opencode cleanup: empty session id")
	}
	srv := pooledServer(req.AgentID)
	if srv == nil {
		return fmt.Errorf("opencode cleanup: no running server for agent %q", req.AgentID)
	}

	url := srv.baseURL + "/session/" + req.SessionID
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := serverHTTP(srv).Do(httpReq)
	if err != nil {
		return fmt.Errorf("opencode cleanup: DELETE %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opencode cleanup: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

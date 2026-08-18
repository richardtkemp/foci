package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"foci/internal/delegator"
)

// RunBatch executes an ephemeral Codex thread. When called on a started
// backend it multiplexes the thread over that backend's existing app-server;
// it never launches `codex exec`. The unstarted case is retained for callers
// that construct a backend directly: it starts an app-server temporarily and
// uses the same protocol path.
// interruptBatchTurn asks codex to stop a batch thread's turn. Best-effort by
// design: it targets the batch's OWN thread id rather than going through
// Interrupt(), which resolves the caller's interactive session key and would
// aim at the wrong thread entirely. A failure here costs an orphaned turn, so
// it is logged rather than returned — the caller is already unwinding a
// cancellation and has nothing useful to do with the error.
func (b *Backend) interruptBatchTurn(threadID string) {
	if threadID == "" {
		return
	}
	p := b.process()
	p.mu.Lock()
	wr := p.writer
	p.mu.Unlock()
	if wr == nil {
		return
	}
	if err := wr.sendNotification("turn/interrupt", struct {
		ThreadID string `json:"threadId"`
	}{ThreadID: threadID}); err != nil {
		b.logWarnf("batch thread %s: turn/interrupt failed (%v) — turn may keep running", threadID, err)
	}
}

func (b *Backend) RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error) {
	startedHere := false
	if !b.IsRunning() {
		opts := b.startOpts
		opts.WorkDir = req.WorkDir
		opts.AgentID = req.AgentID
		opts.SessionKey = req.SessionKey
		opts.SystemPrompt = req.SystemPrompt
		opts.Model = req.Model
		opts.BatchOnly = true
		if err := b.Start(ctx, opts); err != nil {
			return "", fmt.Errorf("codex batch: start app-server: %w", err)
		}
		startedHere = true
	}
	// closeIfStarted releases the pool ref this call took, if any. It must
	// fire on EVERY exit path from here on -- including the early
	// validation/RPC errors below, not just the turn's own cleanup -- or a
	// failure before the turn even starts leaks the ref forever.
	closeIfStarted := func() {
		if startedHere {
			_ = b.Close()
		}
	}

	if req.WorkDir == "" {
		req.WorkDir = b.workDir
	}
	if req.SessionKey == "" {
		closeIfStarted()
		return "", fmt.Errorf("codex batch: explicit batch session key is required")
	}
	params := threadStartParams{
		Cwd:              req.WorkDir,
		Sandbox:          b.sandboxMode(),
		BaseInstructions: req.SystemPrompt,
		Ephemeral:        true,
	}
	if req.Model != "" {
		params.Model = req.Model
	}
	result, err := b.sendAndWait("thread/start", params)
	if err != nil {
		closeIfStarted()
		return "", fmt.Errorf("codex batch: thread/start: %w", err)
	}
	var tr threadResult
	if err := json.Unmarshal(result, &tr); err != nil {
		closeIfStarted()
		return "", fmt.Errorf("codex batch: parse thread/start: %w", err)
	}
	if tr.Thread.ID == "" {
		closeIfStarted()
		return "", fmt.Errorf("codex batch: thread/start returned empty thread id")
	}
	run := &batchRun{fociSessionID: req.SessionKey, done: make(chan batchResult, 1)}
	b.registerThread(req.SessionKey, tr.Thread.ID)
	b.threadMapMu.Lock()
	if b.batchRuns == nil {
		b.batchRuns = make(map[string]*batchRun)
	}
	b.batchRuns[tr.Thread.ID] = run
	b.threadMapMu.Unlock()
	// cleanup drops the thread's bookkeeping and releases the pool ref (if
	// this call attached/spawned its own app-server). It must only run once
	// the turn is truly over -- see the ctx.Done() case below for why.
	cleanup := func() {
		b.threadMapMu.Lock()
		delete(b.batchRuns, tr.Thread.ID)
		b.threadMapMu.Unlock()
		b.unregisterThread(tr.Thread.ID)
		closeIfStarted()
	}

	turnParams := turnStartParams{
		ThreadID:       tr.Thread.ID,
		Input:          []turnInput{{Type: "text", Text: req.Prompt}},
		Cwd:            req.WorkDir,
		Model:          req.Model,
		ApprovalPolicy: "never",
		// camelCase, NOT the kebab spelling used for thread/start's `sandbox`
		// field — the two enums genuinely differ (verified against a live
		// codex 0.145.0 app-server). Kebab here is rejected with JSON-RPC
		// -32600 and the batch fails at turn/start.
		SandboxPolicy: &sandboxPolicy{Type: "dangerFullAccess", NetworkAccess: true},
	}
	turnResp, err := b.sendAndWait("turn/start", turnParams)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("codex batch: turn/start: %w", err)
	}
	var started turnStartedParams
	if err := json.Unmarshal(turnResp, &started); err == nil {
		run.mu.Lock()
		run.turnID = started.Turn.ID
		run.mu.Unlock()
	}
	select {
	case res := <-run.done:
		cleanup()
		return res.text, res.err
	case <-ctx.Done():
		// The turn may still be live on the app-server: cancelling/timing out
		// HERE does not stop it server-side. Cleaning up the bookkeeping
		// immediately would unregister this thread while a turn/completed for
		// it is still in flight -- and once unregistered, dispatch() has no
		// facade to route that late notification to, so it falls through to
		// the process owner's OWN interactive handlers and silently completes
		// whatever turn is active there instead (#1570). Finish the wait for
		// the turn's real end (or the process dying, so this can't leak
		// forever) in the background, off the caller's critical path.
		//
		// Ask codex to stop the turn first. Without this the orphaned turn
		// keeps running and spending tokens with nothing tracking it, and the
		// waiter below blocks on a turn nobody asked to end -- holding this
		// facade's pool ref open, so the agent's app-server is never reaped
		// either. processDone() only bounds the wait on the process dying,
		// which is exactly the case that does NOT apply when sibling sessions
		// keep it alive. The interrupt makes codex emit the turn/completed
		// this waits on, so the normal path is prompt rather than terminal.
		b.interruptBatchTurn(tr.Thread.ID)
		go func() {
			select {
			case <-run.done:
			case <-b.processDone():
			}
			cleanup()
		}()
		return "", ctx.Err()
	}
}

// handleBatchNotification consumes every event belonging to a multiplexed
// batch thread, preventing it from being handled by the backend's
// interactive-thread state.
//
// The thread lookup is the whole decision: RunBatch shares the owner session's
// live backend, so once a notification's threadId resolves to a batchRun the
// event provably belongs to the batch and NOTHING about it concerns the owner.
// Hence the default is to consume, not to fall through — the cases below only
// exist to extract the data a batch run needs, never to decide ownership. An
// enumerate-the-known-events design was tried and leaked: unlisted methods fell
// through to the owner's interactive handlers, streaming the batch's reasoning
// into the user's chat, overwriting the owner's token usage, and renaming the
// owner's chat from thread/name/updated. A whitelist here silently reopens that
// the next time codex adds an event type.
func (b *Backend) handleBatchNotification(method string, params []byte) bool {
	var envelope struct {
		ThreadID string `json:"threadId"`
		Thread   struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil {
		return false
	}
	threadID := envelope.ThreadID
	if threadID == "" {
		threadID = envelope.Thread.ID
	}
	if threadID == "" {
		return false
	}
	b.threadMapMu.RLock()
	run := b.batchRuns[threadID]
	b.threadMapMu.RUnlock()
	if run == nil {
		return false
	}
	switch method {
	case "thread/started", "mcpServer/startupStatus/updated", "thread/status/changed":
		return true
	case "turn/started":
		var p turnStartedParams
		if json.Unmarshal(params, &p) == nil {
			run.mu.Lock()
			run.turnID = p.Turn.ID
			run.mu.Unlock()
		}
		return true
	case "item/agentMessage/delta":
		var p agentMessageDeltaParams
		if json.Unmarshal(params, &p) == nil {
			run.mu.Lock()
			run.text.WriteString(p.Delta)
			run.mu.Unlock()
		}
		return true
	case "item/completed":
		var p itemCompletedParams
		if json.Unmarshal(params, &p) == nil {
			var item itemEnvelope
			if json.Unmarshal(p.Item, &item) == nil && item.Type == "agentMessage" && item.Text != "" {
				run.mu.Lock()
				if item.Phase == "final_answer" || run.text.Len() == 0 {
					run.text.Reset()
					run.text.WriteString(item.Text)
				}
				run.mu.Unlock()
			}
		}
		return true
	case "turn/completed":
		var p turnCompletedParams
		if err := json.Unmarshal(params, &p); err != nil {
			return true
		}
		run.mu.Lock()
		text := strings.TrimSpace(run.text.String())
		run.mu.Unlock()
		result := batchResult{text: text}
		if p.Turn.Status == "failed" {
			result.err = fmt.Errorf("codex batch turn failed")
		}
		// Non-blocking: this runs on the SHARED reader goroutine, so a send
		// that blocks stalls notification processing for every session
		// multiplexed on this app-server, not just this batch. run.done is
		// buffered 1 and the first completion is the answer — a duplicate
		// turn/completed for the same thread is noise, and dropping it is the
		// correct semantics as well as the safe one (#1580).
		select {
		case run.done <- result:
		default:
		}
		return true
	default:
		// Consumed, not ignored: the event is the batch's, and the owner must
		// not see it. Logged so a codex event type we should be extracting
		// data from is visible rather than silently swallowed.
		b.logDebugf("batch thread %s: consuming unhandled notification %s", threadID, method)
		return true
	}
}

// tomlBasicString renders s as a quoted TOML basic string for app-server
// configuration overrides.
func tomlBasicString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

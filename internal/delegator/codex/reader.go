package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"foci/internal/delegator"
)

// readStream is the reader goroutine entry point. It reads JSON-RPC messages
// (one per line) from the app-server's stdout and dispatches them.
func (b *Backend) readStream(ctx context.Context, r io.Reader) {
	reader := bufio.NewReaderSize(r, 64*1024)

	for {
		select {
		case <-ctx.Done():
			b.onReaderStopped(fmt.Errorf("codex: reader cancelled: %w", ctx.Err()))
			return
		default:
		}

		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) > 0 {
			b.dispatch(trimmed)
		}
		if err != nil {
			b.onReaderStopped(fmt.Errorf("codex: reader stopped: %w", err))
			return
		}
	}
}

// dispatch parses a single JSON-RPC line and routes it to the appropriate
// handler. Incoming messages are one of:
//   - Response to our request (has id, no method) → deliver to pendingRPC
//   - Server-initiated request (has method + id) → approval/elicitation handler
//   - Notification (has method, no id) → event handler
func (b *Backend) dispatch(line []byte) {
	b.touchActivity()

	var env wireEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		b.lg.Warnf("dropping unparseable line: %v", err)
		return
	}

	// Response to our request (id present, no method).
	if env.ID != nil && env.Method == "" {
		b.handleResponse(line)
		return
	}

	// Server-initiated request (method + id — server wants us to respond).
	if env.ID != nil && env.Method != "" {
		b.handleServerRequest(line, *env.ID, env.Method)
		return
	}

	// Notification (method, no id).
	if env.Method != "" {
		b.handleNotification(line, env.Method)
		return
	}

	b.lg.Debugf("unrecognised message shape: %s", string(line))
}

// handleResponse delivers a response to a pending RPC request.
func (b *Backend) handleResponse(line []byte) {
	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		b.lg.Warnf("dropping malformed response: %v", err)
		return
	}

	b.rpcMu.Lock()
	ch, ok := b.pendingRPC[resp.ID]
	if ok {
		delete(b.pendingRPC, resp.ID)
	}
	b.rpcMu.Unlock()

	if ok {
		ch <- resp.Result
	}
}

// handleServerRequest processes server-initiated JSON-RPC requests
// (approval prompts, elicitation, etc.).
func (b *Backend) handleServerRequest(line []byte, id int64, method string) {
	switch method {
	case "item/commandExecution/requestApproval":
		b.onCommandApproval(line, id)
	case "item/fileChange/requestApproval":
		b.onFileChangeApproval(line, id)
	case "item/permissions/requestApproval":
		b.onPermissionApproval(line, id)
	default:
		b.lg.Debugf("unhandled server request: %s (id=%d)", method, id)
	}
}

// handleNotification dispatches notification messages.
func (b *Backend) handleNotification(line []byte, method string) {
	switch method {
	case "turn/started":
		b.onTurnStarted()
	case "turn/completed":
		var params turnCompletedParams
		if err := json.Unmarshal(line, &params); err != nil {
			b.lg.Warnf("dropping malformed turn/completed: %v", err)
			return
		}
		b.onTurnCompleted(&params)
	case "item/started":
		var params itemStartedParams
		if err := json.Unmarshal(line, &params); err != nil {
			b.lg.Warnf("dropping malformed item/started: %v", err)
			return
		}
		b.onItemStarted(&params)
	case "item/completed":
		var params itemCompletedParams
		if err := json.Unmarshal(line, &params); err != nil {
			b.lg.Warnf("dropping malformed item/completed: %v", err)
			return
		}
		b.onItemCompleted(&params)
	case "item/agentMessage/delta":
		var params agentMessageDeltaParams
		if err := json.Unmarshal(line, &params); err != nil {
			b.lg.Warnf("dropping malformed item/agentMessage/delta: %v", err)
			return
		}
		b.onAgentMessageDelta(&params)
	case "thread/tokenUsage/updated":
		var params tokenUsageParams
		if err := json.Unmarshal(line, &params); err != nil {
			b.lg.Warnf("dropping malformed thread/tokenUsage/updated: %v", err)
			return
		}
		b.onTokenUsage(&params)
	case "serverRequest/resolved":
		var params serverRequestResolvedParams
		if err := json.Unmarshal(line, &params); err != nil {
			b.lg.Warnf("dropping malformed serverRequest/resolved: %v", err)
			return
		}
		b.onServerRequestResolved(&params)
	case "item/reasoning/textDelta":
		var params reasoningDeltaParams
		if err := json.Unmarshal(line, &params); err != nil {
			b.lg.Warnf("dropping malformed item/reasoning/textDelta: %v", err)
			return
		}
		b.onReasoningDelta(&params)
	case "item/reasoning/summaryTextDelta":
		var params reasoningSummaryDeltaParams
		if err := json.Unmarshal(line, &params); err != nil {
			b.lg.Warnf("dropping malformed item/reasoning/summaryTextDelta: %v", err)
			return
		}
		b.onReasoningSummaryDelta(&params)
	default:
		switch method {
		case "configWarning":
			var params configWarningParams
			if err := json.Unmarshal(line, &params); err != nil {
				b.lg.Warnf("dropping malformed configWarning: %v", err)
				return
			}
			b.onConfigWarning(&params)
		case "warning":
			var params runtimeWarningParams
			if err := json.Unmarshal(line, &params); err != nil {
				b.lg.Warnf("dropping malformed warning: %v", err)
				return
			}
			b.lg.Infof("codex runtime warning: %s", params.Message)
			b.fireWarning(params.Message)
		default:
			b.lg.Debugf("unhandled notification: %s", method)
		}
	}
}

// onReaderStopped is called when the reader goroutine exits.
func (b *Backend) onReaderStopped(err error) {
	b.lg.Debugf("reader stopped: %v", err)

	b.mu.Lock()
	b.running = false
	b.mu.Unlock()

	// If a turn is still active, complete it.
	b.turnMu.Lock()
	active := b.turnActive
	b.turnMu.Unlock()
	if active {
		b.completeTurn(&delegator.TurnResult{
			Text: b.turnText.String(),
		})
	}

	// Fail any pending RPC requests.
	b.rpcMu.Lock()
	for id, ch := range b.pendingRPC {
		ch <- nil
		delete(b.pendingRPC, id)
	}
	b.rpcMu.Unlock()

	close(b.done)
}

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

func (b *Backend) dispatch(line []byte) {
	p := b.process()
	var env struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &env) == nil && env.Method != "" {
		var x struct {
			ThreadID string `json:"threadId"`
			Thread   struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		_ = json.Unmarshal(env.Params, &x)
		id := x.ThreadID
		if id == "" {
			id = x.Thread.ID
		}
		if target := p.facadeForThread(id); target != nil && target != p {
			target.dispatchLocal(line)
			return
		}
		// A subagent's child thread is NOT a foci session, so it resolves to no
		// facade and would fall through to the process owner below — handing
		// the owner the child's turn/completed (ending the parent's live turn
		// with the child's answer), the child's message deltas (streamed into
		// the user's chat as the parent's text) and the child's token usage.
		// Same class as the batch-thread leak fixed in 825ac551. The child's
		// events belong to the subagent UI, so consume them here.
		if b.subagents != nil && b.subagents.isChild(id) {
			b.handleSubagentNotification(id, env.Method, env.Params)
			return
		}
	}
	b.dispatchLocal(line)
}

func (b *Backend) dispatchLocal(line []byte) {
	b.touchActivity()

	var env wireEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		b.logWarnf("dropping unparseable line: %v", err)
		return
	}

	if env.ID != nil && env.Method == "" {
		b.handleResponse(line)
		return
	}

	if env.ID != nil && env.Method != "" {
		b.handleServerRequest(line, *env.ID, env.Method)
		return
	}

	if env.Method != "" {
		b.handleNotification(line, env.Method)
		return
	}

	b.logDebugf("unrecognised message shape: %s", string(line))
}

func (b *Backend) handleResponse(line []byte) {
	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		b.logWarnf("dropping malformed response: %v", err)
		return
	}

	b.rpcMu.Lock()
	ch, ok := b.pendingRPC[resp.ID]
	if ok {
		delete(b.pendingRPC, resp.ID)
	}
	b.rpcMu.Unlock()

	if ok {
		if resp.Error != nil {
			ch <- rpcReply{err: fmt.Errorf("codex rpc error %d: %s", resp.Error.Code, resp.Error.Message)}
		} else {
			ch <- rpcReply{result: resp.Result}
		}
	}
}

// extractParams pulls the nested "params" field from a full JSON-RPC line.
func extractParams(line []byte) json.RawMessage {
	var wrapper struct {
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(line, &wrapper)
	return wrapper.Params
}

func (b *Backend) handleServerRequest(line []byte, id int64, method string) {
	params := extractParams(line)
	switch method {
	case "item/commandExecution/requestApproval":
		b.onCommandApproval(params, id)
	case "item/fileChange/requestApproval":
		b.onFileChangeApproval(params, id)
	case "item/permissions/requestApproval":
		b.onPermissionApproval(params, id)
	default:
		b.logDebugf("unhandled server request: %s (id=%d)", method, id)
	}
}

func (b *Backend) handleNotification(line []byte, method string) {
	params := extractParams(line)
	if b.handleBatchNotification(method, params) {
		return
	}
	if len(params) == 0 {
		b.logWarnf("dropping %s: missing params", method)
		return
	}

	switch method {
	case "thread/started":
		var p threadStartedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed thread/started: %v", err)
			return
		}
		b.onThreadStarted(&p)
	case "thread/status/changed":
		var p threadStatusChangedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed thread/status/changed: %v", err)
			return
		}
		b.onThreadStatusChanged(&p)
	case "turn/started":
		b.onTurnStarted()
	case "turn/completed":
		var p turnCompletedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed turn/completed: %v", err)
			return
		}
		b.onTurnCompleted(&p)
	case "item/started":
		var p itemStartedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed item/started: %v", err)
			return
		}
		b.onItemStarted(&p)
	case "item/completed":
		var p itemCompletedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed item/completed: %v", err)
			return
		}
		b.onItemCompleted(&p)
	case "item/agentMessage/delta":
		var p agentMessageDeltaParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed item/agentMessage/delta: %v", err)
			return
		}
		b.onAgentMessageDelta(&p)
	case "thread/tokenUsage/updated":
		var p tokenUsageParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed thread/tokenUsage/updated: %v", err)
			return
		}
		b.onTokenUsage(&p)
	case "serverRequest/resolved":
		var p serverRequestResolvedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed serverRequest/resolved: %v", err)
			return
		}
		b.onServerRequestResolved(&p)
	case "item/reasoning/textDelta":
		var p reasoningDeltaParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed item/reasoning/textDelta: %v", err)
			return
		}
		b.onReasoningDelta(&p)
	case "item/reasoning/summaryTextDelta":
		var p reasoningSummaryDeltaParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.logWarnf("dropping malformed item/reasoning/summaryTextDelta: %v", err)
			return
		}
		b.onReasoningSummaryDelta(&p)
	default:
		switch method {
		case "configWarning":
			var p configWarningParams
			if err := json.Unmarshal(params, &p); err != nil {
				b.logWarnf("dropping malformed configWarning: %v", err)
				return
			}
			b.onConfigWarning(&p)
		case "warning":
			var p runtimeWarningParams
			if err := json.Unmarshal(params, &p); err != nil {
				b.logWarnf("dropping malformed warning: %v", err)
				return
			}
			// WARN, not Info — see onConfigWarning: the log level IS the
			// delivery mechanism, and log.SetWarnHook ignores Info.
			b.logWarnf("codex runtime warning: %s", p.Message)
		case "model/rerouted":
			var p struct {
				ToModel string `json:"toModel"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return
			}
			b.mu.Lock()
			b.model = p.ToModel
			b.mu.Unlock()
			b.logInfof("model rerouted to %s", p.ToModel)
		case "thread/name/updated":
			var p threadNameUpdatedParams
			if err := json.Unmarshal(params, &p); err != nil {
				return
			}
			if p.ThreadName != nil && *p.ThreadName != "" {
				b.mu.Lock()
				b.threadName = *p.ThreadName
				b.mu.Unlock()
				b.logInfof("thread name: %s", *p.ThreadName)
			}
		default:
			b.logDebugf("unhandled notification: %s", method)
		}
	}
}

func (b *Backend) onReaderStopped(err error) {
	b.logDebugf("reader stopped: %v", err)

	b.mu.Lock()
	b.running = false
	b.mu.Unlock()

	// The connection owns the reader, but each facade owns its turn state.
	b.threadMapMu.RLock()
	facades := make(map[*Backend]struct{})
	for _, f := range b.threadBackends {
		facades[f] = struct{}{}
	}
	b.threadMapMu.RUnlock()
	facades[b] = struct{}{}
	for f := range facades {
		f.turnMu.Lock()
		active := f.turnActive
		text := f.turnText.String()
		f.turnMu.Unlock()
		if active {
			f.completeTurn(&delegator.TurnResult{Text: text})
		}
	}

	b.rpcMu.Lock()
	for id, ch := range b.pendingRPC {
		ch <- rpcReply{} // nil result → sendAndWait reports "process exited"
		delete(b.pendingRPC, id)
	}
	b.rpcMu.Unlock()

	if b.done != nil {
		close(b.done)
	}
}

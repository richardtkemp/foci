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
	b.touchActivity()

	var env wireEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		b.lg.Warnf("dropping unparseable line: %v", err)
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

	b.lg.Debugf("unrecognised message shape: %s", string(line))
}

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
		b.lg.Debugf("unhandled server request: %s (id=%d)", method, id)
	}
}

func (b *Backend) handleNotification(line []byte, method string) {
	params := extractParams(line)
	if len(params) == 0 {
		b.lg.Warnf("dropping %s: missing params", method)
		return
	}

	switch method {
	case "turn/started":
		b.onTurnStarted()
	case "turn/completed":
		var p turnCompletedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.lg.Warnf("dropping malformed turn/completed: %v", err)
			return
		}
		b.onTurnCompleted(&p)
	case "item/started":
		var p itemStartedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.lg.Warnf("dropping malformed item/started: %v", err)
			return
		}
		b.onItemStarted(&p)
	case "item/completed":
		var p itemCompletedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.lg.Warnf("dropping malformed item/completed: %v", err)
			return
		}
		b.onItemCompleted(&p)
	case "item/agentMessage/delta":
		var p agentMessageDeltaParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.lg.Warnf("dropping malformed item/agentMessage/delta: %v", err)
			return
		}
		b.onAgentMessageDelta(&p)
	case "thread/tokenUsage/updated":
		var p tokenUsageParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.lg.Warnf("dropping malformed thread/tokenUsage/updated: %v", err)
			return
		}
		b.onTokenUsage(&p)
	case "serverRequest/resolved":
		var p serverRequestResolvedParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.lg.Warnf("dropping malformed serverRequest/resolved: %v", err)
			return
		}
		b.onServerRequestResolved(&p)
	case "item/reasoning/textDelta":
		var p reasoningDeltaParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.lg.Warnf("dropping malformed item/reasoning/textDelta: %v", err)
			return
		}
		b.onReasoningDelta(&p)
	case "item/reasoning/summaryTextDelta":
		var p reasoningSummaryDeltaParams
		if err := json.Unmarshal(params, &p); err != nil {
			b.lg.Warnf("dropping malformed item/reasoning/summaryTextDelta: %v", err)
			return
		}
		b.onReasoningSummaryDelta(&p)
		default:
			switch method {
			case "configWarning":
				var p configWarningParams
				if err := json.Unmarshal(params, &p); err != nil {
					b.lg.Warnf("dropping malformed configWarning: %v", err)
					return
				}
				b.onConfigWarning(&p)
			case "warning":
				var p runtimeWarningParams
				if err := json.Unmarshal(params, &p); err != nil {
					b.lg.Warnf("dropping malformed warning: %v", err)
					return
				}
				b.lg.Infof("codex runtime warning: %s", p.Message)
				b.fireWarning(p.Message)
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
			b.lg.Infof("model rerouted to %s", p.ToModel)
		case "thread/name/updated":
			var p threadNameUpdatedParams
			if err := json.Unmarshal(params, &p); err != nil {
				return
			}
			if p.ThreadName != nil && *p.ThreadName != "" {
				b.mu.Lock()
				b.threadName = *p.ThreadName
				b.mu.Unlock()
				b.lg.Infof("thread name: %s", *p.ThreadName)
			}
			default:
				b.lg.Debugf("unhandled notification: %s", method)
			}
	}
}

func (b *Backend) onReaderStopped(err error) {
	b.lg.Debugf("reader stopped: %v", err)

	b.mu.Lock()
	b.running = false
	b.mu.Unlock()

	b.turnMu.Lock()
	active := b.turnActive
	b.turnMu.Unlock()
	if active {
		b.completeTurn(&delegator.TurnResult{
			Text: b.turnText.String(),
		})
	}

	b.rpcMu.Lock()
	for id, ch := range b.pendingRPC {
		ch <- nil
		delete(b.pendingRPC, id)
	}
	b.rpcMu.Unlock()

	close(b.done)
}

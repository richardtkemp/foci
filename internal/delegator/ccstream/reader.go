package ccstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"foci/internal/log"
)

// Handler receives typed messages from the CC stdout reader.
// Methods are called sequentially from the reader goroutine — no concurrent
// dispatch. Implementations do not need internal synchronisation for these calls.
type Handler interface {
	OnAssistant(msg *AssistantMessage)
	OnResult(msg *ResultMessage)
	OnPermissionRequest(msg *PermissionRequest)
	OnElicitationRequest(msg *ElicitationRequest)
	OnControlResponse(raw json.RawMessage)
	OnControlCancelRequest(reqID string)
	OnToolProgress(msg *ToolProgressMessage)
	OnStreamEvent(raw json.RawMessage)
	OnRateLimit(msg *RateLimitEvent)
	OnKeepAlive()
	OnSystem(subtype string, raw json.RawMessage)
	OnReaderStopped(err error)
}

// Reader reads NDJSON from CC's stdout and dispatches to a Handler.
// Run blocks until EOF or context cancellation.
type Reader struct {
	r       io.Reader
	handler Handler
	lg      *log.ComponentLogger
}

// NewReader creates a Reader that reads from r and dispatches to handler. The
// reader borrows the handler's session/agent-scoped logger when it exposes one
// (the *Backend does), so per-stream parse-drop lines are attributable to the
// owning agent rather than a bare [ccstream].
func NewReader(r io.Reader, handler Handler) *Reader {
	lg := log.NewComponentLogger("ccstream")
	if h, ok := handler.(interface{ logger() *log.ComponentLogger }); ok {
		lg = h.logger()
	}
	return &Reader{
		r:       r,
		handler: handler,
		lg:      lg,
	}
}

// maxTokenSize is the maximum line size the scanner will accept.
// Tool results can be large, so we use 1MB instead of bufio's default 64KB.
const maxTokenSize = 1 << 20 // 1MB

// Run is the blocking read loop. It reads lines from the underlying reader,
// unmarshals each as NDJSON, and dispatches to the appropriate handler method.
// It returns when the reader reaches EOF, the context is cancelled, or a
// scanner error occurs.
func (rd *Reader) Run(ctx context.Context) {
	scanner := bufio.NewScanner(rd.r)
	scanner.Buffer(make([]byte, 0, maxTokenSize), maxTokenSize)

	for {
		// Check context before blocking on the next line. A cancelled
		// reader is still a reader exit — notify the handler so the
		// backend's bookkeeping (running=false, in-flight turn cleanup)
		// runs even when shutdown is initiated by Close() rather than by
		// the subprocess itself dying.
		select {
		case <-ctx.Done():
			rd.handler.OnReaderStopped(fmt.Errorf("ccstream: reader stopped: %w", ctx.Err()))
			return
		default:
		}

		if !scanner.Scan() {
			// Always notify the handler when the reader exits.
			// scanner.Err() is nil on clean EOF (process exited),
			// non-nil on read errors (broken pipe, etc.).
			// Both cases mean the subprocess is gone — in-flight turns
			// must be completed and the backend marked as dead.
			err := scanner.Err()
			if err == nil {
				err = io.EOF
			}
			rd.handler.OnReaderStopped(fmt.Errorf("ccstream: reader stopped: %w", err))
			return
		}

		line := scanner.Bytes()
		rd.dispatch(line)
	}
}

// controlRequestEnvelope extracts the subtype from a control_request's inner
// request object for discrimination before full unmarshal.
type controlRequestEnvelope struct {
	Request struct {
		Subtype string `json:"subtype"`
	} `json:"request"`
}

// cancelEnvelope extracts the request_id from a control_cancel_request.
type cancelEnvelope struct {
	RequestID string `json:"request_id"`
}

// dispatch unmarshals a single NDJSON line and calls the appropriate handler
// method.
//
// A per-line unmarshal failure logs and skips that line (P1-9). It must NOT call
// OnReaderStopped: the scanner is still alive and the CC subprocess is still
// running, so finalizing the backend here would mark a live process dead, leak
// the subprocess (Close early-returns once running=false), and respawn a
// duplicate CC. A single malformed line (a CC schema change, or a stray
// non-protocol line on fd 1 from a hook/MCP server) must be survivable.
// OnReaderStopped is reserved for the genuine scanner-exit paths in Run.
func (rd *Reader) dispatch(line []byte) {
	// Step 1: Discriminate on Type (and optionally Subtype).
	var env StdoutEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		rd.lg.Warnf("dropping unparseable stdout line (envelope): %v", err)
		return
	}

	switch env.Type {
	case "assistant":
		var msg AssistantMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			rd.lg.Warnf("dropping malformed assistant line: %v", err)
			return
		}
		rd.handler.OnAssistant(&msg)

	case "result":
		var msg ResultMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			rd.lg.Warnf("dropping malformed result line: %v", err)
			return
		}
		rd.handler.OnResult(&msg)

	case "control_request":
		var crEnv controlRequestEnvelope
		if err := json.Unmarshal(line, &crEnv); err != nil {
			rd.lg.Warnf("dropping malformed control_request envelope: %v", err)
			return
		}
		switch crEnv.Request.Subtype {
		case "can_use_tool":
			var msg PermissionRequest
			if err := json.Unmarshal(line, &msg); err != nil {
				rd.lg.Warnf("dropping malformed permission_request: %v", err)
				return
			}
			rd.lg.Debugf("received permission request req_id=%s tool=%s", msg.RequestID, msg.Request.ToolName)
			rd.handler.OnPermissionRequest(&msg)
		case "elicitation":
			var msg ElicitationRequest
			if err := json.Unmarshal(line, &msg); err != nil {
				rd.lg.Warnf("dropping malformed elicitation: %v", err)
				return
			}
			rd.lg.Debugf("received elicitation req_id=%s server=%s mode=%s",
				msg.RequestID, msg.Request.McpServerName, msg.Request.Mode)
			rd.handler.OnElicitationRequest(&msg)
		default:
			rd.lg.Debugf("unknown control_request subtype %q", crEnv.Request.Subtype)
		}

	case "control_response":
		rd.handler.OnControlResponse(json.RawMessage(copyBytes(line)))

	case "control_cancel_request":
		var ce cancelEnvelope
		if err := json.Unmarshal(line, &ce); err != nil {
			rd.lg.Warnf("dropping malformed control_cancel_request: %v", err)
			return
		}
		rd.handler.OnControlCancelRequest(ce.RequestID)

	case "tool_progress":
		var msg ToolProgressMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			rd.lg.Warnf("dropping malformed tool_progress: %v", err)
			return
		}
		rd.handler.OnToolProgress(&msg)

	case "stream_event":
		rd.handler.OnStreamEvent(json.RawMessage(copyBytes(line)))

	case "system":
		rd.handler.OnSystem(env.Subtype, json.RawMessage(copyBytes(line)))

	// Intentionally ignored — protocol informational types.
	case "user":
		// Replay messages, only emitted with --replay-user-messages —
		// which is the echo-of-our-input feature, not a surfacing of
		// CC's internal tool_result generation. Tool results from CC's
		// own tool execution never reach stdout; the ccstream path has
		// no way to fire OnToolEnd (the cctmux session-file watcher
		// fires it from parsed JSONL instead).
	case "keep_alive":
		// Heartbeat — touch activity so the idle/timeout tracker knows the
		// stream is alive. NOTE: CC never sends keep_alive in --pipe mode
		// (only on WebSocket transports), so this branch is effectively
		// dead code. See OnKeepAlive comment for details.
		rd.handler.OnKeepAlive()
	case "tool_use_summary":
		// Informational summary of tool use.
	case "auth_status":
		// Authentication status, informational.
	case "rate_limit_event":
		var msg RateLimitEvent
		if err := json.Unmarshal(line, &msg); err != nil {
			rd.lg.Warnf("dropping malformed rate_limit_event: %v", err)
			return
		}
		rd.handler.OnRateLimit(&msg)

	default:
		rd.lg.Debugf("unknown message type %q", env.Type)
	}
}

// copyBytes returns a copy of b, decoupled from the scanner's buffer which
// is reused on the next Scan call. This is necessary for json.RawMessage
// values that escape into handler calls.
func copyBytes(b []byte) []byte {
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

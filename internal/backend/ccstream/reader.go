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
	OnControlResponse(raw json.RawMessage)
	OnControlCancelRequest(reqID string)
	OnToolProgress(msg *ToolProgressMessage)
	OnStreamEvent(raw json.RawMessage)
	OnSystem(subtype string, raw json.RawMessage)
	OnError(err error)
}

// Reader reads NDJSON from CC's stdout and dispatches to a Handler.
// Run blocks until EOF or context cancellation.
type Reader struct {
	r       io.Reader
	handler Handler
}

// NewReader creates a Reader that reads from r and dispatches to handler.
func NewReader(r io.Reader, handler Handler) *Reader {
	return &Reader{
		r:       r,
		handler: handler,
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
		// Check context before blocking on the next line.
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !scanner.Scan() {
			// scanner.Err() returns nil on clean EOF.
			if err := scanner.Err(); err != nil {
				rd.handler.OnError(fmt.Errorf("ccstream: scanner: %w", err))
			}
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
func (rd *Reader) dispatch(line []byte) {
	// Step 1: Discriminate on Type (and optionally Subtype).
	var env StdoutEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		rd.handler.OnError(fmt.Errorf("ccstream: unmarshal envelope: %w", err))
		return
	}

	switch env.Type {
	case "assistant":
		var msg AssistantMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			rd.handler.OnError(fmt.Errorf("ccstream: unmarshal assistant: %w", err))
			return
		}
		rd.handler.OnAssistant(&msg)

	case "result":
		var msg ResultMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			rd.handler.OnError(fmt.Errorf("ccstream: unmarshal result: %w", err))
			return
		}
		rd.handler.OnResult(&msg)

	case "control_request":
		var crEnv controlRequestEnvelope
		if err := json.Unmarshal(line, &crEnv); err != nil {
			rd.handler.OnError(fmt.Errorf("ccstream: unmarshal control_request envelope: %w", err))
			return
		}
		switch crEnv.Request.Subtype {
		case "can_use_tool":
			var msg PermissionRequest
			if err := json.Unmarshal(line, &msg); err != nil {
				rd.handler.OnError(fmt.Errorf("ccstream: unmarshal permission_request: %w", err))
				return
			}
			log.Debugf("ccstream", "received permission request req_id=%s tool=%s", msg.RequestID, msg.Request.ToolName)
			rd.handler.OnPermissionRequest(&msg)
		default:
			log.Debugf("ccstream", "unknown control_request subtype %q", crEnv.Request.Subtype)
		}

	case "control_response":
		rd.handler.OnControlResponse(json.RawMessage(copyBytes(line)))

	case "control_cancel_request":
		var ce cancelEnvelope
		if err := json.Unmarshal(line, &ce); err != nil {
			rd.handler.OnError(fmt.Errorf("ccstream: unmarshal control_cancel_request: %w", err))
			return
		}
		rd.handler.OnControlCancelRequest(ce.RequestID)

	case "tool_progress":
		var msg ToolProgressMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			rd.handler.OnError(fmt.Errorf("ccstream: unmarshal tool_progress: %w", err))
			return
		}
		rd.handler.OnToolProgress(&msg)

	case "stream_event":
		rd.handler.OnStreamEvent(json.RawMessage(copyBytes(line)))

	case "system":
		rd.handler.OnSystem(env.Subtype, json.RawMessage(copyBytes(line)))

	// Intentionally ignored — protocol informational types.
	case "user":
		// Replay messages, only emitted with --replay-user-messages.
	case "keep_alive":
		// Heartbeat, no action needed.
	case "tool_use_summary":
		// Informational summary of tool use.
	case "auth_status":
		// Authentication status, informational.
	case "rate_limit_event":
		// Rate limit info; api_retry (system subtype) is more actionable.

	default:
		log.Debugf("ccstream", "unknown message type %q", env.Type)
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

package main

import (
	"errors"
	"fmt"
	"net/http"

	"foci/internal/memory"
)

// todoDoneRequest is POST /todo/done's body. The reason travels base64-
// encoded in a field whose name SAYS so, because a close reason is whatever
// bytes the caller passed and a JSON string would silently rewrite invalid
// UTF-8 to U+FFFD; base64 carries every byte unchanged. The encoding being
// in the field name is what lets the handler refuse a plain "reason" string
// below instead of decoding it — a silently decoded string would store
// garbage bytes (or fail mid-decode) rather than tell the caller they used
// the wrong field.
type todoDoneRequest struct {
	Agent string `json:"agent"` // required: todo ids are per agent, name one
	ID    int64  `json:"id"`    // required: the todo to close
	// ReasonBase64 is the base64 form: decoded and stored verbatim as
	// close_reason; required.
	Reason []byte `json:"reason_base64"`
	// PlainReason is a "reason" sent as a plain JSON string. It is never
	// read as a value: its presence only makes the handler refuse, naming
	// the field the caller should have used.
	PlainReason string `json:"reason"`
}

// todoDoneMaxBodyBytes caps POST /todo/done's request body. A close reason
// is argv-borne: the kernel allows at most 128 KiB in one argument
// (MAX_ARG_STRLEN) and 2 MiB — 3 MiB with a large stack rlimit — across
// the whole command line, and the wire carries it base64-expanded (4/3).
// 8 MiB therefore admits every reason a command line can deliver while
// staying a bounded body; the CLI-side and documented limits are the
// kernel's, not this cap.
const todoDoneMaxBodyBytes = 8 << 20

// handleTodoDone returns the handler for POST /todo/done: mark one todo of
// the named agent done with a reason, exactly as the agent todo tool's
// "complete" action does — the same TodoStore.Transition writer, the same
// reason-required refusal (memory.RequireCloseReason). This is the route a
// service running as the gateway's user but outside any agent exec — the
// factory's landing, whose environment has no FOCI_SOCK/BASH_ENV and
// therefore no foci_todo — uses to close its tickets.
//
// Unlike the other control endpoints the agent is REQUIRED: resolveAgent
// silently picks the first configured agent for an empty id, which would
// close the WRONG agent's todo N. Every check below runs before any write,
// so a refused request changes nothing.
func handleTodoDone(resolveAgent agentResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req todoDoneRequest
		if !decodeCappedJSONAt(w, r, &req, todoDoneMaxBodyBytes) {
			return
		}
		if req.Agent == "" {
			http.Error(w, "agent is required: todo ids are per agent, name one", http.StatusBadRequest)
			return
		}
		inst, ok := resolveAgent(req.Agent)
		if !ok {
			httpLog.Warnf("POST /todo/done: unknown agent %q", req.Agent)
			http.Error(w, fmt.Sprintf("unknown agent: %q", req.Agent), http.StatusBadRequest)
			return
		}
		if req.ID <= 0 {
			http.Error(w, "id is required (a positive todo id)", http.StatusBadRequest)
			return
		}
		if req.PlainReason != "" {
			http.Error(w, `reason must be base64-encoded in the "reason_base64" field (a plain JSON string would not carry every byte verbatim)`, http.StatusBadRequest)
			return
		}
		if err := memory.RequireCloseReason("done", string(req.Reason)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if inst.cc.TodoStore == nil {
			http.Error(w, fmt.Sprintf("todo store not configured for agent %q", req.Agent), http.StatusServiceUnavailable)
			return
		}
		if err := inst.cc.TodoStore.Transition(inst.id, req.ID, "done", string(req.Reason)); err != nil {
			var nf *memory.TodoNotFoundError
			if errors.As(err, &nf) {
				httpLog.Warnf("POST /todo/done: agent=%s %v", inst.id, err)
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			// The reason is free text and stays out of the log line.
			httpLog.Errorf("POST /todo/done: agent=%s close todo #%d: %v", inst.id, req.ID, err)
			http.Error(w, fmt.Sprintf("close todo #%d: %v", req.ID, err), http.StatusInternalServerError)
			return
		}
		// Same text shape the todo tool answers for a transition (#N: done).
		httpLog.Infof("POST /todo/done: agent=%s todo #%d closed", inst.id, req.ID)
		writeJSONResponse(w, fmt.Sprintf("#%d: done", req.ID))
	}
}

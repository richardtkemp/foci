package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

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
		target := p.facadeForThread(id)
		if target != nil && target != p {
			target.dispatchLocal(line)
			return
		}
		// A thread that is no foci session resolves to no facade. Codex's own
		// subagents are off (agents.enabled=false, #2839) and its auto_review
		// guardian threads send nothing on the wire (live 0.166.0, #2840), so
		// nothing is expected here but a fork's or a resume's early lines.
		if target == nil && id != "" {
			// Nothing says whose it is. It still goes to the owner (dropping could strand a
			// prompt nobody answers), and is reported lost, naming where it
			// came from — but only if the thread is still unclaimed after
			// unclaimedGrace (noteUnclaimed). codex sends a fork's and a
			// resumed thread's first notifications before the response that
			// lets foci register the thread, so an immediate WARN fired on
			// every fork and resume (#2700).
			// thread/started is never reported: a facade's own thread/started
			// can arrive before the thread/start or thread/resume response that
			// registers it, and a fork's before its branch facade resumes it;
			// the owner treats a thread/started that is not its root as a no-op.
			// Nor is a thread foci forked whose session has not resumed it yet
			// (a depth leaf that has not run, #2720): it is known, not lost. Its
			// lines are the fork's own lifecycle, which dispatchLost consumes.
			// Nor is a thread foci released (release.go), which is unmapped by
			// design: codex still sends its status changes until it unloads it,
			// and then thread/closed.
			if env.Method == "thread/closed" {
				defer p.forgetReleased(id)
			}
			if env.Method != "thread/started" && env.Method != "thread/closed" && !p.isPendingFork(id) && !p.isReleased(id) {
				b.noteUnclaimed(id, env.Method, time.Now())
			}
			// Whatever the owner shows the user from this line (message text,
			// a turn error, an approval prompt) is marked with where it came
			// from (lostMarked), so it never reads as the owner's own; and
			// nothing in it may change the owner's own turn or accounting
			// (dispatchLost). Only the reader goroutine runs dispatch, so the
			// field needs no lock.
			b.lostThread = id
			b.dispatchLost(line, env.ID, env.Method, env.Params)
			b.lostThread = ""
			return
		}
	}
	b.dispatchLocal(line)
}

// lostMarked prefixes text with the origin marker while the owner is handling
// a lost line (one whose thread matches no session), and
// returns it unchanged otherwise. Reader goroutine only.
func (b *Backend) lostMarked(text string) string {
	if b.lostThread == "" {
		return text
	}
	return delegator.RedirectedOrigin("codex thread "+b.lostThread,
		"it matches no session, so it is shown here") + "\n\n" + text
}

// dispatchLost handles a lost line (its thread matches no session) on the
// app-server owner. Only what the user must see reaches the
// owner, marked with its origin (lostMarked); nothing touches the owner's own
// turn or accounting state. Before this, the line went through dispatchLocal
// like the owner's own, so a lost turn/completed ended the owner's live turn
// with another thread's result, a lost turn/started reopened it, its token
// usage was booked to the owner's turn, its agent message was appended to the
// owner's turn text, and its tool items entered the owner's item cache and
// tool counts.
//
//   - A server request (an approval prompt) goes through the ordinary request
//     path, so it is prompted (marked), auto-decided, or declined when nobody
//     can answer — codex is never left waiting — and the answer goes back over
//     the shared connection with codex's own id. Its pending entry is keyed by
//     that id, which is the only owner state it adds.
//   - Message text: deltas stream live (marked on the first one) and the
//     completed message is shown (marked) — neither enters the owner's turn
//     text.
//   - A failed turn's error is shown as a marked note; the owner's turn is
//     not ended and its last-error status is not touched.
//   - Everything else (turn/started, turn/completed, token usage, thread
//     status/name, tool and reasoning items, model reroute, ...) is consumed:
//     it only describes the lost thread's own lifecycle, and the WARN in
//     dispatch already records it. Tool items are consumed rather than shown
//     because a tool event has no text to carry the marker, so it would read
//     as the owner's own tool call and count toward its turn.
func (b *Backend) dispatchLost(line []byte, id *int64, method string, params json.RawMessage) {
	if id != nil {
		b.dispatchLocal(line)
		return
	}
	se := b.sessionEvents.Load()
	switch method {
	case "item/agentMessage/delta":
		var p agentMessageDeltaParams
		if json.Unmarshal(params, &p) == nil {
			b.onAgentMessageDelta(&p)
		}
	case "item/completed":
		var p itemCompletedParams
		if json.Unmarshal(params, &p) != nil {
			return
		}
		var item itemEnvelope
		if json.Unmarshal(p.Item, &item) != nil || item.Type != "agentMessage" {
			return
		}
		b.lostDeltaThread = ""
		if item.Text != "" && se != nil && se.OnText != nil {
			se.OnText(b.lostMarked(item.Text))
		}
	case "turn/completed":
		var p turnCompletedParams
		if json.Unmarshal(params, &p) != nil || p.Turn.Status != "failed" {
			return
		}
		msg := "unknown error"
		if p.Turn.Error != nil && strings.TrimSpace(p.Turn.Error.Message) != "" {
			msg = strings.TrimSpace(p.Turn.Error.Message)
		}
		if se != nil && se.OnText != nil {
			se.OnText(b.lostMarked(turnFailedText(msg)))
		}
	default:
		b.logDebugf("lost %s for thread %s consumed", method, b.lostThread)
	}
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
	case "thread/closed":
		// Codex unloaded a thread this facade still maps. A thread foci
		// releases is unmapped in the same step (release.go), long before
		// codex's 60s unload, so this one was not released by foci; its next
		// turn/start would fail. Say so.
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &p)
		b.logWarnf("codex unloaded thread %s while a session still uses it (thread/closed)", p.ThreadID)
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
		case "account/rateLimits/updated":
			b.onRateLimitsUpdated(params)
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

	// No thread can be registered any more: report the unclaimed ones now.
	b.stopUnclaimed()

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

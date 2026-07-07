package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"foci/internal/app/fap"
	"foci/internal/log"
)

// ErrNoLiveDevice is returned by InvokeTool when the agent has no connected
// app device (no live WebSocket). The tool surfaces this verbatim so the agent
// can decide whether to retry, ask the user to open the app, or fall back.
var ErrNoLiveDevice = errors.New("app: no live device for agent")

// pendingToolCall is a waiting InvokeTool caller, keyed by InvocationID.
// The result channel is buffered (1) so a late completion arriving after the
// caller's ctx has been cancelled can be delivered without blocking the
// dispatcher goroutine.
type pendingToolCall struct {
	result chan fap.ToolResult
}

// toolCallRegistry tracks InvocationID → pending caller. Methods are goroutine-
// safe; one registry lives on the Hub.
type toolCallRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingToolCall
}

func newToolCallRegistry() *toolCallRegistry {
	return &toolCallRegistry{pending: make(map[string]*pendingToolCall)}
}

// register adds a pending caller and returns it (with its result channel) plus
// a deregister func the caller MUST defer. The deregister is idempotent.
func (r *toolCallRegistry) register(invocationID string) (*pendingToolCall, func()) {
	p := &pendingToolCall{result: make(chan fap.ToolResult, 1)}
	r.mu.Lock()
	r.pending[invocationID] = p
	r.mu.Unlock()
	return p, func() {
		r.mu.Lock()
		// Don't clobber a re-registered id (extremely unlikely given ULID).
		if cur, ok := r.pending[invocationID]; ok && cur == p {
			delete(r.pending, invocationID)
		}
		r.mu.Unlock()
	}
}

// deliver routes an inbound ToolResult to its waiting caller. Returns false if
// no caller is waiting (timed out, cancelled, or unsolicited) — the caller is
// then responsible for logging or dropping.
func (r *toolCallRegistry) deliver(res fap.ToolResult) bool {
	r.mu.Lock()
	p := r.pending[res.InvocationID]
	r.mu.Unlock()
	if p == nil {
		return false
	}
	select {
	case p.result <- res:
		return true
	default:
		// Channel already has a result (e.g. completed arriving after pending).
		// Drop the duplicate — first writer wins, the deregister cleans up.
		return false
	}
}

// InvokeTool sends a tool.invoke frame to any live device for agentID and
// awaits the matching tool.result. The ctx bounds the wait; on expiry the
// pending entry is deregistered and a ctx.Err() is returned.
//
// "Any live device": if the agent's default chat binding has a connected
// client, use that; otherwise scan the agent's bindings for any connected
// client. v1 assumes one device per agent — multiple connected devices race
// on whichever client the scan finds first.
func (h *Hub) InvokeTool(ctx context.Context, agentID, tool, action string, args json.RawMessage) (fap.ToolResult, error) {
	client := h.liveClientForAgent(agentID)
	if client == nil {
		return fap.ToolResult{}, ErrNoLiveDevice
	}

	invocationID := fap.NewULID()
	if args == nil {
		args = json.RawMessage("{}")
	}
	pending, deregister := h.toolCalls.register(invocationID)
	defer deregister()

	client.sendRaw(fap.ToolInvoke{
		InvocationID: invocationID,
		Tool:         tool,
		Action:       action,
		Args:         args,
	})
	log.Debugf("app.tool", "invoked tool=%s action=%s inv=%s agent=%s", tool, action, invocationID, agentID)

	select {
	case res := <-pending.result:
		return res, nil
	case <-ctx.Done():
		return fap.ToolResult{}, ctx.Err()
	}
}

// liveClientForAgent returns any connected wsClient for agentID, preferring the
// default chat binding's client (most likely to be the user's active device).
// Returns nil if the agent has no live socket.
func (h *Hub) liveClientForAgent(agentID string) *wsClient {
	if b := h.defaultChatBinding(agentID); b != nil {
		if c := b.snapshotClient(); c != nil {
			return c
		}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, b := range h.convs {
		if b.agentID != agentID {
			continue
		}
		if c := b.snapshotClient(); c != nil {
			return c
		}
	}
	return nil
}

// snapshotClient returns any one of the binding's currently-attached live
// sockets (multi-device: a conversation may have several). Returns nil when no
// socket is attached. The tool.invoke routing uses this when picking any one
// device to invoke on — it doesn't yet care which device answers.
func (b *convBinding) snapshotClient() *wsClient {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.clients {
		return c
	}
	return nil
}

// deliverToolResult is called from the inbound dispatcher when a ToolResult
// frame arrives. Routes to the waiting InvokeTool caller if any.
func (h *Hub) deliverToolResult(r fap.ToolResult) {
	if !h.toolCalls.deliver(r) {
		log.Debugf("app.tool", "ToolResult with no waiter: inv=%s status=%s", r.InvocationID, r.Status)
	}
}

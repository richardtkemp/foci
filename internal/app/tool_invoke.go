package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"foci/internal/fap"
)

// ErrNoLiveDevice is returned by InvokeTool when no connected app device can
// run the requested tool (none connected, or only ones without that handler). The tool surfaces this verbatim so the agent
// can decide whether to retry, ask the user to open the app, or fall back.
var ErrNoLiveDevice = errors.New("app: no connected device can run this tool")

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
	// Buffered >1: a single invocation can deliver several frames (one or more
	// "pending" keepalives then a terminal). The InvokeTool loop drains them in
	// order; the buffer keeps a terminal from being dropped if it races in
	// before the loop re-selects.
	p := &pendingToolCall{result: make(chan fap.ToolResult, 8)}
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

// InvokeTool sends a tool.invoke frame to a live device that can run tool and
// awaits the matching tool.result. The ctx bounds the wait; on expiry the
// pending entry is deregistered and a ctx.Err() is returned. agentID is only
// logged: a device tool belongs to the device, not to any agent's conversations.
func (h *Hub) InvokeTool(ctx context.Context, agentID, tool, action string, args json.RawMessage) (fap.ToolResult, error) {
	client := h.clientForTool(tool)
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
	app_toolLog.Debugf("invoked tool=%s action=%s inv=%s agent=%s", tool, action, invocationID, agentID)

	// A "pending" frame is a keepalive, NOT a terminal: the device's task
	// overran its own sync window but is still running. Keep waiting (bounded by
	// ctx) for the terminal "completed"/"error" rather than returning — and
	// dropping — the eventual result. Without this, the device's 10s pending
	// short-circuits the server's larger wait budget and slow tasks vanish.
	sawPending := false
	for {
		select {
		case res := <-pending.result:
			if res.Status == fap.ToolStatusPending {
				sawPending = true
				app_toolLog.Debugf("tool pending inv=%s agent=%s — awaiting terminal result", invocationID, agentID)
				continue
			}
			return res, nil
		case <-ctx.Done():
			// Budget exhausted. If we saw a keepalive, surface pending (task is
			// still running on-device) rather than a bare context error, so the
			// caller can tell the agent it didn't finish in time.
			if sawPending {
				return fap.ToolResult{InvocationID: invocationID, Status: fap.ToolStatusPending}, nil
			}
			return fap.ToolResult{}, ctx.Err()
		}
	}
}

// featureToolPrefix + a tool name is the ClientHello capability a client
// advertises for each device-side tool handler it registers ("tool:android" for
// the Tasker bridge). The desktop client registers none.
const featureToolPrefix = "tool:"

// canRunTool reports whether this socket's device can execute tool. Clients
// built before the "tool:" capability advertise no tools at all; for those the
// Android build is the only one with the "android" (Tasker) handler, and its
// ClientInfo.OS ("Android <release>") is the only thing that tells it apart
// from the desktop build sharing the same app name and feature list.
func (c *wsClient) canRunTool(tool string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.features[featureToolPrefix+tool]; ok {
		return true
	}
	return tool == "android" && strings.HasPrefix(c.os, "Android")
}

// clientForTool picks the live socket to run tool on, or nil if none can (#1079).
// Choosing from every live socket rather than whichever the socket map yields
// first is the fix: the desktop client shares the same bindings and answered
// half the Tasker calls with "not supported on desktop". Among capable devices
// the most recent hello wins — the device the user last opened the app on — so
// the choice is deterministic rather than map-order.
func (h *Hub) clientForTool(tool string) *wsClient {
	var best *wsClient
	var bestAt time.Time
	for _, c := range h.snapshotClients() {
		if !c.canRunTool(tool) {
			continue
		}
		c.mu.Lock()
		at := c.helloAt
		c.mu.Unlock()
		if best == nil || at.After(bestAt) {
			best, bestAt = c, at
		}
	}
	return best
}

// deliverToolResult is called from the inbound dispatcher when a ToolResult
// frame arrives. Routes to the waiting InvokeTool caller if any.
func (h *Hub) deliverToolResult(r fap.ToolResult) {
	if !h.toolCalls.deliver(r) {
		app_toolLog.Debugf("ToolResult with no waiter: inv=%s status=%s", r.InvocationID, r.Status)
	}
}

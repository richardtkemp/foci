package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"foci/internal/app/fap"
	"foci/internal/platform"
)

func TestToolCallRegistry_RegisterAndDeliver(t *testing.T) {
	r := newToolCallRegistry()
	p, dereg := r.register("inv-1")
	defer dereg()

	res := fap.ToolResult{InvocationID: "inv-1", Status: "completed", Output: json.RawMessage(`{"x":1}`)}
	if !r.deliver(res) {
		t.Fatal("deliver returned false for a registered caller")
	}
	select {
	case got := <-p.result:
		if got.Status != "completed" {
			t.Errorf("status: got %q want completed", got.Status)
		}
		if string(got.Output) != `{"x":1}` {
			t.Errorf("output: got %q", got.Output)
		}
	case <-time.After(time.Second):
		t.Fatal("no result delivered")
	}
}

func TestToolCallRegistry_DeliverNoWaiter(t *testing.T) {
	r := newToolCallRegistry()
	if r.deliver(fap.ToolResult{InvocationID: "orphan", Status: "completed"}) {
		t.Error("deliver returned true for an unknown InvocationID")
	}
}

func TestToolCallRegistry_DeregisterRemoves(t *testing.T) {
	r := newToolCallRegistry()
	_, dereg := r.register("inv-2")
	dereg()
	if r.deliver(fap.ToolResult{InvocationID: "inv-2", Status: "completed"}) {
		t.Error("deliver returned true after deregister")
	}
}

func TestToolCallRegistry_DeliversPendingThenTerminal(t *testing.T) {
	// A single invocation can carry a "pending" keepalive followed by a terminal
	// result. The registry's buffered channel must queue BOTH (in order) so the
	// InvokeTool loop can drain the keepalive and return on the terminal — a
	// buffer of 1 would drop the terminal and lose the result.
	r := newToolCallRegistry()
	p, _ := r.register("inv-3")
	if !r.deliver(fap.ToolResult{InvocationID: "inv-3", Status: "pending"}) {
		t.Fatal("pending deliver returned false")
	}
	if !r.deliver(fap.ToolResult{InvocationID: "inv-3", Status: "completed"}) {
		t.Fatal("terminal deliver returned false; the keepalive must not drop the terminal")
	}
	if got := <-p.result; got.Status != "pending" {
		t.Errorf("first frame: got %q want pending", got.Status)
	}
	if got := <-p.result; got.Status != "completed" {
		t.Errorf("second frame: got %q want completed", got.Status)
	}
}

// TestInvokeTool_NoLiveDevice asserts the no-device path returns the sentinel
// rather than panicking. The hub here has no bindings, so defaultChatBinding
// and the scan both return nil.
func TestInvokeTool_NoLiveDevice(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	_, err := h.InvokeTool(context.Background(), "ghost-agent", "android", "list", nil)
	if !errors.Is(err, ErrNoLiveDevice) {
		t.Errorf("expected ErrNoLiveDevice, got %v", err)
	}
}

// TestInvokeTool_HappyPath wires a fake wsClient as the agent's binding client,
// invokes a tool, and asserts:
//   - the invoke frame reaches the client's send queue
//   - a delivered ToolResult reaches the waiting caller
//   - the registry cleans up after the call returns
func TestInvokeTool_HappyPath(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()

	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")
	if b == nil {
		t.Fatal("ensureBinding returned nil")
	}
	client := fakeClient()
	b.attach(client)

	type outcome struct {
		res fap.ToolResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := h.InvokeTool(context.Background(), agentID, "android", "list", nil)
		done <- outcome{res, err}
	}()

	var invokeFrame fap.ToolInvoke
	select {
	case wire := <-client.send:
		// ToolInvoke is a server→app frame; the inbound (client→server) decoder
		// doesn't know about it, so parse the envelope directly.
		var env struct {
			T string          `json:"t"`
			D json.RawMessage `json:"d"`
		}
		if err := json.Unmarshal(wire, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if env.T != fap.TypeToolInvoke {
			t.Fatalf("expected tool.invoke, got %q", env.T)
		}
		if err := json.Unmarshal(env.D, &invokeFrame); err != nil {
			t.Fatalf("decode ToolInvoke payload: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ToolInvoke frame never reached the client")
	}

	h.deliverToolResult(fap.ToolResult{
		InvocationID: invokeFrame.InvocationID,
		Status:       "completed",
		Output:       json.RawMessage(`{"tasks":[]}`),
	})

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("InvokeTool returned err: %v", o.err)
		}
		if o.res.Status != "completed" {
			t.Errorf("status: got %q want completed", o.res.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("InvokeTool never returned after result delivery")
	}

	if h.toolCalls.deliver(fap.ToolResult{InvocationID: invokeFrame.InvocationID, Status: "completed"}) {
		t.Error("registry still had a waiter after InvokeTool returned")
	}
}

// TestInvokeTool_PendingKeepaliveThenCompleted asserts a "pending" frame is
// treated as a keepalive: InvokeTool keeps waiting and returns the later
// terminal result rather than returning (and dropping) at the pending.
func TestInvokeTool_PendingKeepaliveThenCompleted(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()

	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")
	client := fakeClient()
	b.attach(client)

	type outcome struct {
		res fap.ToolResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := h.InvokeTool(context.Background(), agentID, "android", "perform", nil)
		done <- outcome{res, err}
	}()

	// Pull the invoke frame to learn the invocation id.
	var inv fap.ToolInvoke
	select {
	case wire := <-client.send:
		var env struct {
			T string          `json:"t"`
			D json.RawMessage `json:"d"`
		}
		if err := json.Unmarshal(wire, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if err := json.Unmarshal(env.D, &inv); err != nil {
			t.Fatalf("decode ToolInvoke: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ToolInvoke never reached client")
	}

	// Keepalive first, then the real result.
	h.deliverToolResult(fap.ToolResult{InvocationID: inv.InvocationID, Status: fap.ToolStatusPending})
	h.deliverToolResult(fap.ToolResult{InvocationID: inv.InvocationID, Status: fap.ToolStatusCompleted, Output: json.RawMessage(`{"ok":true}`)})

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("InvokeTool err: %v", o.err)
		}
		if o.res.Status != fap.ToolStatusCompleted {
			t.Errorf("status: got %q want completed (pending should not have terminated the wait)", o.res.Status)
		}
		if string(o.res.Output) != `{"ok":true}` {
			t.Errorf("output: got %q", o.res.Output)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("InvokeTool never returned the terminal result after a pending keepalive")
	}
}

// TestInvokeTool_PendingThenTimeout asserts that if only a "pending" keepalive
// arrives and the ctx expires, InvokeTool surfaces status=pending (not a bare
// ctx error) so the caller can tell the agent the task is still running.
func TestInvokeTool_PendingThenTimeout(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()

	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")
	client := fakeClient()
	b.attach(client)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan fap.ToolResult, 1)
	go func() {
		res, _ := h.InvokeTool(ctx, agentID, "android", "perform", nil)
		done <- res
	}()

	var inv fap.ToolInvoke
	select {
	case wire := <-client.send:
		var env struct {
			T string          `json:"t"`
			D json.RawMessage `json:"d"`
		}
		_ = json.Unmarshal(wire, &env)
		_ = json.Unmarshal(env.D, &inv)
	case <-time.After(time.Second):
		t.Fatal("ToolInvoke never reached client")
	}

	h.deliverToolResult(fap.ToolResult{InvocationID: inv.InvocationID, Status: fap.ToolStatusPending})

	select {
	case res := <-done:
		if res.Status != fap.ToolStatusPending {
			t.Errorf("status: got %q want pending on timeout-after-keepalive", res.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("InvokeTool didn't return after ctx timeout")
	}
}

func TestInvokeTool_CtxCancel(t *testing.T) {
	h := newHub(platform.ProviderDeps{})
	defer h.Close()
	const agentID = "arnix"
	h.setupAgent(platform.AgentConnectionParams{AgentID: agentID})
	b := h.ensureBinding(nil, agentID, "conv-1")
	client := fakeClient()
	b.attach(client)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.InvokeTool(ctx, agentID, "android", "list", nil)
		done <- err
	}()

	<-client.send // let the invoke frame arrive
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("InvokeTool didn't return after ctx cancel")
	}
}

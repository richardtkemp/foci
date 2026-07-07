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

func TestToolCallRegistry_LateCompletionDropped(t *testing.T) {
	r := newToolCallRegistry()
	p, _ := r.register("inv-3")
	if !r.deliver(fap.ToolResult{InvocationID: "inv-3", Status: "pending"}) {
		t.Fatal("first deliver returned false")
	}
	if r.deliver(fap.ToolResult{InvocationID: "inv-3", Status: "completed"}) {
		t.Error("second deliver returned true; expected dropped (first wins)")
	}
	got := <-p.result
	if got.Status != "pending" {
		t.Errorf("got %q, expected pending (first writer wins)", got.Status)
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

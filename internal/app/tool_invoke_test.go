package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"foci/internal/fap"
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

// toolClient is a live socket (registered on the hub, as serveWS does) whose
// device advertises the Android Tasker handler.
func toolClient(h *Hub) *wsClient {
	c := fakeClient()
	c.features = featureSet([]string{featureToolPrefix + "android"})
	h.addClient(c)
	return c
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
	client := toolClient(h)
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
	client := toolClient(h)
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
	client := toolClient(h)
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
	client := toolClient(h)
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

// helloClient registers a live socket on h and runs a real ClientHello through
// dispatchInbound, so the capability/OS the selection reads comes from the same
// path production takes. Drains the hello's own replies.
func helloClient(t *testing.T, h *Hub, deviceID, os string, features ...string) *wsClient {
	t.Helper()
	c := fakeClientFor(h)
	h.addClient(c)
	d, err := json.Marshal(fap.ClientHello{
		Client:   fap.ClientInfo{App: "foci-android", OS: os, Version: "1", DeviceID: deviceID},
		Features: features,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.dispatchInbound(c, []byte(`{"t":"hello","id":"`+deviceID+`","d":`+string(d)+`}`))
	drainEnv(t, c)
	return c
}

// invokedClient starts an InvokeTool for "android" and reports which of the
// candidate sockets received the tool.invoke frame (nil = none), then cancels.
func invokedClient(t *testing.T, h *Hub, agentID string, candidates ...*wsClient) (*wsClient, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := h.InvokeTool(ctx, agentID, "android", "list", nil)
		errc <- err
	}()
	deadline := time.After(time.Second)
	for {
		for _, c := range candidates {
			select {
			case wire := <-c.send:
				var env struct {
					T string `json:"t"`
				}
				_ = json.Unmarshal(wire, &env)
				if env.T == fap.TypeToolInvoke {
					cancel()
					<-errc
					return c, nil
				}
			default:
			}
		}
		select {
		case err := <-errc:
			return nil, err
		case <-deadline:
			t.Fatal("InvokeTool neither sent a frame nor returned")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestInvokeTool_RoutesOnlyToCapableDevice is #1079: with a desktop and an
// Android client both connected, the Tasker bridge picked whichever socket map
// iteration yielded first, so roughly half the calls reached the desktop and
// failed "not supported on desktop". Only a client that can run the tool may be
// picked; repeat enough times that a coin-flip choice cannot pass.
func TestInvokeTool_RoutesOnlyToCapableDevice(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	b := h.ensureBinding(nil, "ag", "conv-1")
	desktop := helloClient(t, h, "mac", "Mac OS X 15.5", "interactiveBatch")
	phone := helloClient(t, h, "phone", "Android 15", "interactiveBatch", "tool:android")
	b.attach(desktop)
	b.attach(phone)
	for i := 0; i < 20; i++ {
		got, err := invokedClient(t, h, "ag", desktop, phone)
		if err != nil {
			t.Fatalf("run %d: InvokeTool err %v", i, err)
		}
		if got != phone {
			t.Fatalf("run %d: tool.invoke went to the desktop, want the Android client", i)
		}
	}
}

// TestInvokeTool_LegacyAndroidClientByOS: an Android build from before the
// "tool:<name>" capability advertises no tool features, so its OS is the only
// discriminator. It must still be reachable, and still preferred to a desktop.
func TestInvokeTool_LegacyAndroidClientByOS(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	desktop := helloClient(t, h, "mac", "Mac OS X 15.5", "interactiveBatch")
	phone := helloClient(t, h, "phone", "Android 14", "interactiveBatch")
	for i := 0; i < 20; i++ {
		got, err := invokedClient(t, h, "ag", desktop, phone)
		if err != nil {
			t.Fatalf("run %d: InvokeTool err %v", i, err)
		}
		if got != phone {
			t.Fatalf("run %d: tool.invoke went to the desktop, want the legacy Android client", i)
		}
	}
}

// TestInvokeTool_NoCapableDevice: a connected desktop is not a device that can
// run the Tasker bridge, so the call fails fast with ErrNoLiveDevice instead of
// round-tripping to a client that can only answer "not supported".
func TestInvokeTool_NoCapableDevice(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	b := h.ensureBinding(nil, "ag", "conv-1")
	desktop := helloClient(t, h, "mac", "Mac OS X 15.5", "interactiveBatch")
	b.attach(desktop)
	got, err := invokedClient(t, h, "ag", desktop)
	if got != nil {
		t.Fatal("tool.invoke was sent to the desktop")
	}
	if !errors.Is(err, ErrNoLiveDevice) {
		t.Fatalf("err = %v, want ErrNoLiveDevice", err)
	}
}

// TestInvokeTool_PrefersMostRecentlyConnectedDevice: with two capable devices
// the choice is deterministic — the one that said hello last, i.e. the one the
// user most recently opened the app on.
func TestInvokeTool_PrefersMostRecentlyConnectedDevice(t *testing.T) {
	h := newTestHub()
	registerBareAgent(h, "ag")
	older := helloClient(t, h, "tablet", "Android 14", "tool:android")
	time.Sleep(2 * time.Millisecond)
	newer := helloClient(t, h, "phone", "Android 15", "tool:android")
	for i := 0; i < 20; i++ {
		got, err := invokedClient(t, h, "ag", older, newer)
		if err != nil {
			t.Fatalf("run %d: InvokeTool err %v", i, err)
		}
		if got != newer {
			t.Fatalf("run %d: tool.invoke went to the older device", i)
		}
	}
}

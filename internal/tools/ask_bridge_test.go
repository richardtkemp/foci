package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"foci/internal/question"
)

// capturePresenter records the session key the tool was invoked with and the
// onResponse callback, so the bridge round-trip test can drive an answer after
// the real socket dispatch.
type capturePresenter struct {
	mu         sync.Mutex
	calls      int
	sessionKey string
	onResponse func(string)
}

func (c *capturePresenter) present(sessionKey, _ /*msgID*/, _ /*text*/, _ /*summary*/ string, _ []question.Choice, onResponse func(string)) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.sessionKey = sessionKey
	c.onResponse = onResponse
	return ""
}

func (c *capturePresenter) answer(data string) {
	c.mu.Lock()
	cb := c.onResponse
	c.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

// TestAsk_ExecBridgeRoundTrip is an in-process integration test that drives the
// `ask` tool through the REAL exec bridge: a unix-socket request (the same
// transport the generated foci_ask shell function uses) dispatches to
// tool.Execute, and the calling session key set on the bridge context must flow
// through SessionKeyFromContext into the presenter and the delivered answer
// batch. This covers the bridge → Execute → session-routing wiring that the
// direct-Execute unit tests (ask_test.go) bypass.
func TestAsk_ExecBridgeRoundTrip(t *testing.T) {
	const sk = "clutch/c777/1000"

	pres := &capturePresenter{}
	var (
		dmu         sync.Mutex
		delivered   []string
		deliveredSK []string
	)
	deliver := func(sessionKey, msg string) {
		dmu.Lock()
		defer dmu.Unlock()
		delivered = append(delivered, msg)
		deliveredSK = append(deliveredSK, sessionKey)
	}

	reg := NewRegistry()
	tool, _ := NewAskTool(pres.present, nil, deliver, nil, "test")
	reg.Register(tool)

	bridge, err := NewExecBridge(reg, WithSessionKey(context.Background(), sk))
	if err != nil {
		t.Fatalf("NewExecBridge: %v", err)
	}
	defer bridge.Close()

	req := `{"tool":"ask","params":{"questions":[{"question":"Pick?","header":"Pick","options":[{"label":"Red"},{"label":"Blue"}]}]}}`
	result, errMsg := callBridge(t, bridge.SockPath(), req)
	if errMsg != "" {
		t.Fatalf("bridge returned error: %s", errMsg)
	}
	if !strings.Contains(result, `"status":"asked"`) {
		t.Fatalf("expected async ack, got %q", result)
	}

	// The session key on the bridge ctx must have reached Execute → presenter.
	if pres.calls != 1 {
		t.Fatalf("presenter called %d times, want 1", pres.calls)
	}
	if pres.sessionKey != sk {
		t.Errorf("presenter sessionKey = %q, want %q (SessionKeyFromContext not propagated through the bridge)", pres.sessionKey, sk)
	}

	// Simulate the user clicking "Blue"; the batch must deliver to the same session.
	pres.answer("qa:1")

	dmu.Lock()
	defer dmu.Unlock()
	if len(delivered) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(delivered))
	}
	if deliveredSK[0] != sk {
		t.Errorf("answer batch delivered to %q, want %q", deliveredSK[0], sk)
	}
	if !strings.Contains(delivered[0], "Blue") {
		t.Errorf("delivered batch missing the chosen answer:\n%s", delivered[0])
	}
}

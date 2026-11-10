package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

// slotGate is a one-slot CallGate that counts admissions and releases.
type slotGate struct {
	slot      chan struct{}
	admitted  int
	released  int
	inFlight  int
	maxFlight int
}

func newSlotGate() *slotGate { return &slotGate{slot: make(chan struct{}, 1)} }

func (g *slotGate) gate(ctx context.Context, _ *MessageRequest) (func(), error) {
	select {
	case g.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	g.admitted++
	g.inFlight++
	g.maxFlight = max(g.maxFlight, g.inFlight)
	return func() {
		g.inFlight--
		g.released++
		<-g.slot
	}, nil
}

// TestCallGate_SendAdmitsOnceAndReleases: a call through a gated handle is
// admitted once (not again by sendWithRetry on the pinned client) and its
// slot is freed when it returns, on success and on failure.
func TestCallGate_SendAdmitsOnceAndReleases(t *testing.T) {
	g := newSlotGate()
	c := &swapTestClient{name: "A"}
	h := NewSwappableClient(c)
	h.SetGate(g.gate)

	if _, err := Send(context.Background(), h, &MessageRequest{Model: "m"}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if g.admitted != 1 || g.released != 1 {
		t.Fatalf("admitted %d released %d, want 1 and 1", g.admitted, g.released)
	}

	c.onSend = func(int) error { return &APIError{StatusCode: 401} } // not retried, no fallback
	if _, err := Send(context.Background(), h, &MessageRequest{Model: "m"}, nil, nil, nil, nil); err == nil {
		t.Fatal("want the 401")
	}
	if g.admitted != 2 || g.released != 2 || g.inFlight != 0 {
		t.Fatalf("after a failure: admitted %d released %d in flight %d", g.admitted, g.released, g.inFlight)
	}
}

// TestCallGate_GateErrorIsNotSent: a gate that refuses (its ctx ended while
// waiting) returns the error and the client is never called.
func TestCallGate_GateErrorIsNotSent(t *testing.T) {
	c := &swapTestClient{name: "A"}
	h := NewSwappableClient(c)
	h.SetGate(func(context.Context, *MessageRequest) (func(), error) { return nil, context.Canceled })
	_, err := Send(context.Background(), h, &MessageRequest{Model: "m"}, nil, nil, nil, nil)
	if !errors.Is(err, context.Canceled) || c.sends != 0 {
		t.Fatalf("err %v sends %d, want context.Canceled and no send", err, c.sends)
	}
}

// TestCallGate_FallbackOnSameEndpointDoesNotDeadlock: the primary call frees
// its slot before the fallback chain, so a hop on the same one-slot endpoint
// (its client from the provider, or the caller's own client) is admitted.
func TestCallGate_FallbackOnSameEndpointDoesNotDeadlock(t *testing.T) {
	for _, viaProvider := range []bool{true, false} {
		g := newSlotGate()
		mc := &fallbackMockClient{responses: []fallbackMockResponse{
			{err: &APIError{StatusCode: 529}},
			{resp: &MessageResponse{Content: TextContent("fb")}},
		}}
		h := NewSwappableClient(mc)
		h.SetGate(g.gate)
		var cp ClientProvider
		if viaProvider {
			cp = &fallbackMockClientProvider{clients: map[string]Client{"local:openai": h}}
		}
		fallbackFn := func(model string) (string, string, string, bool) {
			return "fb-model", "local", "openai", model == "m"
		}

		done := make(chan error, 1)
		go func() {
			_, err := Send(context.Background(), h, &MessageRequest{Model: "m"}, nil, fallbackFn, cp, nil)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("viaProvider=%v: %v", viaProvider, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("viaProvider=%v: the fallback hop waited on the primary's slot", viaProvider)
		}
		if g.admitted != 2 || g.released != 2 || g.maxFlight != 1 {
			t.Errorf("viaProvider=%v: admitted %d released %d max in flight %d, want 2, 2, 1",
				viaProvider, g.admitted, g.released, g.maxFlight)
		}
	}
}

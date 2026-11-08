package provider

import (
	"context"
	"testing"
)

// swapTestClient is a Client that can also stream, and runs onSend on each
// SendMessage call before answering with its name.
type swapTestClient struct {
	name    string
	sends   int
	streams int
	onSend  func(n int) error
}

func (c *swapTestClient) SendMessage(_ context.Context, _ *MessageRequest) (*MessageResponse, error) {
	c.sends++
	if c.onSend != nil {
		if err := c.onSend(c.sends); err != nil {
			return nil, err
		}
	}
	return &MessageResponse{Content: TextContent(c.name)}, nil
}

func (c *swapTestClient) StreamMessage(_ context.Context, _ *MessageRequest, _ *StreamHandler) (*MessageResponse, error) {
	c.streams++
	return &MessageResponse{Content: TextContent(c.name)}, nil
}

func (c *swapTestClient) CountTokens(context.Context, *MessageRequest) (int, error) { return 0, nil }
func (c *swapTestClient) IsCachingAvailable() bool                                  { return false }

// TestSwappable_NextCallUsesSwappedClient: a swap reaches the next call
// through the same handle.
func TestSwappable_NextCallUsesSwappedClient(t *testing.T) {
	a, b := &swapTestClient{name: "A"}, &swapTestClient{name: "B"}
	h := NewSwappableClient(a)
	req := &MessageRequest{Model: "m"}
	if resp, _ := Send(context.Background(), h, req, nil, nil, nil, nil); TextOf(resp.Content) != "A" {
		t.Fatalf("before swap: %q", TextOf(resp.Content))
	}
	h.Swap(b)
	if resp, _ := Send(context.Background(), h, req, nil, nil, nil, nil); TextOf(resp.Content) != "B" {
		t.Fatalf("after swap: %q, want B", TextOf(resp.Content))
	}
}

// TestSwappable_StreamingSeenThroughHandle: the provider layer's optional
// interfaces (here StreamingClient) are checked on the pinned concrete
// client, not on the wrapper, so a handle does not turn a streaming call into
// a non-streaming one.
func TestSwappable_StreamingSeenThroughHandle(t *testing.T) {
	a := &swapTestClient{name: "A"}
	h := NewSwappableClient(a)
	if _, err := Send(context.Background(), h, &MessageRequest{Model: "m"}, &StreamHandler{}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if a.streams != 1 || a.sends != 0 {
		t.Fatalf("streams=%d sends=%d, want 1 stream and 0 sends", a.streams, a.sends)
	}
}

// TestSwappable_CallPinsItsClientAcrossRetry: a swap during a call does not
// move that call's retry (here the 400 unsupported-param retry) to the new
// client.
func TestSwappable_CallPinsItsClientAcrossRetry(t *testing.T) {
	b := &swapTestClient{name: "B"}
	var h *SwappableClient
	a := &swapTestClient{name: "A", onSend: func(n int) error {
		if n == 1 {
			h.Swap(b) // the config edit lands mid-call
			return &APIError{StatusCode: 400, Body: "thinking is not supported"}
		}
		return nil
	}}
	h = NewSwappableClient(a)
	req := &MessageRequest{Model: "m", Thinking: &ThinkingConfig{}}
	resp, err := Send(context.Background(), h, req, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := TextOf(resp.Content); got != "A" || b.sends != 0 {
		t.Fatalf("retry served by %q (B sends=%d), want A with no B sends", got, b.sends)
	}
}

// TestPin_NonHandlePassesThrough: Pin leaves an ordinary client alone.
func TestPin_NonHandlePassesThrough(t *testing.T) {
	a := &swapTestClient{name: "A"}
	if Pin(a) != Client(a) {
		t.Fatal("Pin changed a non-handle client")
	}
}

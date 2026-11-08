package provider

import (
	"context"
	"sync/atomic"
)

// SwappableClient is a Client whose underlying client can be replaced while
// callers hold it (#2980). The client registry hands one out per
// endpoint:format pair; agents, sessions, spawns and commands keep that one
// handle for the life of the process, and a live [endpoints.*] config edit
// swaps the client behind it. The next call reaches the new client with no
// restart.
//
// A call pins the client it starts with: Send and sendWithRetry resolve the
// handle through Pin once, at entry, so every retry, extended retry and
// stream of that call uses one concrete client, and a swap mid-call does not
// move it. Pinning also makes the provider layer's optional-interface checks
// (StreamingClient, selfRetryingClient, retryableClient, endpointDescriber)
// see the concrete client, not this wrapper.
type SwappableClient struct {
	cur atomic.Pointer[clientBox]
}

// clientBox lets an interface value live in an atomic.Pointer.
type clientBox struct{ c Client }

// NewSwappableClient returns a handle that serves c until the first Swap.
func NewSwappableClient(c Client) *SwappableClient {
	s := &SwappableClient{}
	s.cur.Store(&clientBox{c: c})
	return s
}

// Current returns the client the next call will use.
func (s *SwappableClient) Current() Client { return s.cur.Load().c }

// Swap makes c the client for every call that starts after it returns.
// Calls already running keep the client they pinned.
func (s *SwappableClient) Swap(c Client) { s.cur.Store(&clientBox{c: c}) }

// SendMessage implements Client on the current client.
func (s *SwappableClient) SendMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	return s.Current().SendMessage(ctx, req)
}

// CountTokens implements Client on the current client.
func (s *SwappableClient) CountTokens(ctx context.Context, req *MessageRequest) (int, error) {
	return s.Current().CountTokens(ctx, req)
}

// IsCachingAvailable implements Client on the current client.
func (s *SwappableClient) IsCachingAvailable() bool { return s.Current().IsCachingAvailable() }

// Pin returns the concrete client a call should use from start to finish:
// the current client behind a SwappableClient, or c itself otherwise. Use it
// before a type assertion on a client that may be a registry handle.
func Pin(c Client) Client {
	if s, ok := c.(*SwappableClient); ok {
		return s.Current()
	}
	return c
}

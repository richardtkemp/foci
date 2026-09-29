package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/provider"
)

// testClient is a mock provider.Client for unit tests.
// It implements Client, StreamingClient, and retryableClient (via RetryBaseDelay)
// so that provider.Send works with fast retries.
type testClient struct {
	handler func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error)
}

func (c *testClient) SendMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	return c.handler(ctx, req)
}

func (c *testClient) StreamMessage(ctx context.Context, req *provider.MessageRequest, sh *provider.StreamHandler) (*provider.MessageResponse, error) {
	resp, err := c.handler(ctx, req)
	if err != nil {
		return nil, err
	}
	if sh != nil {
		text := provider.TextOf(resp.Content)
		if text != "" && sh.OnTextDelta != nil {
			sh.OnTextDelta(text)
		}
		for _, block := range resp.Content {
			if block.Type == "thinking" && block.Thinking != "" && sh.OnThinkingDelta != nil {
				sh.OnThinkingDelta(block.Thinking)
			}
		}
	}
	return resp, nil
}

func (c *testClient) CountTokens(_ context.Context, _ *provider.MessageRequest) (int, error) {
	return 0, nil
}

func (c *testClient) IsCachingAvailable() bool { return true }

// The following methods satisfy the provider.retryableClient interface (structural typing)
// so that provider.Send uses 1ms backoff instead of 2s in tests.
func (c *testClient) RetryBaseDelay() time.Duration         { return time.Millisecond }
func (c *testClient) OverloadBaseDelay() time.Duration      { return 5 * time.Millisecond }
func (c *testClient) OverloadMaxDuration() time.Duration    { return 500 * time.Millisecond }
func (c *testClient) ServerErrorMaxDuration() time.Duration { return 100 * time.Millisecond }
func (c *testClient) OnRetrySuccess()                       {}
func (c *testClient) WaitForRecovery() <-chan struct{}      { return make(chan struct{}) }

// newTestClient creates a test client from a response handler (success path).
func newTestClient(handler func(req *provider.MessageRequest) *provider.MessageResponse) *testClient {
	return &testClient{handler: func(_ context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
		return handler(req), nil
	}}
}

// newTestClientWithError creates a test client that can return errors.
func newTestClientWithError(handler func(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error)) *testClient {
	return &testClient{handler: handler}
}

// openTestLedger opens a cost ledger in a temp dir and makes it the live one
// (accounting.Live) for the test.
func openTestLedger(t *testing.T) *accounting.Ledger {
	t.Helper()
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{
		OnAlarm: func(a accounting.Alarm) { t.Errorf("ledger alarm: %+v", a) },
	})
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() {
		accounting.SetLive(nil)
		_ = l.Close()
	})
	return l
}

// ledgerCalls reads every call booked in l.
func ledgerCalls(t *testing.T, l *accounting.Ledger) []accounting.CallRow {
	t.Helper()
	rows, err := l.Calls(time.Time{})
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return rows
}

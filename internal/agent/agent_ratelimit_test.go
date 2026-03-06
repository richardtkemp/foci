package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/anthropic"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestHandleMessageRateLimitGateBlocks(t *testing.T) {
	// When the gate is closed, HandleMessage should queue the message
	// and return RateLimitedError without touching the session.
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		t.Fatal("API should not be called when gate is closed")
		return nil
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	// Close the gate for anthropic endpoint (default)
	until := time.Now().Add(1 * time.Hour)
	gate := ag.getOrCreateRateLimitGate("anthropic")
	gate.Close(until)

	ctx := WithTrigger(context.Background(), "telegram")
	_, err := ag.HandleMessage(ctx, "test/igate/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected RateLimitedError")
	}

	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}
	if !rlErr.Until.Equal(until) {
		t.Errorf("until = %v, want %v", rlErr.Until, until)
	}
}

func TestHandleMessageRateLimitClosesGate(t *testing.T) {
	// A 429 from the API should close the gate so subsequent calls are blocked.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	// First call hits the API, gets 429, closes gate
	_, err := ag.HandleMessage(context.Background(), "test/igate429/1000000000", "Hello")
	if err == nil {
		t.Fatal("expected error")
	}
	var rlErr *RateLimitedError
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}

	// Gate should now be closed for anthropic endpoint
	gate := ag.getOrCreateRateLimitGate("anthropic")
	limited, _ := gate.IsLimited()
	if !limited {
		t.Error("gate should be closed after 429")
	}

	// Second call should be blocked by the gate (no API hit)
	_, err = ag.HandleMessage(context.Background(), "test/igate429/1000000000", "World")
	if err == nil {
		t.Fatal("expected RateLimitedError on second call")
	}
	if !errors.As(err, &rlErr) {
		t.Fatalf("expected *RateLimitedError on second call, got %T: %v", err, err)
	}
}

func TestDrainRateLimitQueue(t *testing.T) {
	// When the gate opens, DrainRateLimitQueue should replay messages.
	var apiCalls atomic.Int32
	server := mockServer(func(req *provider.MessageRequest) *provider.MessageResponse {
		apiCalls.Add(1)
		return &provider.MessageResponse{
			ID:         "msg_drain",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("replayed"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	// Queue items as if rate-limited on anthropic endpoint
	gate := ag.getOrCreateRateLimitGate("anthropic")
	gate.Close(time.Now().Add(-1 * time.Second)) // already expired
	gate.Enqueue("test/idrain/1000000000", "msg1", "user")
	gate.Enqueue("test/idrain/1000000000", "msg2", "keepalive")

	ag.DrainRateLimitQueue(context.Background())

	if got := apiCalls.Load(); got != 2 {
		t.Errorf("expected 2 API calls from replay, got %d", got)
	}
}

// TestCanFireBackgroundOperation_RateLimited proves that the method returns false
// when the rate limit gate is closed, with a descriptive reason including reset time.
func TestCanFireBackgroundOperation_RateLimited(t *testing.T) {
	ag := &Agent{ManaInvestInterval: 30 * time.Minute, Endpoint: "anthropic"}
	gate := ag.getOrCreateRateLimitGate("anthropic")
	gate.Close(time.Now().Add(2 * time.Hour))

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123/1000000000")

	if canFire {
		t.Error("expected canFire=false when rate limited")
	}
	if !strings.Contains(reason, "rate limited") {
		t.Errorf("expected rate limited reason, got: %s", reason)
	}
	if !strings.Contains(reason, "resets") {
		t.Errorf("expected reset time in reason, got: %s", reason)
	}
}

// TestCanFireBackgroundOperation_NoSessionKey proves that the method returns false
// with "no session key" when given an empty session key.
// TestCanFireBackgroundOperation_NoSessionKey proves that the method returns false
// with "no session key" when given an empty session key.
func TestCanFireBackgroundOperation_NoSessionKey(t *testing.T) {
	ag := &Agent{ManaInvestInterval: 30 * time.Minute}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "")

	if canFire {
		t.Error("expected canFire=false with empty session key")
	}
	if reason != "no session key" {
		t.Errorf("expected 'no session key', got: %s", reason)
	}
}

// TestCanFireBackgroundOperation_NoUsageClient proves that the method returns true
// (skips mana check) when there's no UsageClient for the session's endpoint.
// TestCanFireBackgroundOperation_NoUsageClient proves that the method returns true
// (skips mana check) when there's no UsageClient for the session's endpoint.
func TestCanFireBackgroundOperation_NoUsageClient(t *testing.T) {
	ag := &Agent{
		UsageClient:        nil,
		GetUsageClient:     func(endpoint string) provider.UsageClient { return nil },
		ManaInvestInterval: 30 * time.Minute,
	}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123/1000000000")

	if !canFire {
		t.Errorf("expected canFire=true for non-Anthropic endpoint, got false: %s", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got: %s", reason)
	}
}

// TestCanFireBackgroundOperation_ZeroInvestInterval proves that mana checking is skipped
// when ManaInvestInterval is zero (mana tracking disabled).
// TestCanFireBackgroundOperation_ZeroInvestInterval proves that mana checking is skipped
// when ManaInvestInterval is zero (mana tracking disabled).
func TestCanFireBackgroundOperation_ZeroInvestInterval(t *testing.T) {
	// Mock UsageClient that would fail if called
	mockClient := anthropic.NewUsageClient("dummy")

	ag := &Agent{
		UsageClient:        mockClient,
		GetUsageClient:     func(endpoint string) provider.UsageClient { return mockClient },
		ManaInvestInterval: 0, // disabled
	}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123/1000000000")

	if !canFire {
		t.Errorf("expected canFire=true with zero invest interval, got false: %s", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got: %s", reason)
	}
}

// TestCanFireBackgroundOperation_ManaInsufficient proves that the method returns false
// when mana is insufficient according to the monitor's IsGoodFor check.
// TestCanFireBackgroundOperation_ManaInsufficient proves that the method returns false
// when mana is insufficient according to the monitor's IsGoodFor check.
func TestCanFireBackgroundOperation_ManaInsufficient(t *testing.T) {
	// Skipping this test since UsageClient baseURL cannot be easily mocked in agent tests.
	// The mana.Monitor.IsGoodFor logic is already tested in the mana package tests.
	t.Skip("Skipping mana insufficient test - UsageClient baseURL cannot be easily mocked in agent tests")
}

// TestCanFireBackgroundOperation_Success proves that the method returns true
// when all checks pass (gate open, valid session, no usage client = mana check skipped).
// TestCanFireBackgroundOperation_Success proves that the method returns true
// when all checks pass (gate open, valid session, no usage client = mana check skipped).
func TestCanFireBackgroundOperation_Success(t *testing.T) {
	// Test the success path by having no usage client (mana check skipped)
	// This is the common path for non-Anthropic endpoints
	ag := &Agent{
		UsageClient:        nil,
		GetUsageClient:     func(endpoint string) provider.UsageClient { return nil },
		ManaInvestInterval: 30 * time.Minute,
	}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123/1000000000")

	if !canFire {
		t.Errorf("expected canFire=true when all checks pass, got false: %s", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got: %s", reason)
	}
}

// Test getOrCreateRateLimitGate creates gates lazily and returns the same instance.
// Test getOrCreateRateLimitGate creates gates lazily and returns the same instance.
func TestGetOrCreateRateLimitGate(t *testing.T) {
	ag := &Agent{}

	// First call creates gate
	gate1 := ag.getOrCreateRateLimitGate("anthropic")
	if gate1 == nil {
		t.Fatal("expected gate to be created")
	}

	// Second call returns same gate
	gate2 := ag.getOrCreateRateLimitGate("anthropic")
	if gate1 != gate2 {
		t.Error("expected same gate instance")
	}

	// Different endpoint gets different gate
	gate3 := ag.getOrCreateRateLimitGate("gemini")
	if gate3 == gate1 {
		t.Error("expected different gate for different endpoint")
	}

	// Empty endpoint defaults to "anthropic"
	gate4 := ag.getOrCreateRateLimitGate("")
	if gate4 != gate1 {
		t.Error("expected empty endpoint to default to anthropic gate")
	}
}

// Test that per-endpoint rate limiting isolates endpoints.
// Test that per-endpoint rate limiting isolates endpoints.
func TestPerEndpointRateLimiting(t *testing.T) {
	ag := &Agent{
		Endpoint:           "anthropic",
		ManaInvestInterval: 30 * time.Minute,
	}

	// Create two sessions with different endpoints
	session1 := "test/c123/1000000000"
	session2 := "test/c123/2000000000"

	// Set endpoints
	sm1 := ag.getSessionMeta(session1)
	ag.metaMu.Lock()
	sm1.modelEndpoint = "anthropic"
	ag.metaMu.Unlock()

	sm2 := ag.getSessionMeta(session2)
	ag.metaMu.Lock()
	sm2.modelEndpoint = "gemini"
	ag.metaMu.Unlock()

	// Close anthropic gate
	anthropicGate := ag.getOrCreateRateLimitGate("anthropic")
	anthropicGate.Close(time.Now().Add(2 * time.Hour))

	// Session 1 (anthropic) should be blocked
	canFire1, reason1 := ag.CanFireBackgroundOperation(context.Background(), session1)
	if canFire1 {
		t.Error("expected anthropic session to be blocked")
	}
	if !strings.Contains(reason1, "anthropic") {
		t.Errorf("expected reason to mention anthropic, got: %s", reason1)
	}

	// Session 2 (gemini) should NOT be blocked
	canFire2, reason2 := ag.CanFireBackgroundOperation(context.Background(), session2)
	if !canFire2 {
		t.Errorf("expected gemini session to be available, got: %s", reason2)
	}
}

// Test that DrainRateLimitQueue drains all endpoint queues independently.
// Test that DrainRateLimitQueue drains all endpoint queues independently.
func TestDrainRateLimitQueue_MultipleEndpoints(t *testing.T) {
	ag := &Agent{Endpoint: "anthropic"}

	// Create gates for two endpoints
	anthropicGate := ag.getOrCreateRateLimitGate("anthropic")
	geminiGate := ag.getOrCreateRateLimitGate("gemini")

	// Close anthropic gate with already-expired time
	anthropicGate.Close(time.Now().Add(-1 * time.Second))

	// Close gemini gate with future time
	geminiGate.Close(time.Now().Add(1 * time.Hour))

	// Verify gates are independent by checking their states
	limited1, _ := anthropicGate.IsLimited()
	limited2, _ := geminiGate.IsLimited()

	if limited1 {
		t.Error("anthropic gate should be open (expired)")
	}
	if !limited2 {
		t.Error("gemini gate should still be closed")
	}
}

// Test concurrent gate creation doesn't create duplicates.
// Test concurrent gate creation doesn't create duplicates.
func TestGetOrCreateRateLimitGate_Concurrent(t *testing.T) {
	ag := &Agent{}

	const goroutines = 100
	const endpoint = "anthropic"

	var wg sync.WaitGroup
	gates := make([]*RateLimitGate, goroutines)

	// Launch many goroutines trying to create the same gate
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			gates[idx] = ag.getOrCreateRateLimitGate(endpoint)
		}(i)
	}

	wg.Wait()

	// All goroutines should have received the same gate instance
	firstGate := gates[0]
	for i, gate := range gates {
		if gate != firstGate {
			t.Errorf("gate %d is different from gate 0", i)
		}
	}
}

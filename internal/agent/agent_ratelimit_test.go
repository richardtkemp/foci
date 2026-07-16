package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestHandleMessageRateLimitGateAllowsSuccessfulUserProbe(t *testing.T) {
	// Proves a closed API gate allows a human retry through and a successful
	// response releases the gate immediately.
	RegisterPlatformTrigger("telegram-rate-probe-e2e")
	t.Cleanup(func() { platformTriggers.Delete("telegram-rate-probe-e2e") })
	var calls atomic.Int32
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		calls.Add(1)
		return &provider.MessageResponse{
			Role:       "assistant",
			Content:    provider.TextContent("recovered"),
			StopReason: "end_turn",
		}
	})
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		Endpoint:  "anthropic",
	}

	// Close the gate for the agent's endpoint
	until := time.Now().Add(1 * time.Hour)
	gate := ag.getOrCreateRateLimitGate("anthropic")
	gate.Close(until)

	ctx := WithTrigger(context.Background(), "telegram-rate-probe-e2e")
	if _, err := ag.hmTest(ctx, "test/igate", "Hello"); err != nil {
		t.Fatalf("user probe: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("API calls = %d, want 1", calls.Load())
	}
	if limited, _ := gate.IsLimited(); limited {
		t.Error("successful user probe left gate closed")
	}
}

func TestHandleMessageRateLimitClosesGate(t *testing.T) {
	// A 429 from the API should close the gate so subsequent calls are blocked.
	client := newTestClientWithError(func(_ context.Context, _ *provider.MessageRequest) (*provider.MessageResponse, error) {
		return nil, &provider.APIError{StatusCode: 429, RetryAfter: "300", Body: "rate limited"}
	})
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     registry,
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
		Endpoint:  "anthropic",
	}

	// First system call hits the API, gets 429, and closes the gate.
	ctx := WithTrigger(context.Background(), "keepalive")
	_, err := ag.hmTest(ctx, "test/igate429", "Hello")
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
	_, err = ag.hmTest(ctx, "test/igate429", "World")
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
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
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
	gate.Enqueue("test/idrain", "msg1", "user")
	gate.Enqueue("test/idrain", "msg2", "keepalive")

	ag.DrainRateLimitQueue(context.Background())

	if got := apiCalls.Load(); got != 2 {
		t.Errorf("expected 2 API calls from replay, got %d", got)
	}
}

func TestCanFireBackgroundOperation_RateLimited(t *testing.T) {
	// Proves that a closed rate-limit gate makes CanFireBackgroundOperation return false
	// with a reason that includes "rate limited" and the gate's reset time.
	ag := &Agent{Endpoint: "anthropic"}
	gate := ag.getOrCreateRateLimitGate("anthropic")
	gate.Close(time.Now().Add(2 * time.Hour))

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123")

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

func TestCanFireBackgroundOperation_NoSessionKey(t *testing.T) {
	// Proves that an empty session key is rejected immediately with "no session key",
	// without consulting the rate-limit gate.
	ag := &Agent{}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "")

	if canFire {
		t.Error("expected canFire=false with empty session key")
	}
	if reason != "no session key" {
		t.Errorf("expected 'no session key', got: %s", reason)
	}
}

func TestCanFireBackgroundOperation_Success(t *testing.T) {
	// Proves the full success path: gate open, valid session key, and no
	// can_run_background gate configured all combine to return canFire=true.
	ag := &Agent{}

	canFire, reason := ag.CanFireBackgroundOperation(context.Background(), "test/c123")

	if !canFire {
		t.Errorf("expected canFire=true when all checks pass, got false: %s", reason)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got: %s", reason)
	}
}

func TestCanFireBackgroundOperation_CanRunBackgroundDeclined(t *testing.T) {
	// Proves that a can_run_background executable exiting non-zero declines the
	// operation, while one exiting zero permits it.
	declined := &Agent{CanRunBackground: "/bin/false"}
	if canFire, reason := declined.CanFireBackgroundOperation(context.Background(), "test/c123"); canFire {
		t.Errorf("expected canFire=false when can_run_background exits non-zero, got reason: %s", reason)
	} else if !strings.Contains(reason, "can_run_background") {
		t.Errorf("expected can_run_background reason, got: %s", reason)
	}

	allowed := &Agent{CanRunBackground: "/bin/true"}
	if canFire, reason := allowed.CanFireBackgroundOperation(context.Background(), "test/c123"); !canFire {
		t.Errorf("expected canFire=true when can_run_background exits zero, got reason: %s", reason)
	}
}

func TestGetOrCreateRateLimitGate(t *testing.T) {
	// Proves that getOrCreateRateLimitGate creates gates lazily, returns the same instance
	// for the same endpoint, uses different instances for different endpoints, and maps
	// an empty endpoint string to the agent's configured default endpoint.
	ag := &Agent{Endpoint: "anthropic"}

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

	// Empty endpoint defaults to agent's configured endpoint
	gate4 := ag.getOrCreateRateLimitGate("")
	if gate4 != gate1 {
		t.Error("expected empty endpoint to default to agent endpoint gate")
	}
}

func TestPerEndpointRateLimiting(t *testing.T) {
	// Proves that rate-limit gates are isolated per endpoint: closing the anthropic gate
	// blocks only anthropic sessions while gemini sessions remain unaffected.
	ag := &Agent{
		Endpoint: "anthropic",
	}

	// Create two sessions with different endpoints
	session1 := "test/c123"
	session2 := "test/c456"

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

func TestDrainRateLimitQueue_MultipleEndpoints(t *testing.T) {
	// Proves that rate-limit gate expiry is independent per endpoint: an anthropic gate
	// with an already-expired time opens while a gemini gate with a future time stays closed.
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

func TestGetOrCreateRateLimitGate_Concurrent(t *testing.T) {
	// Proves that concurrent calls to getOrCreateRateLimitGate for the same endpoint
	// all receive the exact same gate pointer, with no races or duplicate creation.
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

package opencode

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/ratelimit"
)

// TestParseRateLimitRetry proves an explicit reset offset is honoured while
// unrelated OpenCode retries are ignored.
func TestParseRateLimitRetry(t *testing.T) {
	got, ok := parseRateLimitRetry("Usage limit reached for 5 hour. Your limit will reset at 2026-07-16 19:13:59+08:00")
	if !ok {
		t.Fatal("usage-limit retry was not recognised")
	}
	want := time.Date(2026, 7, 16, 19, 13, 59, 0, time.FixedZone("reset", 8*60*60))
	if got.Kind != ratelimit.KindUsage || !got.ResetAt.Equal(want) {
		t.Errorf("signal = %+v, want usage reset %v", got, want)
	}
	if _, ok := parseRateLimitRetry("temporary upstream unavailable"); ok {
		t.Error("transient retry was misclassified as a rate limit")
	}
}

// TestParseRateLimitRetryFallback proves the observed timezone-less Z.AI reset
// is not misrepresented as local time and instead uses the fallback window.
func TestParseRateLimitRetryFallback(t *testing.T) {
	got, ok := parseRateLimitRetry("Usage limit reached. Your limit will reset at 2026-07-16 19:13:59")
	if !ok {
		t.Fatal("rate-limit retry was not recognised")
	}
	if got.Kind != ratelimit.KindUsage || !got.ResetAt.IsZero() {
		t.Errorf("ambiguous signal = %+v, want usage with no reset hint", got)
	}
}

// TestOnSessionStatus_RateLimitAbortsAndCompletes proves the first usage-limit
// retry engages the callback, POSTs /abort, and releases the waiting turn.
func TestOnSessionStatus_RateLimitAbortsAndCompletes(t *testing.T) {
	b, rec := newControlTestBackend(t)
	var callbackCount atomic.Int32
	var signal ratelimit.Signal
	b.SetOnRateLimited(func(got ratelimit.Signal) {
		callbackCount.Add(1)
		signal = got
	})
	var completed *delegator.TurnResult
	b.beginTurn(&delegator.TurnEvents{OnTurnComplete: func(result *delegator.TurnResult) {
		completed = result
	}})
	wantReset := time.Now().Add(2 * time.Hour).UTC()
	b.onSessionStatus(b.sessionID, SessionStatus{
		Type:    StatusRetry,
		Attempt: 1,
		Message: "Usage limit reached. Your limit will reset at " + wantReset.Format("2006-01-02 15:04:05Z07:00"),
	})

	if got := callbackCount.Load(); got != 1 {
		t.Fatalf("rate-limit callback count = %d, want 1", got)
	}
	if signal.ResetAt.Sub(wantReset) < -time.Second || signal.ResetAt.Sub(wantReset) > time.Second {
		t.Errorf("callback reset = %v, want approximately %v", signal.ResetAt, wantReset)
	}
	if _, ok := rec.lastAbort(); !ok {
		t.Fatal("rate-limit retry did not POST /abort")
	}
	if completed == nil {
		t.Fatal("waiting turn was not completed")
	}
	if completed.Text != "" {
		t.Errorf("completion text = %q, want empty (notification is delivered by the rate-limit hook)", completed.Text)
	}
	if b.IsTurnInFlight() {
		t.Error("turn remains in flight after rate-limit cancellation")
	}

	// Delayed duplicate retry events from the aborted turn must not notify or
	// abort again after the turn has already been released.
	b.onSessionStatus(b.sessionID, SessionStatus{Type: StatusRetry, Attempt: 2, Message: "rate limited"})
	if got := callbackCount.Load(); got != 1 {
		t.Errorf("duplicate retry fired callback %d times, want 1", got)
	}
}

// TestOnSessionStatus_TransientRetryKeepsWaiting proves ordinary OpenCode
// retries retain their existing behavior and do not abort a recoverable turn.
func TestOnSessionStatus_TransientRetryKeepsWaiting(t *testing.T) {
	b, rec := newControlTestBackend(t)
	var callbackFired atomic.Bool
	b.SetOnRateLimited(func(ratelimit.Signal) { callbackFired.Store(true) })
	b.beginTurn(&delegator.TurnEvents{})

	b.onSessionStatus(b.sessionID, SessionStatus{Type: StatusRetry, Attempt: 1, Message: "temporary upstream unavailable"})

	if callbackFired.Load() {
		t.Error("transient retry fired rate-limit callback")
	}
	if _, ok := rec.lastAbort(); ok {
		t.Error("transient retry POSTed /abort")
	}
	if !b.IsTurnInFlight() {
		t.Error("transient retry unexpectedly completed the turn")
	}
}

// TestOnSessionStatus_RateLimitAbortFailureStillCompletes proves a broken
// local abort endpoint cannot leave Foci's waiting turn permanently pending.
func TestOnSessionStatus_RateLimitAbortFailureStillCompletes(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "abort failed", http.StatusInternalServerError)
	}))
	t.Cleanup(hs.Close)
	b := &Backend{
		server:      &Server{baseURL: hs.URL, http: hs.Client(), agentID: "rate-limit-test"},
		agentID:     "rate-limit-test",
		sessionID:   "sess-rate-limit",
		readyCh:     make(chan struct{}),
		outstanding: delegator.NewOutstandingRegistry(),
	}
	var completed atomic.Bool
	b.beginTurn(&delegator.TurnEvents{OnTurnComplete: func(*delegator.TurnResult) {
		completed.Store(true)
	}})

	b.onSessionStatus(b.sessionID, SessionStatus{Type: StatusRetry, Message: "rate limited"})

	if !completed.Load() {
		t.Error("abort HTTP failure left the turn waiting")
	}
	if b.IsTurnInFlight() {
		t.Error("abort HTTP failure left backend turn active")
	}
}

package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

// newControlTestBackend returns a Backend wired to an httptest server
// that records PATCH /config and POST /abort requests.
func newControlTestBackend(t *testing.T) (*Backend, *controlRecorder) {
	t.Helper()
	rec := &controlRecorder{}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, controlRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   body,
		})
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hs.Close)
	b := &Backend{
		server:      &Server{baseURL: hs.URL, http: hs.Client(), agentID: "ctrl-test"},
		agentID:     "ctrl-test",
		sessionID:   "sess-ctrl",
		readyCh:     make(chan struct{}),
		outstanding: NewOutstandingRegistry(),
	}
	return b, rec
}

type controlRequest struct {
	Method string
	Path   string
	Body   []byte
}

type controlRecorder struct {
	mu       sync.Mutex
	requests []controlRequest
}

func (r *controlRecorder) lastConfigPatch() (controlRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if r.requests[i].Method == http.MethodPatch && r.requests[i].Path == "/config" {
			return r.requests[i], true
		}
	}
	return controlRequest{}, false
}

func (r *controlRecorder) lastAbort() (controlRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.requests) - 1; i >= 0; i-- {
		if r.requests[i].Path == "/session/sess-ctrl/abort" {
			return r.requests[i], true
		}
	}
	return controlRequest{}, false
}

// ---------------------------------------------------------------------------
// SendControl — SetModel
// ---------------------------------------------------------------------------

func TestSendControl_SetModel(t *testing.T) {
	// Verifies SetModelRequest translates to PATCH /config with the
	// model in the body.
	b, rec := newControlTestBackend(t)

	if err := b.SendControl(context.Background(), &delegator.SetModelRequest{
		Model: "anthropic/claude-sonnet-4",
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	patch, ok := rec.lastConfigPatch()
	if !ok {
		t.Fatal("PATCH /config was not sent")
	}
	var body map[string]any
	if err := json.Unmarshal(patch.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["model"] != "anthropic/claude-sonnet-4" {
		t.Errorf("model = %v, want anthropic/claude-sonnet-4", body["model"])
	}
}

// ---------------------------------------------------------------------------
// SendControl — SetPermissionMode
// ---------------------------------------------------------------------------

func TestSendControl_SetPermissionMode(t *testing.T) {
	// Verifies SetPermissionModeRequest translates to PATCH /config with
	// a permission map derived from the mode.
	b, rec := newControlTestBackend(t)

	if err := b.SendControl(context.Background(), &delegator.SetPermissionModeRequest{
		Mode: "acceptEdits",
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	patch, ok := rec.lastConfigPatch()
	if !ok {
		t.Fatal("PATCH /config was not sent")
	}
	var body struct {
		Permission map[string]string `json:"permission"`
	}
	if err := json.Unmarshal(patch.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Permission["edit"] != "allow" {
		t.Errorf("permission.edit = %q, want allow", body.Permission["edit"])
	}
	if body.Permission["*"] != "ask" {
		t.Errorf("permission.* = %q, want ask", body.Permission["*"])
	}
}

// ---------------------------------------------------------------------------
// SendControl — ApplyFlagSettings (effort) is a no-op
// ---------------------------------------------------------------------------

func TestSendControl_SetEffort_NoOpReturns(t *testing.T) {
	// Verifies ApplyFlagSettingsRequest (used for /effort) is a no-op
	// for opencode — returns nil without sending any HTTP request.
	b, rec := newControlTestBackend(t)

	if err := b.SendControl(context.Background(), &delegator.ApplyFlagSettingsRequest{
		Settings: map[string]any{"effortLevel": "high"},
	}); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	if len(rec.requests) != 0 {
		t.Errorf("effort no-op should not send any requests; got %d", len(rec.requests))
	}
}

// ---------------------------------------------------------------------------
// Interrupt — POST /session/:id/abort
// ---------------------------------------------------------------------------

func TestInterrupt_PostsAbort(t *testing.T) {
	// Verifies Interrupt POSTs to /session/:id/abort.
	b, rec := newControlTestBackend(t)

	if err := b.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	abort, ok := rec.lastAbort()
	if !ok {
		t.Fatal("POST /session/:id/abort was not sent")
	}
	if abort.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", abort.Method)
	}
}

func TestInterrupt_BeforeStart(t *testing.T) {
	// Verifies Interrupt on a never-Started Backend returns an error
	// rather than panicking.
	b := &Backend{}
	err := b.Interrupt(context.Background())
	if err == nil {
		t.Error("Interrupt before Start should error")
	}
}

// ---------------------------------------------------------------------------
// CompactionWait — already tested in Step 6's TestArmCompactionWait_ContextCancellation.
// Add the "fires on session.compacted" test here.
// ---------------------------------------------------------------------------

func TestCompactionWait_FiresOnSessionCompacted(t *testing.T) {
	// Verifies WaitForCompaction unblocks when onSessionCompacted fires.
	b := &Backend{
		sessionID:     "sess-test",
		compactDoneCh: make(chan struct{}, 1),
		outstanding:   NewOutstandingRegistry(),
	}
	b.mu.Lock()
	b.onCompactionDone = func(int) {}
	b.mu.Unlock()

	// Arm + fire.
	b.ArmCompactionWait()
	b.onSessionCompacted("sess-test")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.WaitForCompaction(ctx); err != nil {
		t.Errorf("WaitForCompaction: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CompactionStartWait — synthesised immediate fire
// ---------------------------------------------------------------------------

func TestCompactionStartWait_FiresImmediatelyAfterArm(t *testing.T) {
	// Verifies ArmCompactionStartWait + WaitForCompactionStart returns
	// immediately — opencode has no "compacting started" event, so we
	// synthesise it by closing the channel on arm.
	b := &Backend{
		sessionID:     "sess-test",
		compactDoneCh: make(chan struct{}, 1),
		outstanding:   NewOutstandingRegistry(),
	}

	b.ArmCompactionStartWait()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.WaitForCompactionStart(ctx); err != nil {
		t.Errorf("WaitForCompactionStart: %v", err)
	}
}

func TestCompactionStartWait_NotArmedReturnsNil(t *testing.T) {
	// Verifies WaitForCompactionStart without arming returns nil
	// (matching WaitForCompaction's no-arm contract).
	b := &Backend{
		sessionID:     "sess-test",
		compactDoneCh: make(chan struct{}, 1),
		outstanding:   NewOutstandingRegistry(),
	}

	if err := b.WaitForCompactionStart(context.Background()); err != nil {
		t.Errorf("WaitForCompactionStart (unarmed): %v", err)
	}
}

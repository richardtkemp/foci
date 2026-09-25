package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/telemetry"
)

// batchTurnBackend is a delegated backend that serves turns the way the real
// ones do (ImmediateInject → OnTurnComplete with usage). It ALSO carries a
// RunBatch method shaped like the old one-shot path (text back, nothing
// logged), so a RunBatch that still took that separate path returns text
// without writing a row — the #1962 defect — rather than erroring.
type batchTurnBackend struct {
	mockBackendDT

	optsMu    sync.Mutex
	started   []delegator.StartOptions
	closed    int
	oneShotMu sync.Mutex
	oneShot   bool
}

// completeTurns makes every turn finish at once with a short answer — the
// default mock's SendToPane never fires OnTurnComplete, which would leave the
// batch turn waiting forever.
func (b *batchTurnBackend) completeTurns() {
	b.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		if h != nil && h.OnTurnComplete != nil {
			h.OnTurnComplete(&delegator.TurnResult{Text: "ok", Model: "claude-sonnet-4-5"})
		}
		return nil, nil
	}
}

func (b *batchTurnBackend) Start(_ context.Context, opts delegator.StartOptions) error {
	b.optsMu.Lock()
	b.started = append(b.started, opts)
	b.optsMu.Unlock()
	return nil
}

func (b *batchTurnBackend) Close() error {
	b.optsMu.Lock()
	b.closed++
	b.optsMu.Unlock()
	return nil
}

// RunBatch is the legacy one-shot shape. The turn path must never call it.
func (b *batchTurnBackend) RunBatch(_ context.Context, _ delegator.BatchRequest) (string, error) {
	b.oneShotMu.Lock()
	b.oneShot = true
	b.oneShotMu.Unlock()
	return "consolidated", nil
}

// fakeOTLP records every OTLP/HTTP export body. Span attributes are carried
// as raw strings inside the protobuf, so a byte search is enough to prove a
// span with a given session/tag was exported.
type fakeOTLP struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (f *fakeOTLP) all() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Join(f.bodies, nil)
}

func startFakeOTLP(t *testing.T) *fakeOTLP {
	t.Helper()
	f := &fakeOTLP{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, b)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	if err := telemetry.Init(context.Background(), telemetry.Options{
		Endpoint: srv.URL, PublicKey: "pk", SecretKey: "sk", Environment: "test",
	}); err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	t.Cleanup(func() { telemetry.Shutdown(context.Background()) })
	return f
}

// newBatchTestAgent wires an Agent and its DelegatedManager the way
// cmd/foci-gw/agents_delegated.go does in production.
func newBatchTestAgent(t *testing.T, be delegator.Delegator) *Agent {
	t.Helper()
	mgr := &DelegatedManager{
		AgentID:    "helen",
		StartOpts:  delegator.StartOptions{AgentID: "helen", WorkDir: t.TempDir(), Model: "opus"},
		NewBackend: func() (delegator.Delegator, error) { return be, nil },
	}
	a := &Agent{AgentID: "helen", Model: "opus", DelegatedManager: mgr}
	mgr.AttachDelivery = a.AttachDelivery
	mgr.RunBatchTurn = a.RunBatchTurn
	return a
}

// TestRunBatch_RecordsAPIRowAndTrace is the #1962 regression: a batch run
// (consolidation, nudge extraction, the summary tool) must go through the
// ordinary delegated turn, so the existing turn code writes the api.db row and
// the Langfuse trace. Before, RunBatch shelled a separate one-shot and left no
// record of its spend at all.
func TestRunBatch_RecordsAPIRowAndTrace(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "api.db")
	if err := log.InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	t.Cleanup(log.CloseAPIDB)
	otlp := startFakeOTLP(t)

	cost := 0.0123
	be := &batchTurnBackend{}
	be.sessionFile = "/tmp/batch.jsonl"
	var sentPrompt string
	be.sendToPaneFn = func(_ context.Context, prompt string, h *mockHandler) (*delegator.TurnResult, error) {
		sentPrompt = prompt
		if h != nil && h.OnText != nil {
			h.OnText("consolidated")
		}
		if h != nil && h.OnTurnComplete != nil {
			h.OnTurnComplete(&delegator.TurnResult{
				Text:  "consolidated",
				Model: "claude-sonnet-4-5",
				Usage: &delegator.TurnUsage{InputTokens: 12, OutputTokens: 340, CacheReadInputTokens: 5000, CalculatedCostUSD: &cost},
			})
		}
		return nil, nil
	}
	a := newBatchTestAgent(t, be)

	got, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt:          "consolidate your memory",
		SystemPrompt:    "CHARACTER",
		OwnerSessionKey: "helen/c42",
		Purpose:         delegator.BatchPurposeConsolidation,
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got != "consolidated" {
		t.Errorf("RunBatch returned %q, want the turn's final text", got)
	}
	be.oneShotMu.Lock()
	usedOneShot := be.oneShot
	be.oneShotMu.Unlock()
	if usedOneShot {
		t.Error("RunBatch used the separate one-shot path instead of a delegated turn")
	}

	rows := log.ReadAPIDBLog()
	if len(rows) != 1 {
		t.Fatalf("api.db rows after a batch run = %d, want 1 (a batch run must be accounted like any turn)", len(rows))
	}
	r := rows[0]
	if r.CallType != "delegated_turn" {
		t.Errorf("call_type = %q, want delegated_turn", r.CallType)
	}
	if !strings.HasPrefix(r.Session, "helen/c42/b") {
		t.Errorf("row session = %q, want a b-child of the owner helen/c42", r.Session)
	}
	if r.AgentID != "helen" {
		t.Errorf("row agent_id = %q, want helen", r.AgentID)
	}
	if r.Output != 340 || r.CacheRead != 5000 {
		t.Errorf("row tokens = {out:%d cr:%d}, want {340 5000}", r.Output, r.CacheRead)
	}
	if r.CalculatedCostUSD == nil || *r.CalculatedCostUSD != cost {
		t.Errorf("row calculated cost = %v, want %v", r.CalculatedCostUSD, cost)
	}
	if r.Purpose != delegator.BatchPurposeConsolidation {
		t.Errorf("row purpose = %q, want %q", r.Purpose, delegator.BatchPurposeConsolidation)
	}

	// The batch-specific launch: the caller's system prompt replaces the
	// agent's, no permission prompts, and the prompt goes out verbatim.
	be.optsMu.Lock()
	started, closed := be.started, be.closed
	be.optsMu.Unlock()
	if len(started) != 1 {
		t.Fatalf("backend started %d times, want 1", len(started))
	}
	if started[0].SystemPrompt != "CHARACTER" || started[0].SystemPromptFunc != nil {
		t.Errorf("batch launched with system prompt %q (func set=%v), want the caller's CHARACTER only",
			started[0].SystemPrompt, started[0].SystemPromptFunc != nil)
	}
	if !started[0].SkipPermissions {
		t.Error("batch session launched with permission prompts enabled — a prompt would land in a chat")
	}
	if started[0].SessionKey != r.Session {
		t.Errorf("backend started for %q, want the batch session %q", started[0].SessionKey, r.Session)
	}
	if sentPrompt != "consolidate your memory" {
		t.Errorf("prompt sent = %q, want the caller's prompt verbatim (no [meta] header)", sentPrompt)
	}
	// No session left behind: the backend is closed once the batch returns.
	if closed != 1 {
		t.Errorf("backend closed %d times, want 1", closed)
	}
	if n := a.DelegatedManager.Count(); n != 0 {
		t.Errorf("%d backend(s) still managed after the batch", n)
	}

	// Flush the batcher, then look for the batch's turn trace.
	telemetry.Shutdown(context.Background())
	exported := otlp.all()
	if !bytes.Contains(exported, []byte(r.Session)) {
		t.Errorf("no exported span carries the batch session %q — the batch turn was not traced", r.Session)
	}
	if !bytes.Contains(exported, []byte(r.TurnID)) {
		t.Errorf("no exported span carries turn_id %q — trace and api.db row are not linked", r.TurnID)
	}
	if !bytes.Contains(exported, []byte("purpose:consolidation")) {
		t.Error("the batch trace is not tagged purpose:consolidation")
	}
}

// defaultingBackend is a batch-capable backend with a cheap batch model.
type defaultingBackend struct{ batchTurnBackend }

func (*defaultingBackend) BatchDefaultModel() string { return "sonnet" }

// TestRunBatch_ModelSelection: an explicit model wins; otherwise the backend's
// batch default (CC: sonnet); otherwise the agent's own model.
func TestRunBatch_ModelSelection(t *testing.T) {
	type batchBackend interface {
		delegator.Delegator
		startedOpts() []delegator.StartOptions
		completeTurns()
	}
	cases := []struct {
		name  string
		be    batchBackend
		model string
		want  string
	}{
		{"explicit override", &defaultingBackend{}, "haiku", "haiku"},
		{"backend batch default", &defaultingBackend{}, "", "sonnet"},
		{"no default: agent model", &batchTurnBackend{}, "", "opus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.be.completeTurns()
			a := newBatchTestAgent(t, c.be)
			if _, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{
				Prompt: "p", Model: c.model, OwnerSessionKey: "helen/c1", Purpose: delegator.BatchPurposeSummary,
			}); err != nil {
				t.Fatalf("RunBatch: %v", err)
			}
			opts := c.be.startedOpts()
			if len(opts) != 1 || opts[0].Model != c.want {
				t.Fatalf("started with %+v, want model %q", opts, c.want)
			}
		})
	}
}

func (b *batchTurnBackend) startedOpts() []delegator.StartOptions {
	b.optsMu.Lock()
	defer b.optsMu.Unlock()
	return append([]delegator.StartOptions(nil), b.started...)
}

// TestRunBatch_RequiresPurpose: an unlabelled batch would be an unattributable
// api.db row, so it is refused rather than run.
func TestRunBatch_RequiresPurpose(t *testing.T) {
	be := &batchTurnBackend{}
	be.completeTurns()
	a := newBatchTestAgent(t, be)
	if _, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{Prompt: "p"}); err == nil {
		t.Fatal("RunBatch ran a batch with no purpose")
	}
}

// TestRunBatch_NoOwnerGetsSyntheticRoot: with no owner session (nothing in ctx
// either) the batch is still a well-formed, non-root child key.
func TestRunBatch_NoOwnerGetsSyntheticRoot(t *testing.T) {
	be := &batchTurnBackend{}
	be.completeTurns()
	a := newBatchTestAgent(t, be)
	if _, err := a.DelegatedManager.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt: "p", Purpose: delegator.BatchPurposeConsolidation,
	}); err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	opts := be.startedOpts()
	if len(opts) != 1 || !strings.HasPrefix(opts[0].SessionKey, "helen/ibatch/b") {
		t.Fatalf("batch session = %+v, want helen/ibatch/b<ns>", opts)
	}
}

// closeEndsTurnBackend never finishes a turn on its own; Close ends it the way
// a real backend's process-exit finalize does (OnTurnComplete, no result).
type closeEndsTurnBackend struct {
	batchTurnBackend
	mu      sync.Mutex
	pending func(*delegator.TurnResult)
}

func (b *closeEndsTurnBackend) Close() error {
	b.mu.Lock()
	fn := b.pending
	b.pending = nil
	b.mu.Unlock()
	if fn != nil {
		fn(nil)
	}
	return b.batchTurnBackend.Close()
}

// TestRunBatch_CancelClosesTheBatch: a turn waits for the backend, not for
// ctx, so a caller that gives up must not be held for the rest of the turn —
// RunBatch closes the batch backend, which ends it.
func TestRunBatch_CancelClosesTheBatch(t *testing.T) {
	be := &closeEndsTurnBackend{}
	started := make(chan struct{})
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		be.mu.Lock()
		be.pending = h.OnTurnComplete
		be.mu.Unlock()
		close(started)
		return nil, nil
	}
	a := newBatchTestAgent(t, be)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := a.DelegatedManager.RunBatch(ctx, delegator.BatchRequest{
			Prompt: "p", OwnerSessionKey: "helen/c1", Purpose: delegator.BatchPurposeSummary,
		})
		errc <- err
	}()
	<-started
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("cancelled RunBatch returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunBatch still waiting on its turn 10s after the caller cancelled")
	}
}

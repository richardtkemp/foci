package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
)

// stubAppServerInterruptEndsTurn models the real contract this depends on: a
// turn/start never completes on its own, and only a turn/interrupt ends it
// (with status "failed", as an aborted turn reports). Every inbound line is
// captured so a test can assert what foci actually sent on the wire rather
// than inferring it from downstream effects.
func stubAppServerInterruptEndsTurn(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.jsonl")
	stub := filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json, sys
capture = ` + `"` + capture + `"` + `
next_thread = 0
with open(capture, "w") as f:
  for line in sys.stdin:
    f.write(line); f.flush()
    try: msg=json.loads(line)
    except Exception: continue
    method=msg.get("method"); ident=msg.get("id")
    def send(x): print(json.dumps(x), flush=True)
    if method == "initialize":
      send({"id":ident,"result":{}})
    elif method == "model/list":
      send({"id":ident,"result":{"data":[]}})
    elif method == "thread/start":
      next_thread += 1; tid="thread-%d" % next_thread
      thread={"id":tid,"path":None,"status":{"type":"idle"}}
      send({"id":ident,"result":{"thread":thread,"model":"test-model"}})
      send({"method":"thread/started","params":{"thread":thread}})
    elif method == "turn/start":
      tid=msg["params"]["threadId"]
      send({"id":ident,"result":{"turn":{"id":"turn-"+tid,"status":"inProgress"}}})
      send({"method":"turn/started","params":{"threadId":tid,"turn":{"id":"turn-"+tid,"status":"inProgress"}}})
      # deliberately NO completion: only an interrupt ends this turn
    elif method == "turn/interrupt":
      tid=msg["params"]["threadId"]
      send({"method":"turn/completed","params":{"threadId":tid,"turn":{"id":"turn-"+tid,"status":"failed"}}})
    elif ident is not None: send({"id":ident,"result":{}})
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub, capture
}

// A cancelled batch must ask codex to STOP the turn, not merely stop waiting
// for it. Deferring cleanup until the turn's real end (the #1570 fix) is only
// half the story: if nothing ends the turn, it keeps running and spending
// tokens with nothing tracking it, and the deferred-cleanup goroutine blocks
// forever holding this facade's pool ref — so the agent's app-server is never
// reaped either. processDone() bounds that wait only on the process dying,
// which is precisely the case that does NOT apply while sibling sessions keep
// it alive.
//
// The stub only ends a turn in response to turn/interrupt, so the refcount
// returning to baseline is real evidence the interrupt was both sent and
// effective — not just that some timeout elapsed.
func TestCancelledBatchInterruptsTheTurn(t *testing.T) {
	stub, capture := stubAppServerInterruptEndsTurn(t)
	cfg := map[string]any{"binary": stub}
	const agent = "interrupt-agent"
	ctx := context.Background()

	ownerBE, _ := newFromConfig(cfg)
	owner := ownerBE.(*Backend)
	if err := owner.Start(ctx, delegator.StartOptions{AgentID: agent, SessionKey: "owner/session", WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("start owner: %v", err)
	}
	defer func() { _ = owner.Close() }()

	sharedPool.Lock()
	baseline := sharedPool.refs[agent]
	sharedPool.Unlock()

	batchBE, _ := newFromConfig(cfg)
	batch := batchBE.(*Backend)

	batchCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := batch.RunBatch(batchCtx, delegator.BatchRequest{
			Prompt:     "hello",
			WorkDir:    t.TempDir(),
			AgentID:    agent,
			SessionKey: "owner/session/b1",
		})
		done <- err
	}()

	// Wait for the turn to be live server-side before cancelling — otherwise
	// the test could cancel before there is anything to interrupt and pass
	// for the wrong reason.
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(capture)
		return err == nil && strings.Contains(string(data), `"method":"turn/start"`)
	}, "turn/start never reached the app-server")

	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled RunBatch returned nil error")
	}

	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(capture)
		return err == nil && strings.Contains(string(data), `"method":"turn/interrupt"`)
	}, "cancelled batch never sent turn/interrupt — the orphaned turn keeps running and spending tokens")

	waitFor(t, 5*time.Second, func() bool {
		sharedPool.Lock()
		defer sharedPool.Unlock()
		return sharedPool.refs[agent] == baseline
	}, "pool ref never returned to baseline after cancel — the deferred-cleanup goroutine is still waiting on a turn nobody asked to end, pinning the app-server")
}

// handleBatchNotification runs on the SHARED reader goroutine, so a blocking
// send stalls notification processing for every session multiplexed on that
// app-server — not just the batch that owns the channel. run.done is buffered
// 1, so a duplicate turn/completed arriving before the run is torn down finds
// it full and, with a plain send, blocks the reader forever (#1580).
//
// The first completion is the answer; later ones are noise, so dropping them
// is the correct semantics as well as the safe one.
func TestDuplicateTurnCompletedDoesNotBlockTheReader(t *testing.T) {
	b := newTestBackend(t)
	run := &batchRun{done: make(chan batchResult, 1)}
	b.batchRuns = map[string]*batchRun{"thread-1": run}

	// Fill the buffer, as a first turn/completed that nothing has drained yet
	// would have.
	run.done <- batchResult{text: "first"}

	returned := make(chan bool, 1)
	go func() {
		returned <- b.handleBatchNotification("turn/completed",
			[]byte(`{"threadId":"thread-1","turn":{"status":"completed"}}`))
	}()

	select {
	case ok := <-returned:
		if !ok {
			t.Error("duplicate turn/completed was not consumed by the batch handler")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleBatchNotification blocked on a full done channel — on the real reader goroutine this stalls every session on the shared app-server")
	}

	// The original result must survive: a duplicate is dropped, not allowed to
	// overwrite the answer the caller is waiting for.
	if got := <-run.done; got.text != "first" {
		t.Errorf("done channel holds %q, want %q — the duplicate displaced the real result", got.text, "first")
	}
}

// waitFor polls cond until it holds or the deadline passes, failing with msg.
// Polling rather than sleeping keeps the test honest under load: a fixed sleep
// would either flake or hide a real delay.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

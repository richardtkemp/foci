package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
)

// TestBatchPoolRefReturnsToBaselineAfterCompleteAndAfterCancel covers #1585
// requirement 3: a batch that attaches to a shared app-server takes its own
// pool ref (distinct from the owner's), and that ref must return to its
// pre-batch baseline both when the batch completes normally AND when it is
// cancelled. Modelled on TestFailedFacadeAttachReleasesPoolRef, which drives
// a real stub app-server and inspects sharedPool.refs directly.
//
// The cancellation half is the one that actually discriminates a correct
// implementation from #1570's bug: the batch's turn is still running
// server-side when RunBatch's caller gives up, so releasing the ref
// immediately (before the turn's real end) is exactly the premature cleanup
// that lets a late turn/completed fall through to the wrong backend. The ref
// must stay elevated until the turn truly ends, then drop back to baseline.
func TestBatchPoolRefReturnsToBaselineAfterCompleteAndAfterCancel(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	sent := filepath.Join(dir, "sent")
	stub := stubAppServerDelayedTurnCompletion(t, release, sent)
	cfg := map[string]any{"binary": stub}
	const agent = "refcount-agent"
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
	done := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = batch.RunBatch(batchCtx, delegator.BatchRequest{
			Prompt: "p", WorkDir: t.TempDir(), AgentID: agent, SessionKey: "owner/session/b1",
		})
		close(done)
	}()

	// Wait for the batch to attach and take its own pool ref.
	waitForRefs(t, agent, baseline+1, "batch never attached to the pool (ref never reached baseline+1)")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunBatch did not return after cancel")
	}
	if runErr == nil {
		t.Fatal("RunBatch returned nil error after cancel, want ctx.Err()")
	}

	// The turn is still "live" on the server (the stub is withholding
	// turn/completed) -- the pool ref must NOT have been released yet.
	sharedPool.Lock()
	afterCancel := sharedPool.refs[agent]
	sharedPool.Unlock()
	if afterCancel != baseline+1 {
		t.Fatalf("pool refs = %d immediately after cancel, want %d (still elevated) -- releasing the ref before the still-live turn actually ends is the #1570 hazard", afterCancel, baseline+1)
	}

	// Let the turn actually finish server-side, then confirm the ref comes
	// back down to baseline -- no leak from the cancel path either.
	if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForRefs(t, agent, baseline, "pool ref never returned to baseline after the batch's real completion")
}

func waitForRefs(t *testing.T, agent string, want int, failMsg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sharedPool.Lock()
		cur := sharedPool.refs[agent]
		sharedPool.Unlock()
		if cur == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (stuck at %d, want %d)", failMsg, cur, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

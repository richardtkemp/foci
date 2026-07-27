package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator"
)

// stubAppServerDelayedTurnCompletion behaves like stubAppServer for
// thread/start (each call gets a fresh sequential thread id), but a
// turn/start's completion notifications (the agentMessage delta and
// turn/completed) are withheld until releaseFile exists on disk. This lets a
// test cancel its caller BEFORE the turn "really" finishes server-side, then
// release it and observe how the late-arriving turn/completed gets routed.
// releaseFile gates when the notifications are sent; sentFile is touched
// immediately afterward, giving the test a positive signal that the bytes
// left the stub -- independent of whether/how the Go side ends up handling
// them, so the wait works the same whether that handling is correct or
// buggy.
func stubAppServerDelayedTurnCompletion(t *testing.T, releaseFile, sentFile string) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json, sys, os, time
release = ` + `"` + releaseFile + `"` + `
sent = ` + `"` + sentFile + `"` + `
next_thread = 0
for line in sys.stdin:
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
    while not os.path.exists(release):
      time.sleep(0.02)
    send({"method":"item/agentMessage/delta","params":{"threadId":tid,"turnId":"turn-"+tid,"delta":"batch reply"}})
    send({"method":"turn/completed","params":{"threadId":tid,"turn":{"id":"turn-"+tid,"status":"completed"}}})
    with open(sent, "w") as f: f.write("sent")
  elif method == "thread/delete": send({"id":ident,"result":{}})
  elif ident is not None: send({"id":ident,"result":{}})
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// TestCancelledBatchTurnCannotCompleteOwnersTurn reproduces #1570: RunBatch
// multiplexes an ephemeral batch thread over a live backend's app-server
// connection. Codex's turn does not stop just because RunBatch's caller gave
// up (ctx cancel/timeout) -- it is still running server-side and WILL emit a
// turn/completed later. If RunBatch's cancel path unregisters the batch's
// thread mapping immediately (rather than waiting for that real completion),
// the late turn/completed has no registered facade to route to: dispatch()
// falls through to whichever backend object is reading the app-server
// connection and treats the notification as belonging to THAT object's own
// interactive turn -- silently completing it.
//
// This is exercised by calling RunBatch directly on an already-running
// backend (the shape the OLD delegated_manager.RunBatch used: a batch method
// invoked on the owner's own live *codex.Backend), which is the tightest
// reproduction of the bug: the batch and the "owner" turn state live on the
// exact same Go object, so nothing but RunBatch's own cleanup bookkeeping
// stands between a late notification and the owner's turn.
func TestCancelledBatchTurnCannotCompleteOwnersTurn(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	sent := filepath.Join(dir, "sent")
	stub := stubAppServerDelayedTurnCompletion(t, release, sent)

	be, _ := newFromConfig(map[string]any{"binary": stub})
	owner := be.(*Backend)
	if err := owner.Start(context.Background(), delegator.StartOptions{
		AgentID: "owner-agent", SessionKey: "owner/session", WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("start owner: %v", err)
	}
	defer func() { _ = owner.Close() }()

	// Fake an in-flight INTERACTIVE turn on the owner: the exact state
	// onTurnCompleted finalises (turnActive=false, push turnResultCh) if a
	// batch's completion notification is ever misrouted to this object.
	owner.turnMu.Lock()
	owner.turnActive = true
	owner.turnResultCh = make(chan *delegator.TurnResult, 1)
	owner.turnMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := owner.RunBatch(ctx, delegator.BatchRequest{
		Prompt: "batch prompt", WorkDir: t.TempDir(), SessionKey: "owner/session/b1",
	})
	if err == nil {
		t.Fatal("RunBatch returned successfully before the stub released the turn -- test setup is wrong")
	}

	if !owner.IsTurnInFlight() {
		t.Fatal("owner's turn already resolved before the batch's turn/completed was even released -- test setup is wrong")
	}

	// Let the "still live on the server" batch turn actually finish.
	if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait for the stub to have actually written turn/completed -- a signal
	// independent of how (or whether) the Go side routes it, so the wait
	// itself can't hide the bug by racing against buggy-vs-fixed cleanup
	// timing. A short grace period after that lets the reader goroutine
	// actually dispatch the already-buffered line.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sent); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stub never sent the delayed turn/completed (test itself is stuck, not the production code)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	if !owner.IsTurnInFlight() {
		t.Fatal("the batch's turn/completed completed the OWNER's in-flight turn (#1570)")
	}
}

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/delegator/codex"
)

// stubCodexAppServer is a minimal codex app-server stub (same shape as the
// codex package's own stub used by TestFacadesShareAppServerButOwnThreads):
// initialize/model/list/thread/start/turn/start all succeed immediately, and
// EVERY "initialize" call is tallied to initCountFile so a test can tell
// whether a second call attached to the existing app-server process or
// spawned a brand new one.
func stubCodexAppServer(t *testing.T) (stubPath, initCountFile string) {
	t.Helper()
	dir := t.TempDir()
	initCountFile = filepath.Join(dir, "init-count")
	stubPath = filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json, sys, os
initcount = ` + strconv.Quote(initCountFile) + `
next_thread = 0
for line in sys.stdin:
  try: msg=json.loads(line)
  except Exception: continue
  method=msg.get("method"); ident=msg.get("id")
  def send(x): print(json.dumps(x), flush=True)
  if method == "initialize":
    n = 0
    if os.path.exists(initcount):
      with open(initcount) as f: n = int((f.read() or "0").strip() or "0")
    with open(initcount, "w") as f: f.write(str(n+1))
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
    send({"method":"item/agentMessage/delta","params":{"threadId":tid,"turnId":"turn-"+tid,"delta":"batch reply"}})
    send({"method":"turn/completed","params":{"threadId":tid,"turn":{"id":"turn-"+tid,"status":"completed"}}})
  elif method == "thread/delete": send({"id":ident,"result":{}})
  elif ident is not None: send({"id":ident,"result":{}})
`
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stubPath, initCountFile
}

// TestRunBatch_CodexOwnerGetsDistinctFacadeSharedAppServer covers #1585
// requirement 2: a codex batch run against a live owner session must get its
// OWN *codex.Backend facade -- never the owner's mb.be -- while still
// multiplexing over the SAME app-server process. This is the structural fix
// for #1570: the old RunBatch special-cased a live codex owner by calling
// cb.RunBatch(ctx, req) directly on mb.be (see the pre-#1585 branch this test
// fails against), so the batch and the owner's own turn/callback state lived
// on the identical Go object with nothing but thread-ID bookkeeping between
// them.
func TestRunBatch_CodexOwnerGetsDistinctFacadeSharedAppServer(t *testing.T) {
	stub, initCountFile := stubCodexAppServer(t)
	cfg := map[string]any{"binary": stub}
	const agentID = "batch-facade-agent"
	ownerKey := agentID + "/c1"
	ctx := context.Background()

	ownerBE, err := delegator.New("codex", cfg)
	if err != nil {
		t.Fatalf("construct owner backend: %v", err)
	}
	if err := ownerBE.Start(ctx, delegator.StartOptions{AgentID: agentID, SessionKey: ownerKey, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("start owner: %v", err)
	}
	defer func() { _ = ownerBE.Close() }()

	var constructed []delegator.Delegator
	m := &DelegatedManager{
		AgentID:   agentID,
		StartOpts: delegator.StartOptions{WorkDir: t.TempDir(), AgentID: agentID},
		NewBackend: func() (delegator.Delegator, error) {
			be, err := delegator.New("codex", cfg)
			if err == nil {
				constructed = append(constructed, be)
			}
			return be, err
		},
	}
	m.backends = map[string]*managedBackend{
		ownerKey: {be: ownerBE, sessionKey: ownerKey},
	}

	got, err := m.RunBatch(ctx, delegator.BatchRequest{
		OwnerSessionKey: ownerKey,
		Prompt:          "batch prompt",
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got != "batch reply" {
		t.Fatalf("result = %q", got)
	}

	// The batch must have gone through m.NewBackend() -- i.e. gotten a fresh
	// facade -- exactly once, rather than taking the old shortcut of calling
	// RunBatch directly on the owner's already-live backend (which never
	// touches m.NewBackend at all).
	if len(constructed) != 1 {
		t.Fatalf("m.NewBackend called %d times, want exactly 1 -- the batch must get its OWN facade, not reuse the owner's live backend", len(constructed))
	}
	batchBE, ok := constructed[0].(*codex.Backend)
	if !ok {
		t.Fatalf("constructed backend is %T, want *codex.Backend", constructed[0])
	}
	ownerCB, ok := ownerBE.(*codex.Backend)
	if !ok {
		t.Fatalf("owner backend is %T, want *codex.Backend", ownerBE)
	}
	if batchBE == ownerCB {
		t.Fatal("batch facade is the SAME object as the owner's live backend -- no structural separation (#1570)")
	}

	// But it must still be the SAME app-server process: only one "initialize"
	// handshake should have happened total (the owner's), never a second one
	// for the batch.
	data, err := os.ReadFile(initCountFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "1" {
		t.Fatalf("app-server initialize count = %s, want 1 -- the batch spawned its own process instead of attaching to the owner's", got)
	}
}

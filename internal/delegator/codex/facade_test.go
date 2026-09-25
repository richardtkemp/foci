package codex

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"foci/internal/delegator"
)

func stubAppServer(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.jsonl")
	stub := filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json, sys
capture = ` + strconv.Quote(capture) + `
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
      send({"method":"item/agentMessage/delta","params":{"threadId":tid,"turnId":"turn-"+tid,"delta":"batch reply"}})
      send({"method":"turn/completed","params":{"threadId":tid,"turn":{"id":"turn-"+tid,"status":"completed"}}})
    elif method == "thread/delete": send({"id":ident,"result":{}})
    elif ident is not None: send({"id":ident,"result":{}})
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub, capture
}

func TestTomlBasicString(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `"plain"`}, {"a\nb", `"a\nb"`}, {`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`}, {"tab\there", `"tab\there"`}, {"bell\x07", `"bell\u0007"`},
	}
	for _, c := range cases {
		if got := tomlBasicString(c.in); got != c.want {
			t.Errorf("tomlBasicString(%q)=%s want %s", c.in, got, c.want)
		}
	}
}

func TestBatchOnlyThreadStartedCannotBecomeInteractiveRoot(t *testing.T) {
	b := newTestBackend(t)
	b.startOpts.BatchOnly = true
	b.threadID = ""
	b.threadMapMu.Lock()
	delete(b.sessionThreads, b.startOpts.SessionKey)
	b.threadMapMu.Unlock()
	b.onThreadStarted(&threadStartedParams{Thread: threadInfo{ID: "thread-ephemeral"}})
	if got := b.SessionID(); got != "" {
		t.Fatalf("SessionID() = %q, want no interactive mapping", got)
	}
}

// stubAppServerRefusingSecondThreadStart behaves like stubAppServer except the
// SECOND thread/start is rejected — i.e. the app-server is up and the first
// facade owns it, and a later facade's attach fails. That is the exact shape of
// a transient codex-side hiccup, and the path where a pool ref can leak.
func stubAppServerRefusingSecondThreadStart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json, sys
started = 0
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
    if started >= 1:
      send({"id":ident,"error":{"code":-32000,"message":"stub: thread/start refused"}})
    else:
      started += 1; tid="thread-%d" % started
      thread={"id":tid,"path":None,"status":{"type":"idle"}}
      send({"id":ident,"result":{"thread":thread,"model":"test-model"}})
      send({"method":"thread/started","params":{"thread":thread}})
  elif ident is not None: send({"id":ident,"result":{}})
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// The facade-attach path increments the shared-pool refcount BEFORE it tries to
// establish its thread. When that fails the ref must be given back: otherwise
// refs stays permanently above the number of live facades, closeIdle can never
// drive it to zero, and the agent's app-server is never reaped — a leak that
// compounds with every transient failure.
func TestFailedFacadeAttachReleasesPoolRef(t *testing.T) {
	stub := stubAppServerRefusingSecondThreadStart(t)
	cfg := map[string]any{"binary": stub}
	be1, _ := newFromConfig(cfg)
	be2, _ := newFromConfig(cfg)
	b1, b2 := be1.(*Backend), be2.(*Backend)
	ctx := context.Background()
	const agent = "refleak-agent"

	if err := b1.Start(ctx, delegator.StartOptions{AgentID: agent, SessionKey: "session/a", WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("start owner facade: %v", err)
	}
	defer b1.Close()

	sharedPool.Lock()
	before := sharedPool.refs[agent]
	sharedPool.Unlock()

	if err := b2.Start(ctx, delegator.StartOptions{AgentID: agent, SessionKey: "session/b", WorkDir: t.TempDir()}); err == nil {
		_ = b2.Close()
		t.Fatal("second facade started successfully; the stub was supposed to refuse its thread/start")
	}

	sharedPool.Lock()
	after := sharedPool.refs[agent]
	sharedPool.Unlock()
	if after != before {
		t.Errorf("pool refs = %d after a failed attach, want %d — the leaked ref means closeIdle can never reap this agent's app-server", after, before)
	}
}

func TestFacadesShareAppServerButOwnThreads(t *testing.T) {
	stub, _ := stubAppServer(t)
	cfg := map[string]any{"binary": stub}
	be1, _ := newFromConfig(cfg)
	be2, _ := newFromConfig(cfg)
	b1, b2 := be1.(*Backend), be2.(*Backend)
	ctx := context.Background()
	if err := b1.Start(ctx, delegator.StartOptions{AgentID: "facade-agent", SessionKey: "session/a", WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("start first facade: %v", err)
	}
	if err := b2.Start(ctx, delegator.StartOptions{AgentID: "facade-agent", SessionKey: "session/b", WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("start second facade: %v", err)
	}
	defer b1.Close()
	defer b2.Close()
	if b1.process() != b2.process() {
		t.Fatal("facades did not share the app-server owner")
	}
	if b1.SessionID() == "" || b2.SessionID() == "" || b1.SessionID() == b2.SessionID() {
		t.Fatalf("thread IDs = %q/%q, want distinct non-empty IDs", b1.SessionID(), b2.SessionID())
	}
	if b1.SessionIDFor("session/b") != "" {
		t.Fatal("facade leaked sibling session mapping")
	}
}

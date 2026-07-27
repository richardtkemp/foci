package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// capturedCall is one JSON-RPC line the stub app-server recorded on stdin.
type capturedCall struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// readCapturedCalls parses the stub's capture file into the calls foci made.
// Parsing beats substring matching on the raw text: the assertions then can't
// pass or fail on JSON formatting (the encoder emits compact JSON, so a check
// written as `"method": "exec"` with a space silently never fires), and a
// missing field is distinguishable from a differently-spelled one.
func readCapturedCalls(t *testing.T, capture string) []capturedCall {
	t.Helper()
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var calls []capturedCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var c capturedCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("capture line is not JSON-RPC: %q (%v)", line, err)
		}
		calls = append(calls, c)
	}
	return calls
}

// firstCall returns the params of the first call to method, failing if absent.
func firstCall(t *testing.T, calls []capturedCall, method string) json.RawMessage {
	t.Helper()
	for _, c := range calls {
		if c.Method == method {
			return c.Params
		}
	}
	t.Fatalf("app-server never received %s; got %v", method, methodsOf(calls))
	return nil
}

func methodsOf(calls []capturedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Method)
	}
	return out
}

func TestRunBatchUsesEphemeralAppServerThread(t *testing.T) {
	stub, capture := stubAppServer(t)
	be, _ := newFromConfig(map[string]any{"binary": stub})
	b := be.(*Backend)
	workDir := t.TempDir()
	got, err := b.RunBatch(context.Background(), delegator.BatchRequest{
		Prompt: "extract the rules", SystemPrompt: "system",
		WorkDir: workDir, SessionKey: "test/i-batch",
	})
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if got != "batch reply" {
		t.Fatalf("result=%q", got)
	}

	// The stub answers "batch reply" whatever it is sent, so the RESULT proves
	// only that the round trip completed. What the request actually carried has
	// to be asserted against the capture, or a regression that drops the
	// prompt, system prompt or workdir passes unnoticed (#1579).
	calls := readCapturedCalls(t, capture)

	var start threadStartParams
	if err := json.Unmarshal(firstCall(t, calls, "thread/start"), &start); err != nil {
		t.Fatalf("parse thread/start params: %v", err)
	}
	if !start.Ephemeral {
		t.Error("thread/start was not ephemeral — a batch must not create a durable thread")
	}
	if start.BaseInstructions != "system" {
		t.Errorf("thread/start baseInstructions = %q, want %q — the system prompt was dropped", start.BaseInstructions, "system")
	}
	if start.Cwd != workDir {
		t.Errorf("thread/start cwd = %q, want %q", start.Cwd, workDir)
	}

	var turn turnStartParams
	if err := json.Unmarshal(firstCall(t, calls, "turn/start"), &turn); err != nil {
		t.Fatalf("parse turn/start params: %v", err)
	}
	if len(turn.Input) != 1 || turn.Input[0].Text != "extract the rules" {
		t.Errorf("turn/start input = %+v, want the caller's prompt — RunBatch could send an empty prompt and this suite would not notice", turn.Input)
	}
	if turn.ThreadID == "" {
		t.Error("turn/start carried no threadId")
	}
	// These two enums use OPPOSITE casing conventions, verified against a live
	// codex 0.145.0 app-server rather than inferred:
	//
	//   thread/start params.sandbox           -> kebab: read-only, workspace-write, danger-full-access
	//   turn/start   params.sandboxPolicy.type -> camel: readOnly, workspaceWrite, dangerFullAccess, externalSandbox
	//
	// Sending the kebab spelling here is rejected outright with JSON-RPC
	// -32600 "unknown variant `danger-full-access`", which fails the whole
	// batch at turn/start. A stub app-server cannot catch that (it accepts any
	// string), so this asserts the exact wire literal — the only form of the
	// check that would have caught the real bug.
	if turn.SandboxPolicy == nil {
		t.Fatal("turn/start carried no sandboxPolicy")
	}
	if turn.SandboxPolicy.Type != "dangerFullAccess" {
		t.Errorf("turn/start sandboxPolicy.type = %q, want %q — codex rejects the kebab spelling with -32600 and the batch never runs", turn.SandboxPolicy.Type, "dangerFullAccess")
	}

	// NOTE: the removed `"method": "exec"` assertion was dead twice over — it
	// searched for a space after the colon that the compact encoder never
	// emits, and `exec` was the OLD `codex exec` CLI subcommand, passed via
	// argv and never as a JSON-RPC method. A regression back to the CLI would
	// produce NO stdin line at all rather than a matching one, so stdin capture
	// cannot detect it. Asserting thread/start + turn/start actually happened
	// is the observable form of the same intent.
}

func TestBatchNotificationRouting(t *testing.T) {
	b := newTestBackend(t)
	b.batchRuns = map[string]*batchRun{"thread-1": {done: make(chan batchResult, 1)}}
	if !b.handleBatchNotification("item/agentMessage/delta", []byte(`{"threadId":"thread-1","delta":"hello"}`)) {
		t.Fatal("delta not routed")
	}
	if !b.handleBatchNotification("turn/completed", []byte(`{"threadId":"thread-1","turn":{"status":"completed"}}`)) {
		t.Fatal("completion not routed")
	}
	got := <-b.batchRuns["thread-1"].done
	if got.text != "hello" || got.err != nil {
		t.Fatalf("result=%+v", got)
	}
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

func TestBatchNotificationRoutesThreadStartedByNestedID(t *testing.T) {
	b := newTestBackend(t)
	b.batchRuns = map[string]*batchRun{"thread-ephemeral": {done: make(chan batchResult, 1)}}
	if !b.handleBatchNotification("thread/started", []byte(`{"thread":{"id":"thread-ephemeral"}}`)) {
		t.Fatal("nested thread/started was not routed to batch")
	}
}

// A batch thread's events must ALL be consumed, not just the handful the
// switch extracts data from. RunBatch shares the owner session's live backend,
// so an event that falls through reaches the owner's interactive handlers:
// item/reasoning/*Delta streams the batch's thinking into the user's chat,
// thread/tokenUsage/updated overwrites the owner's usage, and
// thread/name/updated renames the owner's chat. The methods below are the ones
// that actually leaked; the trailing unknown method guards the general case, so
// a future codex event type cannot reopen this by simply not being listed.
func TestBatchNotificationConsumesUnlistedMethods(t *testing.T) {
	for _, method := range []string{
		"item/started",
		"item/reasoning/summaryPartDelta",
		"thread/tokenUsage/updated",
		"thread/name/updated",
		"some/method/codex/has/not/invented/yet",
	} {
		t.Run(method, func(t *testing.T) {
			b := newTestBackend(t)
			b.batchRuns = map[string]*batchRun{"thread-1": {done: make(chan batchResult, 1)}}
			if !b.handleBatchNotification(method, []byte(`{"threadId":"thread-1"}`)) {
				t.Errorf("%s on a batch thread fell through to the owner's interactive handlers", method)
			}
		})
	}
}

// The consume-everything default must not swallow events for threads that are
// NOT batch runs — those are the owner's and have to reach the interactive
// handlers as before.
func TestBatchNotificationIgnoresNonBatchThreads(t *testing.T) {
	b := newTestBackend(t)
	b.batchRuns = map[string]*batchRun{"thread-1": {done: make(chan batchResult, 1)}}
	if b.handleBatchNotification("item/started", []byte(`{"threadId":"thread-interactive"}`)) {
		t.Error("consumed an event for a non-batch thread")
	}
	if b.handleBatchNotification("item/started", []byte(`{}`)) {
		t.Error("consumed an event carrying no thread id")
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

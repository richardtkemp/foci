package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/turnevent"
)

// callExecBridge sends one tool request over a real exec bridge socket, as
// foci-call does, and returns the result text or the error text.
func callExecBridge(t *testing.T, sockPath, toolName string, params any) (result, errMsg string) {
	t.Helper()
	conn, err := net.DialTimeout("unix", sockPath, 10*time.Second)
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second)) // hang guard only
	p, _ := json.Marshal(params)
	if _, err := fmt.Fprintf(conn, `{"tool":%q,"params":%s}`+"\n", toolName, p); err != nil {
		t.Fatalf("write request: %v", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() {
		t.Fatalf("read response: %v", sc.Err())
	}
	var resp struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("parse response %q: %v", sc.Text(), err)
	}
	return resp.Result, resp.Error
}

// TestRemappedBridgeRunsToolsAsBranch is the #3067 F5 regression. On a reset
// the live backend moves to a reflection branch key (RemapSession) and keeps
// its exec bridge, because FOCI_SOCK is baked into the process. Every foci
// tool call from that process must then run as the branch: its session key,
// and so its session type, which bars a reflection from messaging other
// sessions. Before the fix the bridge kept the root key it was created with,
// so the reflection acted as the live chat and passed the send bar.
func TestRemappedBridgeRunsToolsAsBranch(t *testing.T) {
	t.Parallel()
	const (
		rootKey   = "test-agent/c1"
		branchKey = "test-agent/c1/b1700000000"
	)
	types := map[string]session.SessionType{
		rootKey:   session.SessionTypeChat,
		branchKey: session.SessionTypeReflection,
	}

	reg := tools.NewRegistry()
	reg.Register(&tools.Tool{
		Name:       "whoami_probe",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.TextResult(tools.SessionKeyFromContext(ctx)), nil
		},
	})
	// The real send_to_session tool. The reflection bar refuses before the
	// tool touches its store or notifier, so nil is safe for the barred call;
	// the chat-side control below only reads the caller identity.
	reg.Register(tools.NewSendToSessionTool(nil, nil, nil, nil, nil,
		func(sk string) session.SessionType { return types[sk] }))

	idx := newTestSessionIndex(t)
	mgr, _ := newTestManager(t, idx)
	mgr.StartOpts.ExecRegistry = reg

	if _, err := mgr.Get(context.Background(), rootKey); err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	mb, ok := mgr.getManaged(rootKey)
	if !ok || mb.bridge == nil {
		t.Fatal("root backend has no exec bridge")
	}
	sock := mb.bridge.SockPath()

	// Control: before the handover the process runs as the chat.
	if got, e := callExecBridge(t, sock, "whoami_probe", map[string]any{}); e != "" || got != rootKey {
		t.Fatalf("before remap: whoami = %q (err %q), want %q", got, e, rootKey)
	}

	if !mgr.RemapSession(rootKey, branchKey) {
		t.Fatal("RemapSession failed")
	}

	// The same process, over the same socket, is now the reflection branch.
	if got, e := callExecBridge(t, sock, "whoami_probe", map[string]any{}); e != "" || got != branchKey {
		t.Errorf("after remap: tool call ran as %q (err %q), want the branch %q", got, e, branchKey)
	}
	_, e := callExecBridge(t, sock, "send_to_session", map[string]string{"session_key": "other/c9", "message": "hi"})
	if !strings.Contains(e, "disabled in reflection") {
		t.Errorf("after remap: send_to_session error = %q, want the reflection send bar", e)
	}

	// The fresh backend that takes over the chat runs as the chat.
	if _, err := mgr.Get(context.Background(), rootKey); err != nil {
		t.Fatalf("Get(root, fresh): %v", err)
	}
	fresh, ok := mgr.getManaged(rootKey)
	if !ok || fresh.bridge == nil {
		t.Fatal("fresh root backend has no exec bridge")
	}
	if got, e := callExecBridge(t, fresh.bridge.SockPath(), "whoami_probe", map[string]any{}); e != "" || got != rootKey {
		t.Errorf("fresh root backend: whoami = %q (err %q), want %q", got, e, rootKey)
	}
}

// textSink records the TextBlock texts it receives: the stand-in for a running
// turn's delivering sink.
type textSink struct {
	mu    sync.Mutex
	texts []string
}

func (s *textSink) Emit(_ context.Context, ev turnevent.Event) {
	if e, ok := ev.(turnevent.TextBlock); ok {
		s.mu.Lock()
		s.texts = append(s.texts, e.Text)
		s.mu.Unlock()
	}
}
func (s *textSink) DeliversToPlatform() bool { return true }
func (s *textSink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

// TestSystemTurn_EarlyTextNeverReachesChat is the #3067 F6 regression. A
// backend can emit a turn's first text as soon as the turn's input lands,
// before RunInference returns. A non-delivering turn (reflection) used to
// register its suppressing sink only after RunInference, so that text found
// no sink and went to the session's delivering fallback: the chat. The mock
// emits synchronously inside the begin, the earliest a real backend can.
func TestSystemTurn_EarlyTextNeverReachesChat(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"
	be := &batchTurnBackend{}
	be.sessionFile = "/tmp/early-text.jsonl"
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		h.OnText("private reflection text")
		h.OnTurnComplete(&delegator.TurnResult{Text: "private reflection text", Model: "claude-sonnet-4-5"})
		return nil, nil
	}
	a := newBatchTestAgent(t, be)
	conn := &leakConn{}
	a.ResolveLateConn = func(string) platform.Connection { return conn }

	if err := a.HandleMessage(WithTrigger(context.Background(), "reflection"), sk, []string{"reflect"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if calls := conn.got(); len(calls) != 0 {
		t.Errorf("reflection text emitted at the turn's start reached the chat:\n  %s", strings.Join(calls, "\n  "))
	}
	if a.sessionRouter(sk).current.Load() != nil {
		t.Error("the reflection turn's sink is still registered after it ended")
	}
}

// TestSystemTurn_RefusedBeginKeepsRunningTurnSink pins the other half of
// #3067 F6: registering the system turn's sink earlier must not let a system
// turn that is still WAITING (its begin refused because another turn runs)
// take that turn's sink. The running turn's text must keep reaching its own
// sink while the system turn waits; then the system turn begins and its text
// is suppressed.
func TestSystemTurn_RefusedBeginKeepsRunningTurnSink(t *testing.T) {
	initTestConvo(t, "helen")
	const sk = "helen/c42"
	be := &batchTurnBackend{}
	be.sessionFile = "/tmp/refused-begin.jsonl"
	be.sendToPaneFn = func(_ context.Context, _ string, h *mockHandler) (*delegator.TurnResult, error) {
		h.OnText("private reflection text")
		h.OnTurnComplete(&delegator.TurnResult{Text: "done", Model: "claude-sonnet-4-5"})
		return nil, nil
	}
	a := newBatchTestAgent(t, be)
	conn := &leakConn{}
	a.ResolveLateConn = func(string) platform.Connection { return conn }

	// A turn is running (an adopted autonomous run, say) with its sink
	// registered. The backend must exist first so delivery is attached.
	if _, err := a.DelegatedManager.Get(context.Background(), sk); err != nil {
		t.Fatalf("Get: %v", err)
	}
	running := &textSink{}
	releaseRunning := a.sessionRouter(sk).Register(running)
	be.mu.Lock()
	be.turnInFlight = true
	be.mu.Unlock()

	var waited sync.Once
	be.waitForTurnFn = func(context.Context) error {
		// Called only after the system turn's begin was refused.
		waited.Do(func() {
			if cur := a.sessionRouter(sk).current.Load(); cur == nil || cur.s != turnevent.Sink(running) {
				t.Errorf("a refused system begin replaced the running turn's sink (now %v)", cur)
			}
			be.mu.Lock()
			se := be.sessionEvents
			be.mu.Unlock()
			se.OnText("running turn text")
			// The running turn ends.
			releaseRunning()
			be.mu.Lock()
			be.turnInFlight = false
			be.mu.Unlock()
		})
		return nil
	}

	if err := a.HandleMessage(WithTrigger(context.Background(), "reflection"), sk, []string{"reflect"}, nil); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if got := running.got(); len(got) != 1 || got[0] != "running turn text" {
		t.Errorf("running turn's sink got %q, want only its own text", got)
	}
	if calls := conn.got(); len(calls) != 0 {
		t.Errorf("text reached the chat fallback:\n  %s", strings.Join(calls, "\n  "))
	}
}

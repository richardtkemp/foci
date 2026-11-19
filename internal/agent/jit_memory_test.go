package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/jitmemory"
	"foci/internal/memory"
	"foci/internal/turnevent"
)

// jitHarness is an agent with a JIT memory engine over one note, a fast
// model that answers at release (closed = at once) and a recording
// JITDeliverFn.
type jitHarness struct {
	a       *Agent
	release chan struct{}

	mu        sync.Mutex
	delivered []string
	runs      int
}

func (h *jitHarness) deliveries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.delivered...)
}

func (h *jitHarness) runCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs
}

func newJITHarness(t *testing.T, steers bool, wait time.Duration) *jitHarness {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("## Plex2Shell\nPlex2Shell notes: the web search plugin.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &jitHarness{release: make(chan struct{})}
	h.a = newTestAgent(t)
	mgr := newMockDelegatedManager(t, &recordingBackend{})
	withMidTurnSteer(mgr, steers)
	h.a.DelegatedManager = mgr
	rc := &config.ResolvedAgentConfig{JITMemory: config.ResolvedJITMemory{Enabled: true, SkipChars: 10, ContextChars: 80,
		ContextMessages: 4, TopN: 5, Wait: wait, DedupeCompactions: 3, InjectMaxChars: 3000, SectionMaxChars: 1200,
		QueryStrategy: jitmemory.QueryRaw}}
	h.a.LiveConfigFn = func() *config.ResolvedAgentConfig { return rc }
	h.a.JIT = jitmemory.New(jitmemory.Deps{
		AgentID: "test",
		Search: func(string) ([]memory.Result, error) {
			h.mu.Lock()
			h.runs++
			h.mu.Unlock()
			return []memory.Result{{Path: "n.md", Source: "s", Snippet: ">Plex2Shell< notes"}}, nil
		},
		SourceDirs: func() map[string]string { return map[string]string{"s": dir} },
		Model: func(ctx context.Context, what, model, system, user string, _ int) (string, jitmemory.CallReport, error) {
			select {
			case <-h.release:
			case <-ctx.Done():
				return "", jitmemory.CallReport{}, ctx.Err()
			}
			return "1", jitmemory.CallReport{}, nil
		},
	})
	h.a.JITDeliverFn = func(sk, text string) {
		h.mu.Lock()
		h.delivered = append(h.delivered, sk+"|"+text)
		h.mu.Unlock()
	}
	return h
}

func jitTurn(text string) *TurnState {
	return &TurnState{Ctx: context.Background(), SessionKey: "test/c1", Trigger: "voice", Texts: []string{text}, Prompt: "PROMPT"}
}

// A result ready within the wait is appended to the user's prompt and sent
// nowhere else.
func TestJIT_UserMessage_AttachedInTime(t *testing.T) {
	h := newJITHarness(t, true, 5*time.Second)
	close(h.release)
	ts := jitTurn("what about Plex2Shell web search?")
	p := h.a.startUserJIT(ts)
	h.a.attachUserJIT(ts, p)
	p.abandon()
	if !strings.HasPrefix(ts.Prompt, "PROMPT\n\n"+jitmemory.InjectionHeader) || !strings.Contains(ts.Prompt, "web search plugin") {
		t.Fatalf("prompt = %q", ts.Prompt)
	}
	if d := h.deliveries(); len(d) != 0 {
		t.Fatalf("an attached result was also injected: %v", d)
	}
}

// Past the wait the message goes alone and the late result is injected.
func TestJIT_UserMessage_LateResultInjected(t *testing.T) {
	h := newJITHarness(t, true, 20*time.Millisecond)
	ts := jitTurn("what about Plex2Shell web search?")
	p := h.a.startUserJIT(ts)
	h.a.attachUserJIT(ts, p)
	if ts.Prompt != "PROMPT" {
		t.Fatalf("prompt changed after the wait ran out: %q", ts.Prompt)
	}
	close(h.release)
	if !waitFor(5*time.Second, func() bool { return len(h.deliveries()) == 1 }) {
		t.Fatalf("late result not injected: %v", h.deliveries())
	}
	if d := h.deliveries()[0]; !strings.HasPrefix(d, "test/c1|"+jitmemory.InjectionHeader) {
		t.Fatalf("delivery = %q", d)
	}
}

// Q109: on a backend that takes no message mid-turn, a late result is dropped.
func TestJIT_UserMessage_LateResultDroppedWithoutSteer(t *testing.T) {
	h := newJITHarness(t, false, 20*time.Millisecond)
	h.a.deliverLateJIT("test/c1", jitmemory.Outcome{Injection: "notes"})
	if d := h.deliveries(); len(d) != 0 {
		t.Fatalf("late result injected on a non-steering backend: %v", d)
	}
}

// A turn that ends before its attach point hands a ready result on.
func TestJIT_UserMessage_AbandonedTurnInjects(t *testing.T) {
	h := newJITHarness(t, true, time.Second)
	close(h.release)
	ts := jitTurn("what about Plex2Shell web search?")
	p := h.a.startUserJIT(ts)
	<-p.done
	p.abandon()
	if !waitFor(5*time.Second, func() bool { return len(h.deliveries()) == 1 }) {
		t.Fatalf("abandoned result not injected: %v", h.deliveries())
	}
}

func TestJIT_UserMessage_NotForSystemTurnsOrSpawns(t *testing.T) {
	h := newJITHarness(t, true, time.Second)
	ts := jitTurn("what about Plex2Shell web search?")
	ts.Trigger = "keepalive"
	if h.a.startUserJIT(ts) != nil {
		t.Fatal("a keepalive turn ran JIT memory")
	}
	ts = jitTurn("what about Plex2Shell web search?")
	ts.SessionKey = "test/c1/b1700000000"
	if h.a.startUserJIT(ts) != nil {
		t.Fatal("a branch session ran JIT memory")
	}
}

func agentMsgCtx(trigger string) context.Context {
	return turnevent.WithSink(WithTrigger(context.Background(), trigger), &eventCaptureSink{})
}

// An agent message on a steering backend is searched and its result sent at
// once.
func TestJIT_AgentMessage_Injected(t *testing.T) {
	h := newJITHarness(t, true, time.Second)
	close(h.release)
	h.a.jitOnAgentMessage(agentMsgCtx("voice"), "test/c1", "I will look at the Plex2Shell web search now.")
	if !waitFor(5*time.Second, func() bool { return len(h.deliveries()) == 1 }) {
		t.Fatalf("agent-message result not injected: %v", h.deliveries())
	}
}

// Not on a backend without mid-turn steer, not for the reply to a JIT
// result, not for a silent reply.
func TestJIT_AgentMessage_Skips(t *testing.T) {
	cases := []struct {
		name   string
		steers bool
		ctx    context.Context
		text   string
	}{
		{"no steer", false, agentMsgCtx("voice"), "I will look at the Plex2Shell web search now."},
		{"jit turn", true, agentMsgCtx(TriggerJITMemory), "I will look at the Plex2Shell web search now."},
		{"silent", true, agentMsgCtx("voice"), NoResponseSentinel},
	}
	for _, c := range cases {
		h := newJITHarness(t, c.steers, time.Second)
		close(h.release)
		// Every skip returns before a run starts, so the check is synchronous.
		if _, run := h.a.jitAgentMessageRuns(c.ctx, "test/c1", c.text); run {
			t.Errorf("%s: would run", c.name)
		}
		h.a.jitOnAgentMessage(c.ctx, "test/c1", c.text)
		if h.runCount() != 0 || len(h.deliveries()) != 0 {
			t.Errorf("%s: runs=%d deliveries=%v, want none", c.name, h.runCount(), h.deliveries())
		}
	}
}

// Off: nothing runs.
func TestJIT_Disabled(t *testing.T) {
	h := newJITHarness(t, true, time.Second)
	close(h.release)
	off := &config.ResolvedAgentConfig{}
	h.a.LiveConfigFn = func() *config.ResolvedAgentConfig { return off }
	if h.a.startUserJIT(jitTurn("what about Plex2Shell web search?")) != nil {
		t.Fatal("disabled feature started a run")
	}
	if _, run := h.a.jitAgentMessageRuns(agentMsgCtx("voice"), "test/c1", "I will look at the Plex2Shell web search now."); run {
		t.Fatal("disabled feature searched")
	}
}

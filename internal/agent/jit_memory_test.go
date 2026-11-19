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
	"foci/internal/sqlite"
	"foci/internal/turnevent"
)

// jitHarness is an agent with a JIT memory engine over one note, a fast
// model that answers at release (closed = at once) and a recording
// JITDeliverFn.
type jitHarness struct {
	a       *Agent
	release chan struct{}

	dbPath string

	mu        sync.Mutex
	delivered []string
	runs      int
}

// rows returns the run log as "role|skipped" lines, in order.
func (h *jitHarness) rows(t *testing.T) []string {
	t.Helper()
	db, err := sqlite.Open(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := db.Query(`SELECT role, skipped FROM runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out []string
	for r.Next() {
		var role, skipped string
		if err := r.Scan(&role, &skipped); err != nil {
			t.Fatal(err)
		}
		out = append(out, role+"|"+skipped)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	return out
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
	h := &jitHarness{release: make(chan struct{}), dbPath: filepath.Join(t.TempDir(), "jit_memory.db")}
	store, err := jitmemory.OpenStore(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
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
		Store:   func() *jitmemory.Store { return store },
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

// An agent message on a steering backend is searched and its result sent at
// once.
func TestJIT_AgentMessage_Injected(t *testing.T) {
	h := newJITHarness(t, true, time.Second)
	close(h.release)
	h.a.jitOnAgentMessage("test/c1", "I will look at the Plex2Shell web search now.", true, "voice")
	if !waitFor(5*time.Second, func() bool { return len(h.deliveries()) == 1 }) {
		t.Fatalf("agent-message result not injected: %v", h.deliveries())
	}
}

// Not on a backend without mid-turn steer, not for the reply to a JIT
// result, not for a silent reply, not for text that reaches no chat; each
// skip writes a skipped row with its reason (#3176).
func TestJIT_AgentMessage_Skips(t *testing.T) {
	cases := []struct {
		name     string
		steers   bool
		delivers bool
		trigger  string
		text     string
		want     string
	}{
		{"no steer", false, true, "voice", "I will look at the Plex2Shell web search now.", jitSkipNoSteer},
		{"jit turn", true, true, TriggerJITMemory, "I will look at the Plex2Shell web search now.", jitSkipJITTurn},
		{"silent", true, true, "voice", NoResponseSentinel, jitSkipSilent},
		{"not delivered", true, false, "keepalive", "I will look at the Plex2Shell web search now.", jitSkipNoDelivery},
	}
	for _, c := range cases {
		h := newJITHarness(t, c.steers, time.Second)
		close(h.release)
		// Every skip returns before a run starts, so the check is synchronous.
		h.a.jitOnAgentMessage("test/c1", c.text, c.delivers, c.trigger)
		if h.runCount() != 0 || len(h.deliveries()) != 0 {
			t.Errorf("%s: runs=%d deliveries=%v, want none", c.name, h.runCount(), h.deliveries())
		}
		if got := h.rows(t); len(got) != 1 || got[0] != "agent|"+c.want {
			t.Errorf("%s: rows = %q, want one %q", c.name, got, "agent|"+c.want)
		}
	}
}

// A message that is not on the user's chat (a branch, a spawn) is not an
// agent message to the chat: no run and no row.
func TestJIT_AgentMessage_NotOnChatSession(t *testing.T) {
	h := newJITHarness(t, true, time.Second)
	close(h.release)
	h.a.jitOnAgentMessage("test/c1/b1700000000", "I will look at the Plex2Shell web search now.", true, "voice")
	if h.runCount() != 0 || len(h.rows(t)) != 0 {
		t.Fatalf("branch message: runs=%d rows=%q, want none", h.runCount(), h.rows(t))
	}
}

// #3176: on the delegated path the reply of a jit_memory turn arrives
// through the session router like any other text; the trigger its sink was
// built with (WrapConversationLogging) still stops a chain.
func TestJIT_AgentMessage_DelegatedJITTurnNoChain(t *testing.T) {
	h := newJITHarness(t, true, time.Second)
	close(h.release)
	be := &mockBackendDT{}
	h.a.AttachDelivery(be, "test/c1")
	release := h.a.sessionRouter("test/c1").Register(h.a.WrapConversationLogging(&eventCaptureSink{}, "test/c1", TriggerJITMemory))
	defer release()
	be.sessionEvents.OnText("I will look at the Plex2Shell web search now.")
	if h.runCount() != 0 || len(h.deliveries()) != 0 {
		t.Fatalf("a jit_memory turn's reply ran: runs=%d deliveries=%v", h.runCount(), h.deliveries())
	}
	if got := h.rows(t); len(got) != 1 || got[0] != "agent|"+jitSkipJITTurn {
		t.Fatalf("rows = %q, want one jit-turn skip", got)
	}
}

// #3176: a recorded non-delivered turn (reflection) on the chat session goes
// through a turn-kind logging sink; its text is recorded as not delivered.
func TestJIT_AgentMessage_RecordedTurnKindNotDelivered(t *testing.T) {
	h := newJITHarness(t, true, time.Second)
	close(h.release)
	sink := newTurnKindLoggingSink(&eventCaptureSink{}, h.a, 1, &TurnMetadata{}, "test/c1", "reflection")
	sink.Emit(context.Background(), turnevent.TextBlock{Text: "I will look at the Plex2Shell web search now.", Phase: turnevent.PhaseIntermediate})
	if h.runCount() != 0 || len(h.deliveries()) != 0 {
		t.Fatalf("a recorded turn's text ran: runs=%d deliveries=%v", h.runCount(), h.deliveries())
	}
	if got := h.rows(t); len(got) != 1 || got[0] != "agent|"+jitSkipNoDelivery {
		t.Fatalf("rows = %q, want one not-delivered skip", got)
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
	if _, run := h.a.jitAgentMessageRuns("test/c1", "I will look at the Plex2Shell web search now.", true, "voice"); run {
		t.Fatal("disabled feature searched")
	}
}

// #3176: on a delegated backend an agent message reaches the logging sink
// through the session router (AttachDelivery's OnText), whose ctx carries
// neither the turn's sink nor its trigger. A reply to the app chat must still
// run.
func TestJIT_AgentMessage_DelegatedSinkPath(t *testing.T) {
	h := newJITHarness(t, true, time.Second)
	close(h.release)
	be := &mockBackendDT{}
	h.a.AttachDelivery(be, "test/c1")
	release := h.a.sessionRouter("test/c1").Register(newLoggingSink(&eventCaptureSink{}, h.a, 1, &TurnMetadata{}, "test/c1"))
	defer release()
	be.sessionEvents.OnText("I will look at the Plex2Shell web search now.")
	if !waitFor(5*time.Second, func() bool { return len(h.deliveries()) == 1 }) {
		t.Fatalf("a delegated agent message did not run: deliveries=%v runs=%v", h.deliveries(), h.rows(t))
	}
}

package configapprove

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/config"
)

const baseConfig = `# keep me
[agent_loop]
max_tool_loops = 10

[[agents]]
id = "main"
model = "opus"
`

// harness is a Service over a temp config file with a fake presenter, a
// fake clock for the timeout and recorders for closes and live applies.
type harness struct {
	t       *testing.T
	path    string
	svc     *Service
	mu      sync.Mutex
	prompts []Prompt
	closed  []string
	applied []string
	timers  []func()
	protect error
	check   error
	present error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, path: filepath.Join(t.TempDir(), "foci.toml")}
	if err := os.WriteFile(h.path, []byte(baseConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	h.svc = New(Deps{
		ConfigPath:     h.path,
		FileMode:       0o600,
		CheckProtected: func(string) error { return h.protect },
		CheckContent:   func(string, string) error { return h.check },
		Present: func(p Prompt) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.present != nil {
				return h.present
			}
			h.prompts = append(h.prompts, p)
			return nil
		},
		Close: func(id, text string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.closed = append(h.closed, id+": "+text)
		},
		Apply: func(p config.KeyPath) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.applied = append(h.applied, p.String())
		},
		AfterFunc: func(_ time.Duration, f func()) func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.timers = append(h.timers, f)
			return func() bool { return true }
		},
	})
	return h
}

func (h *harness) file() string {
	data, err := os.ReadFile(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

func (h *harness) prompt(i int) Prompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.prompts) {
		h.t.Fatalf("no prompt %d (have %d)", i, len(h.prompts))
	}
	return h.prompts[i]
}

func (h *harness) set(path, value string) Status {
	return h.svc.Submit(Submission{AgentID: "main", SessionKey: "main/app/1", Op: OpSet, Path: path, Value: value})
}

func (h *harness) outcome(id string) Status {
	st, known := h.svc.Wait(context.Background(), id, time.Millisecond)
	if !known {
		h.t.Fatalf("request %s unknown", id)
	}
	return st
}

// TestApprove_AppliesAndWrites proves the approve path: the prompt names the
// session, path, old and new value; Approve writes the file (only the path
// changes, the comment stays), live-applies the path, and the outcome is
// applied.
func TestApprove_AppliesAndWrites(t *testing.T) {
	h := newHarness(t)
	st := h.set("agent_loop.max_tool_loops", "20")
	if st.Outcome != Pending {
		t.Fatalf("submit: %+v", st)
	}
	p := h.prompt(0)
	for _, want := range []string{"main/app/1", "agent_loop.max_tool_loops", "Current value:\n```\n10\n```", "New value:\n```\n20\n```"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p.Text)
		}
	}
	if h.file() != baseConfig {
		t.Fatal("file written before approval")
	}
	if reply := p.OnAnswer(true); !strings.HasPrefix(reply, "✅") {
		t.Errorf("reply = %q", reply)
	}
	if got := h.outcome(st.ID); got.Outcome != Applied {
		t.Fatalf("outcome = %+v", got)
	}
	if f := h.file(); !strings.Contains(f, "max_tool_loops = 20") || !strings.Contains(f, "# keep me") {
		t.Errorf("file:\n%s", f)
	}
	if len(h.applied) != 1 || h.applied[0] != "agent_loop.max_tool_loops" {
		t.Errorf("live applies = %v", h.applied)
	}
}

// TestDeny_LeavesFile proves Deny reports denied and writes nothing.
func TestDeny_LeavesFile(t *testing.T) {
	h := newHarness(t)
	st := h.set("agent_loop.max_tool_loops", "20")
	h.prompt(0).OnAnswer(false)
	if got := h.outcome(st.ID); got.Outcome != Denied {
		t.Fatalf("outcome = %+v", got)
	}
	if h.file() != baseConfig {
		t.Error("deny changed the file")
	}
}

// TestTimeout_LaterPressWritesNothing proves an unanswered request times
// out, its prompt is edited to say so, and a press after that writes
// nothing and keeps the timed-out outcome.
func TestTimeout_LaterPressWritesNothing(t *testing.T) {
	h := newHarness(t)
	st := h.set("agent_loop.max_tool_loops", "20")
	h.mu.Lock()
	fire := h.timers[0]
	h.mu.Unlock()
	fire()
	if got := h.outcome(st.ID); got.Outcome != TimedOut {
		t.Fatalf("outcome = %+v", got)
	}
	if len(h.closed) != 1 || !strings.Contains(h.closed[0], "Timed out") {
		t.Errorf("closes = %v", h.closed)
	}
	h.prompt(0).OnAnswer(true)
	if h.file() != baseConfig {
		t.Error("a press after the timeout wrote the file")
	}
	if got := h.outcome(st.ID); got.Outcome != TimedOut {
		t.Errorf("outcome after late press = %+v", got)
	}
}

// TestApprove_FileChangedSinceRequest proves the re-read on Approve: when the
// value at the path is no longer the old value shown, the outcome is failed
// and nothing is written.
func TestApprove_FileChangedSinceRequest(t *testing.T) {
	h := newHarness(t)
	st := h.set("agent_loop.max_tool_loops", "20")
	changed := strings.Replace(baseConfig, "max_tool_loops = 10", "max_tool_loops = 12", 1)
	if err := os.WriteFile(h.path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	h.prompt(0).OnAnswer(true)
	got := h.outcome(st.ID)
	if got.Outcome != Failed || !strings.Contains(got.Reason, "changed since the request") {
		t.Fatalf("outcome = %+v", got)
	}
	if h.file() != changed {
		t.Error("the file was written")
	}
}

// TestApprove_ChecksRerun proves the protection check and the config check
// run again on Approve: a failure of either is a failed outcome with nothing
// written.
func TestApprove_ChecksRerun(t *testing.T) {
	for name, set := range map[string]func(*harness){
		"protection":   func(h *harness) { h.protect = errors.New("config file is not protected") },
		"config check": func(h *harness) { h.check = errors.New("unknown key") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			st := h.set("agent_loop.max_tool_loops", "20")
			set(h)
			h.prompt(0).OnAnswer(true)
			if got := h.outcome(st.ID); got.Outcome != Failed {
				t.Fatalf("outcome = %+v", got)
			}
			if h.file() != baseConfig {
				t.Error("the file was written")
			}
		})
	}
}

// TestSubmit_InvalidNeverPrompts proves a request that fails validation — a
// bad path, a value that is not TOML, an unknown agent, a change the config
// check rejects, an unset of an absent key — is failed at once with a reason,
// shows no prompt, and leaves the file unchanged.
func TestSubmit_InvalidNeverPrompts(t *testing.T) {
	cases := map[string]Submission{
		"bad path":      {Op: OpSet, Path: "a..b", Value: "1"},
		"not TOML":      {Op: OpSet, Path: "agent_loop.max_tool_loops", Value: "twenty"},
		"unknown agent": {Op: OpSet, Path: "agents[ghost].model", Value: `"x"`},
		"absent unset":  {Op: OpUnset, Path: "agent_loop.nope"},
		"bad op":        {Op: "delete", Path: "agent_loop.max_tool_loops"},
	}
	for name, sub := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			sub.AgentID, sub.SessionKey = "main", "main/app/1"
			st := h.svc.Submit(sub)
			if st.Outcome != Failed || st.Reason == "" {
				t.Fatalf("status = %+v", st)
			}
			if len(h.prompts) != 0 || h.file() != baseConfig {
				t.Error("prompted or wrote")
			}
		})
	}
	t.Run("config check", func(t *testing.T) {
		h := newHarness(t)
		h.check = errors.New("unknown key x")
		if st := h.set("agent_loop.max_tool_loops", "20"); st.Outcome != Failed || !strings.Contains(st.Reason, "config check") {
			t.Fatalf("status = %+v", st)
		}
		if len(h.prompts) != 0 {
			t.Error("prompted")
		}
	})
}

// TestSubmit_NoChatFails proves a prompt no chat can show fails the request.
func TestSubmit_NoChatFails(t *testing.T) {
	h := newHarness(t)
	h.present = errors.New("no connection")
	if st := h.set("agent_loop.max_tool_loops", "20"); st.Outcome != Failed || !strings.Contains(st.Reason, "could not be shown") {
		t.Fatalf("status = %+v", st)
	}
}

// TestWait_UnknownID proves an id the service never issued (a restart) is
// reported unknown with a failed status, at once.
func TestWait_UnknownID(t *testing.T) {
	h := newHarness(t)
	st, known := h.svc.Wait(context.Background(), "cfg-nope", time.Hour)
	if known || st.Outcome != Failed || !strings.Contains(st.Reason, "restarted") {
		t.Fatalf("known=%v status=%+v", known, st)
	}
}

// TestUnset_Approve proves an approved unset removes the key.
func TestUnset_Approve(t *testing.T) {
	h := newHarness(t)
	st := h.svc.Submit(Submission{AgentID: "main", SessionKey: "main/app/1", Op: OpUnset, Path: "agents[main].model"})
	if st.Outcome != Pending || st.New != "(unset)" {
		t.Fatalf("status = %+v", st)
	}
	h.prompt(0).OnAnswer(true)
	if got := h.outcome(st.ID); got.Outcome != Applied {
		t.Fatalf("outcome = %+v", got)
	}
	if strings.Contains(h.file(), `model = "opus"`) {
		t.Error("key still in file")
	}
}

// TestClampTimeout pins the wait bounds.
func TestClampTimeout(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0:               DefaultTimeout,
		time.Second:     MinTimeout,
		5 * time.Minute: 5 * time.Minute,
		5 * time.Hour:   MaxTimeout,
	} {
		if got := ClampTimeout(in); got != want {
			t.Errorf("ClampTimeout(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestPromptText_ValuesFencedOnOwnLines pins #2936: the current and new
// values each sit in their own fenced code block (monospace on the app,
// Telegram and Discord), separated from the labels and from each other by
// line breaks, so a long value (an array of tables) cannot run into the
// other value.
func TestPromptText_ValuesFencedOnOwnLines(t *testing.T) {
	p, err := config.ParseKeyPath("agent_loop.max_tool_loops")
	if err != nil {
		t.Fatal(err)
	}
	got := promptText("main/app/1", p, "10", "20")
	want := "🔧 Config change requested\n\nSession: main/app/1\nSetting: agent_loop.max_tool_loops\n\n" +
		"Current value:\n```\n10\n```\n\nNew value:\n```\n20\n```\n\nNothing changes unless you approve."
	if got != want {
		t.Errorf("promptText =\n%s\nwant\n%s", got, want)
	}
}

// TestCodeFence_LongerThanAnyBacktickRun proves a value holding backticks
// still renders as one block: the fence is three backticks unless the value
// itself holds a run of three or more, and then one longer than its longest
// run (CommonMark closes a fence only on a run at least as long).
func TestCodeFence_LongerThanAnyBacktickRun(t *testing.T) {
	cases := map[string]string{
		"10":                "```",
		"\"a `b` c\"":       "```",
		"\"x ``y`` z\"":     "```",
		"\"x ```y``` z\"":   "````",
		"\"a\\n`````\\nb\"": "``````",
	}
	for v, want := range cases {
		if got := codeFence(v); got != want {
			t.Errorf("codeFence(%q) = %q, want %q", v, got, want)
		}
		block := fenced(v)
		if !strings.HasPrefix(block, want+"\n"+v+"\n"+want) {
			t.Errorf("fenced(%q) = %q", v, block)
		}
	}
}

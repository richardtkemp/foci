package configapprove

// #3122: an action request runs the caller's function on Approve, once, and
// never otherwise.

import (
	"errors"
	"strings"
	"testing"
)

func (h *harness) action(run func() (string, error)) Status {
	return h.svc.SubmitAction(ActionSubmission{
		AgentID: "main", SessionKey: "main/app/1",
		Title: "new agent stuart", Detail: "Workspace: /x/stuart", Run: run,
	})
}

func TestAction_ApproveRunsOnceAndReportsResult(t *testing.T) {
	h := newHarness(t)
	runs := 0
	st := h.action(func() (string, error) { runs++; return "✅ created", nil })
	if st.Outcome != Pending || st.ID == "" {
		t.Fatalf("submit = %+v, want pending with an id", st)
	}
	if runs != 0 {
		t.Fatal("the action ran before approval")
	}
	p := h.prompt(0)
	if !strings.Contains(p.Text, "new agent stuart") || !strings.Contains(p.Text, "Workspace: /x/stuart") || !strings.Contains(p.Text, "main/app/1") {
		t.Errorf("prompt text misses title, detail or session:\n%s", p.Text)
	}
	reply := p.OnAnswer(true)
	if runs != 1 {
		t.Fatalf("runs = %d after approve, want 1", runs)
	}
	if !strings.Contains(reply, "✅ created") {
		t.Errorf("prompt edited to %q, want the action's report", reply)
	}
	got := h.outcome(st.ID)
	if got.Outcome != Applied || got.Result != "✅ created" || got.Path != "new agent stuart" {
		t.Errorf("outcome = %+v, want applied with the report", got)
	}
	// A second press (double tap) does not run it again.
	_ = p.OnAnswer(true)
	if runs != 1 {
		t.Errorf("runs = %d after a second press, want 1", runs)
	}
	if h.file() != baseConfig {
		t.Error("an action request edited the file itself")
	}
}

func TestAction_DenyAndTimeoutNeverRun(t *testing.T) {
	h := newHarness(t)
	runs := 0
	run := func() (string, error) { runs++; return "", nil }

	st := h.action(run)
	h.prompt(0).OnAnswer(false)
	if got := h.outcome(st.ID); got.Outcome != Denied {
		t.Errorf("deny outcome = %+v", got)
	}

	st2 := h.action(run)
	h.mu.Lock()
	fire := h.timers[len(h.timers)-1]
	h.mu.Unlock()
	fire()
	if got := h.outcome(st2.ID); got.Outcome != TimedOut {
		t.Errorf("timeout outcome = %+v", got)
	}
	h.prompt(1).OnAnswer(true) // a press after the timeout
	if runs != 0 {
		t.Errorf("runs = %d, want 0 (denied, then timed out)", runs)
	}
}

func TestAction_RunErrorFails(t *testing.T) {
	h := newHarness(t)
	st := h.action(func() (string, error) { return "", errors.New("agent `stuart` already exists") })
	reply := h.prompt(0).OnAnswer(true)
	got := h.outcome(st.ID)
	if got.Outcome != Failed || !strings.Contains(got.Reason, "already exists") {
		t.Errorf("outcome = %+v, want failed with the run's error", got)
	}
	if !strings.Contains(reply, "not applied") {
		t.Errorf("prompt edited to %q", reply)
	}
}

func TestAction_ProtectionFailureDoesNotRun(t *testing.T) {
	h := newHarness(t)
	h.protect = errors.New("foci.toml is writable by others")
	runs := 0
	st := h.action(func() (string, error) { runs++; return "", nil })
	h.prompt(0).OnAnswer(true)
	if runs != 0 {
		t.Error("the action ran although the config file failed the protection check")
	}
	if got := h.outcome(st.ID); got.Outcome != Failed || !strings.Contains(got.Reason, "writable") {
		t.Errorf("outcome = %+v", got)
	}
}

func TestAction_NoChatFailsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.present = errors.New("no chat")
	st := h.action(func() (string, error) { t.Fatal("ran"); return "", nil })
	if st.Outcome != Failed || !strings.Contains(st.Reason, "no chat") {
		t.Errorf("submit = %+v, want failed", st)
	}
}

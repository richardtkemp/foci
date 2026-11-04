package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/tools"
	"foci/internal/tools/shell"
	"foci/internal/usercmd"
)

func userShellAgent(t *testing.T, limit int) *Agent {
	t.Helper()
	a := newTestAgent(t)
	a.MaxResultChars = limit
	a.UserShell = &shell.UserRunner{
		WorkDir:        t.TempDir(),
		SpillThreshold: func() int64 { return int64(limit) },
		SpillTempDir:   t.TempDir(),
		Timeout:        func() time.Duration { return 30 * time.Second },
	}
	return a
}

func TestRunUserCommand_UnderLimitIsDeliveredInFull(t *testing.T) {
	t.Parallel()
	res := userShellAgent(t, 1000).RunUserCommand(context.Background(), "a/s", "seq 1 5", nil)
	if res.ForAgent != "1\n2\n3\n4\n5\n" || res.File != "" {
		t.Errorf("ForAgent = %q File = %q", res.ForAgent, res.File)
	}
}

func TestRunUserCommand_OverLimitGetsPointerToFullFile(t *testing.T) {
	t.Parallel()
	a := userShellAgent(t, 1000)
	res := a.RunUserCommand(context.Background(), "a/s", "seq 1 200000", nil)
	if res.File == "" {
		t.Fatal("no saved file")
	}
	if !strings.Contains(res.ForAgent, res.File) || !strings.Contains(res.ForAgent, "Result too large") {
		t.Errorf("ForAgent = %q", res.ForAgent)
	}
	if strings.Contains(res.ForAgent, "199999") {
		t.Error("the agent got the full output")
	}
	data, err := os.ReadFile(res.File)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 200000 {
		t.Errorf("saved file has %d lines", lines)
	}
}

type fakeResultSummariser struct{ input string }

func (f *fakeResultSummariser) Summarise(_ context.Context, content []byte, _, _ string) (string, error) {
	f.input = string(content)
	return "SUMMARY-TEXT", nil
}

var _ tools.Summariser = (*fakeResultSummariser)(nil)

// A delegated agent has no API client; its summary runs on its own backend.
func TestRunUserCommand_OverLimitSummaryOnDelegatedAgent(t *testing.T) {
	t.Parallel()
	a := userShellAgent(t, 1000)
	a.AutoSummarise = true
	sum := &fakeResultSummariser{}
	a.ResultSummariser = sum
	res := a.RunUserCommand(context.Background(), "a/s", "seq 1 200000", nil)
	if !strings.Contains(res.ForAgent, "SUMMARY-TEXT") || !strings.Contains(res.ForAgent, res.File) {
		t.Errorf("ForAgent = %q", res.ForAgent)
	}
	if strings.Contains(sum.input, "199999") {
		t.Error("the summariser got more than the bounded start")
	}
}

func TestRunUserCommand_PersistMovesFile(t *testing.T) {
	t.Parallel()
	a := userShellAgent(t, 1000)
	dst := t.TempDir() + "/kept.txt"
	res := a.RunUserCommand(context.Background(), "a/s", "seq 1 2000", func(p string) (string, error) {
		return dst, os.Rename(p, dst)
	})
	if res.File != dst || !strings.Contains(res.ForAgent, dst) {
		t.Errorf("File = %q ForAgent = %q", res.File, res.ForAgent)
	}
}

func TestUserCommandRefusal(t *testing.T) {
	t.Parallel()
	a := newTestAgent(t)
	a.UserCommandOperators = func(p string) []string {
		if p == "telegram" {
			return []string{"A"}
		}
		return nil
	}
	for _, tc := range []struct {
		platform, user string
		group          bool
		want           string
	}{
		{"telegram", "A", false, ""},
		{"telegram", "B", false, usercmd.RefusedNotOperator},
		{"telegram", "", false, usercmd.RefusedNotOperator},
		{"telegram", "A", true, usercmd.RefusedGroup},
		{"discord", "B", false, ""},
		{"discord", "B", true, usercmd.RefusedGroup},
	} {
		if got := a.UserCommandRefusal(tc.platform, tc.user, tc.group); got != tc.want {
			t.Errorf("%+v: refusal = %q", tc, got)
		}
	}
}

func TestCheckUserCommand_NoRunnerRefuses(t *testing.T) {
	t.Parallel()
	if err := newTestAgent(t).CheckUserCommand("ls"); err == nil {
		t.Fatal("an agent with no runner accepted a command")
	}
}

// Mid-turn, the result joins the running turn like any queued message; and with
// an ask pending it does not answer the ask (Automated).
func TestEnqueueUserCommandResult_MidTurnNeverAnswersAnAsk(t *testing.T) {
	t.Parallel()
	a := userShellAgent(t, 1000)
	a.SetInboxSteerMode(true)
	var answered bool
	a.AskRouter = &tools.AskRouter{
		PendingForSession: func(string) string { return "ask-1" },
		IsPaused:          func(string) bool { return false },
		HandleResponse:    func(string, string) { answered = true },
	}
	be := &recordingBackend{}
	a.SetInboxBackend(func(_ context.Context, _ string) (delegator.Delegator, error) { return be, nil })
	a.getOrCreateInbox("test/s").turnActive.Store(true)

	res := usercmd.Result{Command: "date", State: usercmd.StateExited, ForAgent: "now\n"}
	if !a.EnqueueUserCommandResult(Envelope{SessionKey: "test/s", Steer: platform.SteerAlways}, res) {
		t.Fatal("refused")
	}
	if answered {
		t.Fatal("a command result answered a pending ask")
	}
	injects := be.Injects()
	if len(injects) != 1 || !strings.Contains(injects[0].Text, "$ date") || !strings.Contains(injects[0].Text, "now") {
		t.Fatalf("the result did not join the running turn: %+v", injects)
	}
}

// H2: a message that did not come from a person typing on a chat platform goes to
// the inbox as text, whatever it starts with: nothing here parses it.
func TestEnqueue_BangTextFromNonChatSourcesRunsNothing(t *testing.T) {
	t.Parallel()
	a := userShellAgent(t, 1000)
	marker := a.UserShell.WorkDir + "/h2-marker"
	var seen []string
	a.SetEnqueueObserver(func(e Envelope) { seen = append(seen, e.Text) })
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "!touch " + marker, Human: true})
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "!touch " + marker, Automated: true})
	if len(seen) != 2 || seen[0] != "!touch "+marker {
		t.Errorf("seen = %q", seen)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command ran")
	}
}

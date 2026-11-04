package slack

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/tools/shell"
	"foci/internal/usercmd"
)

// shellAgent is a real agent's "!" flow with its envelopes captured.
type shellAgent struct {
	*agent.Agent
	mu   sync.Mutex
	envs []agent.Envelope
}

func newShellAgent(t *testing.T, operators []string) *shellAgent {
	t.Helper()
	ag := &agent.Agent{AgentID: "ag", MaxResultChars: 1000}
	ag.UserShell = &shell.UserRunner{
		WorkDir:        t.TempDir(),
		SpillThreshold: func() int64 { return 1000 },
		SpillTempDir:   t.TempDir(),
		Timeout:        func() time.Duration { return 20 * time.Second },
	}
	ag.UserCommandOperators = func(string) []string { return operators }
	sa := &shellAgent{Agent: ag}
	ag.SetEnqueueObserver(func(e agent.Envelope) {
		sa.mu.Lock()
		defer sa.mu.Unlock()
		sa.envs = append(sa.envs, e)
	})
	return sa
}

func (s *shellAgent) envelopes() []agent.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Envelope(nil), s.envs...)
}

func slackShellHarness(t *testing.T, sa *shellAgent) *harness {
	t.Helper()
	h := newHarness(t, "ag", map[string]string{"C1": "ag"})
	h.mgr.core("ag").enq = sa
	return h
}

func TestShellCommand_Slack_DMRunsRepliesAndStartsTurn(t *testing.T) {
	sa := newShellAgent(t, nil)
	h := slackShellHarness(t, sa)
	h.deliver(dmEvent("U1", "D1", "!echo hello", "1.1"))
	deadline := time.Now().Add(20 * time.Second)
	for len(sa.envelopes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	envs := sa.envelopes()
	if len(envs) != 1 || !strings.Contains(envs[0].Text, "\nhello\n") || !strings.Contains(envs[0].Text, "exit status 0") {
		t.Fatalf("envelopes = %+v", envs)
	}
	posts := h.fake.postsTo("D1")
	if len(posts) != 1 || !strings.Contains(posts[0].Body["text"].(string), "hello") {
		t.Errorf("posts = %+v", posts)
	}
}

func TestShellCommand_Slack_Refusals(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "m")

	// A channel mention is a group chat, whoever sends it.
	sa := newShellAgent(t, nil)
	h := slackShellHarness(t, sa)
	h.deliver(mentionEvent("U1", "C1", "<@UBOTID> !touch "+marker, "2.2", ""))
	if posts := h.fake.postsTo("C1"); len(posts) != 1 || !strings.Contains(posts[0].Body["text"].(string), usercmd.RefusedGroup) {
		t.Errorf("posts = %+v", posts)
	}

	// With operators set, a non-operator is refused in a DM.
	sa2 := newShellAgent(t, []string{"Uother"})
	h2 := slackShellHarness(t, sa2)
	h2.deliver(dmEvent("U1", "D1", "!touch "+marker, "3.3"))
	if posts := h2.fake.postsTo("D1"); len(posts) != 1 || !strings.Contains(posts[0].Body["text"].(string), usercmd.RefusedNotOperator) {
		t.Errorf("posts = %+v", posts)
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a refused command ran")
	}
	if n := len(sa.envelopes()) + len(sa2.envelopes()); n != 0 {
		t.Errorf("%d envelope(s) reached the agent", n)
	}
}

func TestShellCommand_Slack_SpaceStaysNormalText(t *testing.T) {
	sa := newShellAgent(t, nil)
	h := slackShellHarness(t, sa)
	h.deliver(dmEvent("U1", "D1", "! echo hi", "4.4"))
	envs := sa.envelopes()
	if len(envs) != 1 || envs[0].Text != "! echo hi" {
		t.Errorf("envelopes = %+v", envs)
	}
	if posts := h.fake.postsTo("D1"); len(posts) != 0 {
		t.Errorf("a reply was posted: %+v", posts)
	}
}

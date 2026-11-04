package discord

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/tools/shell"
	"foci/internal/usercmd"

	"github.com/bwmarrin/discordgo"
)

type dcShellFixture struct {
	b    *Bot
	fs   *fakeSession
	mu   sync.Mutex
	envs []agent.Envelope
}

func newDCShellFixture(t *testing.T, operators []string) *dcShellFixture {
	t.Helper()
	b, fs, _ := newTestBot(t, "a")
	ag := &agent.Agent{AgentID: "a", MaxResultChars: 1000}
	ag.UserShell = &shell.UserRunner{
		WorkDir:        t.TempDir(),
		SpillThreshold: func() int64 { return 1000 },
		SpillTempDir:   t.TempDir(),
		Timeout:        func() time.Duration { return 20 * time.Second },
	}
	ag.UserCommandOperators = func(string) []string { return operators }
	f := &dcShellFixture{b: b, fs: fs}
	ag.SetEnqueueObserver(func(e agent.Envelope) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.envs = append(f.envs, e)
	})
	b.agentRef = ag
	return f
}

func (f *dcShellFixture) envelopes() []agent.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Envelope(nil), f.envs...)
}

func TestShellCommand_Discord_RunsRepliesAndStartsTurn(t *testing.T) {
	f := newDCShellFixture(t, nil)
	f.b.receiveMessage(context.Background(), testDiscordMessage("42", "u1", "!echo hello"))
	deadline := time.Now().Add(20 * time.Second)
	for len(f.envelopes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	envs := f.envelopes()
	if len(envs) != 1 || !strings.Contains(envs[0].Text, "\nhello\n") || !strings.Contains(envs[0].Text, "exit status 0") {
		t.Fatalf("envelopes = %+v", envs)
	}
	if got := f.fs.lastSend(t).content; !strings.Contains(got, "hello") {
		t.Errorf("reply = %q", got)
	}
}

func TestShellCommand_Discord_LargeOutputFitsMessageLimit(t *testing.T) {
	f := newDCShellFixture(t, nil)
	f.b.receiveMessage(context.Background(), testDiscordMessage("42", "u1", "!seq 1 200000"))
	deadline := time.Now().Add(20 * time.Second)
	for len(f.envelopes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.fs.sendCount() != 1 {
		t.Errorf("sends = %d", f.fs.sendCount())
	}
	got := f.fs.lastSend(t).content
	if len(got) > 2000 || !strings.Contains(got, "saved to") || strings.Contains(got, "199999") {
		t.Errorf("reply (%d) = %q", len(got), got)
	}
}

func TestShellCommand_Discord_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operators []string
		guild     string
		want      string
	}{
		{"guild channel", nil, "g1", usercmd.RefusedGroup},
		{"guild channel, operator", []string{"u1"}, "g1", usercmd.RefusedGroup},
		{"non-operator dm", []string{"other"}, "", usercmd.RefusedNotOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "m")
			f := newDCShellFixture(t, tc.operators)
			m := testDiscordMessage("42", "u1", "!touch "+marker)
			m.GuildID = tc.guild
			f.b.receiveMessage(context.Background(), m)
			if got := f.fs.lastSend(t).content; !strings.Contains(got, tc.want) {
				t.Errorf("reply = %q, want %q", got, tc.want)
			}
			time.Sleep(100 * time.Millisecond)
			if _, err := os.Stat(marker); err == nil {
				t.Error("a refused command ran")
			}
			if envs := f.envelopes(); len(envs) != 0 {
				t.Errorf("reached the agent: %+v", envs)
			}
		})
	}
}

func TestShellCommand_Discord_EscapesReachTheQueue(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "m")
	for _, text := range []string{"! touch " + marker, "!"} {
		f := newDCShellFixture(t, nil)
		f.b.receiveMessage(context.Background(), testDiscordMessage("42", "u1", text))
		select {
		case qm := <-f.b.mq.Chan():
			if qm.Text != text {
				t.Errorf("queued = %q, want %q", qm.Text, text)
			}
		default:
			t.Errorf("%q did not reach the queue", text)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command ran")
	}
}

// In a require-mention guild channel, an unaddressed "!" gets silence; addressed,
// it is refused with a reply. It runs nothing either way.
func TestShellCommand_Discord_GroupUnaddressedIsSilent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "m")
	f := newDCShellFixture(t, nil)
	f.b.botUserID = "bot1"
	f.b.mq.SetRequireMention(true)

	m := testDiscordMessage("42", "u1", "!touch "+marker)
	m.GuildID = "g1"
	f.b.receiveMessage(context.Background(), m)
	if n := f.fs.sendCount(); n != 0 {
		t.Errorf("an unaddressed message got %d replies", n)
	}

	m2 := testDiscordMessage("42", "u1", "!touch "+marker)
	m2.GuildID = "g1"
	m2.Mentions = []*discordgo.User{{ID: "bot1"}}
	f.b.receiveMessage(context.Background(), m2)
	if got := f.fs.lastSend(t).content; !strings.Contains(got, usercmd.RefusedGroup) {
		t.Errorf("addressed reply = %q", got)
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a group command ran")
	}
	if envs := f.envelopes(); len(envs) != 0 {
		t.Errorf("reached the agent: %+v", envs)
	}
}

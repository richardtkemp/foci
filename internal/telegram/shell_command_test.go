package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/secrets"
	"foci/internal/tools/shell"
	"foci/internal/usercmd"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

const tgShellSecret = "sk-tg-2812-very-secret"

type tgShellFixture struct {
	b    *Bot
	mock *mockClient
	mu   sync.Mutex
	envs []agent.Envelope
}

func newTGShellFixture(t *testing.T, allowed []string, operators []string, limit int) *tgShellFixture {
	t.Helper()
	b, mock := testBot(allowed, command.NewRegistry())
	path := filepath.Join(t.TempDir(), "secrets.toml")
	if err := os.WriteFile(path, []byte("[brave]\napi_key = \""+tgShellSecret+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ag := &agent.Agent{AgentID: "test", MaxResultChars: limit}
	ag.UserShell = &shell.UserRunner{
		Store:          store,
		WorkDir:        t.TempDir(),
		SpillThreshold: func() int64 { return int64(limit) },
		SpillTempDir:   t.TempDir(),
		Timeout:        func() time.Duration { return 20 * time.Second },
	}
	ag.UserCommandOperators = func(string) []string { return operators }
	f := &tgShellFixture{b: b, mock: mock}
	ag.SetEnqueueObserver(func(e agent.Envelope) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.envs = append(f.envs, e)
	})
	b.agentRef = ag
	return f
}

func (f *tgShellFixture) waitEnvelope(t *testing.T) agent.Envelope {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.envs)
		f.mu.Unlock()
		if n > 0 {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.envs[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no envelope reached the agent")
	return agent.Envelope{}
}

func (f *tgShellFixture) noEnvelopes(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.envs) != 0 {
		t.Errorf("reached the agent: %+v", f.envs)
	}
}

func (f *tgShellFixture) lastReply() (string, int) {
	f.mock.mu.Lock()
	defer f.mock.mu.Unlock()
	return f.mock.lastSendInjected, f.mock.sends
}

// H10: the user gets the output as a reply, and the agent's turn starts.
func TestShellCommand_Telegram_RunsRepliesAndStartsTurn(t *testing.T) {
	f := newTGShellFixture(t, []string{"111"}, nil, 1000)
	f.b.receiveMessage(context.Background(), makeMsg(111, "owner", "!echo hello"))
	env := f.waitEnvelope(t)
	if !strings.Contains(env.Text, "$ echo hello") || !strings.Contains(env.Text, "exit status 0") || !strings.Contains(env.Text, "\nhello\n") {
		t.Errorf("agent text = %q", env.Text)
	}
	if env.Driver == nil || env.SessionKey == "" || !env.Automated || env.Original == nil {
		t.Errorf("envelope = %+v", env)
	}
	reply, sends := f.lastReply()
	if sends != 1 || !strings.Contains(reply, "hello") {
		t.Errorf("reply (%d sends) = %q", sends, reply)
	}
	select {
	case qm := <-f.b.mq.Chan():
		t.Errorf("the typed command reached the queue: %+v", qm)
	default:
	}
}

// H10: over the limit the reply is the start and the path, and within Telegram's
// length limit.
func TestShellCommand_Telegram_LargeOutputReplyFits(t *testing.T) {
	f := newTGShellFixture(t, []string{"111"}, nil, 1000)
	f.b.receiveMessage(context.Background(), makeMsg(111, "owner", "!seq 1 200000"))
	env := f.waitEnvelope(t)
	if strings.Contains(env.Text, "199999") {
		t.Error("the agent got the full output")
	}
	reply, sends := f.lastReply()
	if sends != 1 {
		t.Errorf("sends = %d", sends)
	}
	if utf8.RuneCountInString(reply) > 4096 {
		t.Errorf("reply is %d chars", utf8.RuneCountInString(reply))
	}
	if !strings.Contains(reply, "\n1\n") || !strings.Contains(reply, "saved to") || strings.Contains(reply, "199999") {
		t.Errorf("reply = %q", reply)
	}
}

// H4: the access table, end to end through the receive path.
func TestShellCommand_Telegram_Access(t *testing.T) {
	dir := t.TempDir()
	group := func(m *gotgbot.Message) *gotgbot.Message { m.Chat.Type = "supergroup"; return m }
	for _, tc := range []struct {
		name      string
		operators []string
		user      int64
		msg       func(*gotgbot.Message) *gotgbot.Message
		wantRun   bool
		wantReply string
	}{
		{name: "non-operator refused", operators: []string{"111"}, user: 222, wantReply: usercmd.RefusedNotOperator},
		{name: "operator runs", operators: []string{"111"}, user: 111, wantRun: true},
		{name: "no operators: allowed user runs", user: 222, wantRun: true},
		{name: "group refused for an operator", operators: []string{"111"}, user: 111, msg: group, wantReply: usercmd.RefusedGroup},
		{name: "group refused with no operators", user: 111, msg: group, wantReply: usercmd.RefusedGroup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_"))
			f := newTGShellFixture(t, []string{"111", "222"}, tc.operators, 1000)
			m := makeMsg(tc.user, "u", "!touch "+marker)
			if tc.msg != nil {
				m = tc.msg(m)
			}
			f.b.receiveMessage(context.Background(), m)
			if tc.wantRun {
				f.waitEnvelope(t)
				if _, err := os.Stat(marker); err != nil {
					t.Errorf("the command did not run: %v", err)
				}
				return
			}
			reply, _ := f.lastReply()
			if !strings.Contains(reply, tc.wantReply) {
				t.Errorf("reply = %q, want %q", reply, tc.wantReply)
			}
			time.Sleep(100 * time.Millisecond)
			if _, err := os.Stat(marker); err == nil {
				t.Error("a refused command ran")
			}
			f.noEnvelopes(t)
		})
	}
}

// H2: these stay ordinary messages on the queue, unchanged.
func TestShellCommand_Telegram_EscapesReachTheQueue(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "m")
	for _, text := range []string{"! touch " + marker, "!", "/queue !touch " + marker} {
		f := newTGShellFixture(t, []string{"111"}, nil, 1000)
		f.b.receiveMessage(context.Background(), makeMsg(111, "u", text))
		select {
		case qm := <-f.b.mq.Chan():
			if !strings.HasSuffix(text, qm.Text) {
				t.Errorf("%q queued as %q", text, qm.Text)
			}
		default:
			t.Errorf("%q did not reach the queue", text)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%q ran a command", text)
		}
	}
}

func TestShellCommand_Telegram_SecretRefusedAndOutputScrubbed(t *testing.T) {
	f := newTGShellFixture(t, []string{"111"}, nil, 1000)
	f.b.receiveMessage(context.Background(), makeMsg(111, "u", "!echo {{secret:brave.api_key}}"))
	reply, _ := f.lastReply()
	if !strings.Contains(reply, "refused") {
		t.Errorf("reply = %q", reply)
	}
	f.noEnvelopes(t)

	f = newTGShellFixture(t, []string{"111"}, nil, 1000)
	f.b.receiveMessage(context.Background(), makeMsg(111, "u", fmt.Sprintf(`!printf '%%s' %q`, tgShellSecret)))
	env := f.waitEnvelope(t)
	reply, _ = f.lastReply()
	if strings.Contains(reply, tgShellSecret) || strings.Contains(env.Text, tgShellSecret) {
		t.Errorf("the secret survived: reply=%q agent=%q", reply, env.Text)
	}
	if !strings.Contains(reply, "[REDACTED]") {
		t.Errorf("reply = %q", reply)
	}
}

// A "!" replayed from Telegram's update queue after a restart must not execute,
// like a stale slash command.
func TestShellCommand_Telegram_StaleIsDropped(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "m")
	f := newTGShellFixture(t, []string{"111"}, nil, 1000)
	m := makeMsg(111, "u", "!touch "+marker)
	m.Date = time.Now().Add(-time.Hour).Unix()
	f.b.receiveMessage(context.Background(), m)
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a stale command ran")
	}
	f.noEnvelopes(t)
}

// In a require-mention group, an unaddressed "!" gets what any unaddressed message
// gets: silence. Addressed, it is refused with a reply. It runs nothing either way.
func TestShellCommand_Telegram_GroupUnaddressedIsSilent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "m")
	text := "!touch " + marker + " @mybot"
	build := func(mention bool) *gotgbot.Message {
		m := makeMsg(111, "u", text)
		m.Chat.Type = "group"
		if mention {
			m.Entities = []gotgbot.MessageEntity{{Type: "mention", Offset: int64(len(text) - len("@mybot")), Length: int64(len("@mybot"))}}
		}
		return m
	}
	fixture := func() *tgShellFixture {
		f := newTGShellFixture(t, []string{"111"}, nil, 1000)
		f.b.api = &gotgbot.Bot{User: gotgbot.User{Id: 99, Username: "mybot"}}
		f.b.mq.SetRequireMention(true)
		return f
	}

	f := fixture()
	f.b.receiveMessage(context.Background(), build(false))
	if _, sends := f.lastReply(); sends != 0 {
		t.Errorf("an unaddressed message got %d replies", sends)
	}

	f2 := fixture()
	f2.b.receiveMessage(context.Background(), build(true))
	if reply, _ := f2.lastReply(); !strings.Contains(reply, usercmd.RefusedGroup) {
		t.Errorf("addressed reply = %q", reply)
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a group command ran")
	}
	f.noEnvelopes(t)
	f2.noEnvelopes(t)
}

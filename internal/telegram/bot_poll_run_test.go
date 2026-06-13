package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/command"
	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// fakeBotClient implements gotgbot.BotClient for poll-loop tests. Queued
// getUpdates responses are popped in order; once exhausted, getUpdates
// returns an empty batch immediately (a short-poll), and all other methods
// return a generic success.
type fakeBotClient struct {
	mu             sync.Mutex
	queued         []json.RawMessage
	lastGetUpdates map[string]string // params of the most recent getUpdates call
	getUpdateCalls int
}

func (f *fakeBotClient) RequestWithContext(_ context.Context, _ string, method string, params map[string]string, _ map[string]gotgbot.FileReader, _ *gotgbot.RequestOpts) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method == "getUpdates" {
		f.getUpdateCalls++
		f.lastGetUpdates = params
		if len(f.queued) > 0 {
			r := f.queued[0]
			f.queued = f.queued[1:]
			return r, nil
		}
		return json.RawMessage("[]"), nil
	}
	return json.RawMessage("true"), nil
}

func (f *fakeBotClient) GetAPIURL(_ *gotgbot.RequestOpts) string { return "http://fake.local" }

func (f *fakeBotClient) FileURL(_ string, tgFilePath string, _ *gotgbot.RequestOpts) string {
	return "http://fake.local/file/" + tgFilePath
}

func (f *fakeBotClient) lastOffset() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastGetUpdates["offset"]
}

// wireFakeAPI attaches a fake gotgbot API (with identity) to a test bot and
// queues the given updates as the first getUpdates response.
func wireFakeAPI(b *Bot, updates ...gotgbot.Update) *fakeBotClient {
	fake := &fakeBotClient{}
	if len(updates) > 0 {
		raw, _ := json.Marshal(updates)
		fake.queued = append(fake.queued, raw)
	}
	b.api = &gotgbot.Bot{
		Token:     "tok",
		User:      gotgbot.User{Id: 99, Username: "focibot"},
		BotClient: fake,
	}
	return fake
}

func TestPollUpdates_DeliversMessageAndAcksOnShutdown(t *testing.T) {
	// Proves the poll loop fetches updates, routes an authorized user message
	// into the platform queue, advances the offset past the processed update,
	// and fires the final short-poll ack on shutdown.
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	fake := wireFakeAPI(b, gotgbot.Update{
		UpdateId: 41,
		Message: &gotgbot.Message{
			From: &gotgbot.User{Id: 111, Username: "owner"},
			Chat: gotgbot.Chat{Id: 12345},
			Text: "hello",
			Date: time.Now().Unix(),
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.pollUpdates(ctx) }()

	select {
	case qm := <-b.mq.Chan():
		if qm.Text != "hello" || qm.UserID != "111" {
			t.Errorf("queued message = %q from %q, want hello/111", qm.Text, qm.UserID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("message did not reach the platform queue within 2s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pollUpdates did not return after cancel")
	}

	// The shutdown ack is the last getUpdates and must carry offset 42
	// (update_id + 1) so Telegram does not replay the processed update.
	if got := fake.lastOffset(); got != "42" {
		t.Errorf("final ack offset = %q, want 42", got)
	}
}

func TestRun_ProcessesCallbackQueryEndToEnd(t *testing.T) {
	// Proves Run wires the full pipeline: registers commands with Telegram,
	// starts the poll loop, and routes a command-keyboard callback through to
	// command execution; the loop shuts down cleanly on ctx cancel.
	cmdRan := make(chan struct{}, 1)
	cmds := command.NewRegistry()
	cmds.Register(&command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			select {
			case cmdRan <- struct{}{}:
			default:
			}
			return command.Response{Text: "pong"}, nil
		},
	})
	b, mock := testBot([]string{"111"}, cmds)
	wireFakeAPI(b, gotgbot.Update{
		UpdateId: 7,
		CallbackQuery: &gotgbot.CallbackQuery{
			Id:      "cq1",
			Data:    "cmd:/ping",
			Message: gotgbot.Message{MessageId: 5, Chat: gotgbot.Chat{Id: 12345}},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.Run(ctx) }()

	select {
	case <-cmdRan:
	case <-time.After(2 * time.Second):
		t.Fatal("callback command did not run within 2s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// RegisterCommands ran against the bot's client.
	mock.mu.Lock()
	registered := len(mock.setCmds)
	mock.mu.Unlock()
	if registered != 1 {
		t.Errorf("registered commands = %d, want 1 (ping)", registered)
	}
}

func TestBotManagerStartAllAndWait(t *testing.T) {
	// Proves StartAll runs every registered bot's poll loop in a tracked
	// goroutine and Wait blocks until they have all shut down after cancel.
	mgr := NewBotManager()
	b1, _ := testBot([]string{"111"}, command.NewRegistry())
	wireFakeAPI(b1)
	mgr.AddPrimary("scout", b1)
	b2, _ := testBot([]string{"111"}, command.NewRegistry())
	wireFakeAPI(b2)
	b2.SetSessionKeyDirect("")
	mgr.AddFacet("scout", b2)

	ctx, cancel := context.WithCancel(context.Background())
	adapter := platform.NewConnectionManagerAdapter[*Bot](mgr)
	adapter.StartAll(ctx)
	cancel()

	waited := make(chan struct{})
	go func() { defer close(waited); adapter.Wait() }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

func TestBotStartAndStop(t *testing.T) {
	// Proves Start spawns the run loop without error and Stop is a no-op
	// (shutdown is ctx-driven).
	b, _ := testBot([]string{"111"}, command.NewRegistry())
	wireFakeAPI(b)
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if err := b.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestNewBot_WiresBotFromFactory(t *testing.T) {
	// Proves NewBot builds a fully-wired bot off the (stubbed) gotgbot
	// factory: API client shared for send/receive, allowed users mapped,
	// agent identity threaded into session key derivation.
	api := &gotgbot.Bot{User: gotgbot.User{Id: 99, Username: "focibot"}, BotClient: &fakeBotClient{}}
	withStubFactory(t, func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		if token != "tok" {
			t.Errorf("factory token = %q, want tok", token)
		}
		return api, nil
	})

	b, err := NewBot("tok", []string{"111", "222"}, nil, command.NewRegistry(), command.NewLastMessageStore(), "scout", "")
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	if b.api != api || b.client == nil {
		t.Error("api/client not wired from factory")
	}
	if !b.allowedUsers["111"] || !b.allowedUsers["222"] || len(b.allowedUsers) != 2 {
		t.Errorf("allowedUsers = %v", b.allowedUsers)
	}
	if b.agentID != "scout" || b.chatmeta.AgentID != "scout" {
		t.Error("agent identity not threaded through")
	}
	if b.Username() != "focibot" {
		t.Errorf("Username = %q, want focibot", b.Username())
	}
	if b.PlatformName() != "telegram" {
		t.Errorf("PlatformName = %q", b.PlatformName())
	}
}

func TestNewBot_PermanentErrorFailsFast(t *testing.T) {
	// Proves a permanent (auth) factory error aborts NewBot immediately with
	// the token redacted from the message.
	withStubFactory(t, func(token string, opts *gotgbot.BotOpts) (*gotgbot.Bot, error) {
		return nil, errors.New("Unauthorized: token tok rejected")
	})

	_, err := NewBot("tok", nil, nil, command.NewRegistry(), nil, "scout", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "tok") && !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("token not redacted: %v", err)
	}
}

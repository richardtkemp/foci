package testharness

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestTelegramStub_GetMe_ReturnsRegisteredUser verifies the most basic
// Bot API path: a bot fetches its own identity. Real gotgbot calls this
// at startup, so the test harness must serve it before foci-gw starts.
func TestTelegramStub_GetMe_ReturnsRegisteredUser(t *testing.T) {
	stub := NewTelegramStub()
	defer stub.Close()

	token := "12345:ABCDEF"
	stub.RegisterBot(token, gotgbot.User{
		Id: 99, IsBot: true, FirstName: "Stubby", Username: "stub_bot",
	})

	bot, err := gotgbot.NewBot(token, &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			DefaultRequestOpts: &gotgbot.RequestOpts{
				Timeout: 2 * time.Second,
				APIURL:  stub.URL(),
			},
		},
	})
	if err != nil {
		t.Fatalf("gotgbot.NewBot: %v", err)
	}

	if bot.Username != "stub_bot" {
		t.Errorf("bot.Username = %q, want %q", bot.Username, "stub_bot")
	}
	if bot.Id != 99 {
		t.Errorf("bot.Id = %d, want 99", bot.Id)
	}
}

// TestTelegramStub_GetUpdates_DrainsQueue verifies that a pushed update
// is delivered on the next long-poll and removed from the queue.
func TestTelegramStub_GetUpdates_DrainsQueue(t *testing.T) {
	stub := NewTelegramStub()
	defer stub.Close()

	token := "67890:GHIJK"
	stub.RegisterBot(token, gotgbot.User{Id: 1, IsBot: true, FirstName: "B"})

	stub.PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 42, Type: "private"},
			Text: "hello",
			From: &gotgbot.User{Id: 7, FirstName: "Alice"},
		},
	})

	bot, _ := gotgbot.NewBot(token, &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			DefaultRequestOpts: &gotgbot.RequestOpts{
				Timeout: 2 * time.Second,
				APIURL:  stub.URL(),
			},
		},
	})

	updates, err := bot.GetUpdates(&gotgbot.GetUpdatesOpts{
		RequestOpts: &gotgbot.RequestOpts{Timeout: 2 * time.Second},
	})
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Message.Text != "hello" {
		t.Errorf("text = %q, want hello", updates[0].Message.Text)
	}
	if updates[0].UpdateId == 0 {
		t.Errorf("UpdateId was not auto-assigned")
	}
}

// TestTelegramStub_SendMessage_RecordsCall verifies that an outbound
// sendMessage is recorded so tests can assert on what foci tried to send.
func TestTelegramStub_SendMessage_RecordsCall(t *testing.T) {
	stub := NewTelegramStub()
	defer stub.Close()

	token := "TT:UU"
	stub.RegisterBot(token, gotgbot.User{Id: 1, IsBot: true, FirstName: "B"})

	bot, _ := gotgbot.NewBot(token, &gotgbot.BotOpts{
		BotClient: &gotgbot.BaseBotClient{
			DefaultRequestOpts: &gotgbot.RequestOpts{
				Timeout: 2 * time.Second,
				APIURL:  stub.URL(),
			},
		},
	})
	_, err := bot.SendMessage(42, "hello world", nil)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	sent := stub.DrainSent(token)
	var sendMessages []SentCall
	for _, c := range sent {
		if c.Method == "sendMessage" {
			sendMessages = append(sendMessages, c)
		}
	}
	if len(sendMessages) != 1 {
		t.Fatalf("expected 1 sendMessage call, got %d", len(sendMessages))
	}
	var body map[string]any
	if err := json.Unmarshal(sendMessages[0].Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["text"] != "hello world" {
		t.Errorf("text in recorded body = %v, want hello world", body["text"])
	}
	// gotgbot encodes all params as strings before JSON marshalling
	// (params is map[string]string in its API), so chat_id arrives as "42".
	if body["chat_id"] != "42" {
		t.Errorf("chat_id in recorded body = %v (%T), want %q", body["chat_id"], body["chat_id"], "42")
	}
}

// TestTelegramStub_UnknownToken_Returns401 verifies the stub fails loudly
// for unregistered bots rather than serving a generic empty response (which
// would let misconfigured tests run silently against the wrong bot). It
// returns 401 Unauthorized, matching real Telegram's response to a bad
// token, so foci classifies it as a permanent auth error and fast-fails
// instead of retrying the token check forever.
func TestTelegramStub_UnknownToken_Returns401(t *testing.T) {
	stub := NewTelegramStub()
	defer stub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", stub.URL()+"/botunknown:token/getMe", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

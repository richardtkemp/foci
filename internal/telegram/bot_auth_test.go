package telegram

import (
	"context"
	"strings"
	"sync"
	"testing"

	"foci/internal/command"
	"foci/internal/log"
)

func TestReceiveMessage_RejectsUnauthorizedUser(t *testing.T) {
	// Verifies that unauthorized users
	// cannot send messages to the bot.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(999, "hacker", "hello")
	b.receiveMessage(context.Background(), msg)

	// Should not send any reply or queue anything
	if mock.sentCount() != 0 {
		t.Error("should not send reply to unauthorized user")
	}
	if len(b.mq.Chan()) != 0 {
		t.Error("should not queue message from unauthorized user")
	}
}

func TestReceiveMessage_AcceptsAuthorizedUser(t *testing.T) {
	// Verifies that authorized users can
	// send messages to the bot and they are properly queued.
	b, _ := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(111, "owner", "hello world")
	b.receiveMessage(context.Background(), msg)

	// Should be queued for the agent
	if len(b.mq.Chan()) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(b.mq.Chan()))
	}
	qm := <-b.mq.Chan()
	if qm.Text != "hello world" {
		t.Errorf("queued text = %q, want %q", qm.Text, "hello world")
	}
	if qm.UserID != "111" {
		t.Errorf("queued userID = %q, want %q", qm.UserID, "111")
	}
}

// TestReceiveMessage_LogsWarnOnRejection verifies that rejecting an
// unauthorized message emits a WARN log line including the user ID and
// username via formatUserInfo. The WARN surface is what operators see
// when strangers find a bot — this test guards its presence and shape.
func TestReceiveMessage_LogsWarnOnRejection(t *testing.T) {
	var (
		mu      sync.Mutex
		entries []struct {
			level     log.Level
			component string
			msg       string
		}
	)
	log.SetWarnHook(func(level log.Level, component, msg string) {
		mu.Lock()
		defer mu.Unlock()
		entries = append(entries, struct {
			level     log.Level
			component string
			msg       string
		}{level, component, msg})
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	b, _ := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(999, "hacker", "hello")
	b.receiveMessage(context.Background(), msg)

	mu.Lock()
	defer mu.Unlock()

	var found bool
	for _, e := range entries {
		if e.level != log.WARN {
			continue
		}
		if !strings.HasPrefix(e.component, "telegram:") {
			continue
		}
		if strings.Contains(e.msg, "rejected message from") &&
			strings.Contains(e.msg, "999") &&
			strings.Contains(e.msg, "hacker") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected WARN log with component telegram:* and rejection message containing user ID + username; got %+v", entries)
	}
}

func TestReceiveMessage_IgnoresEmptyText(t *testing.T) {
	// Verifies that empty or whitespace-only
	// messages are not queued.
	b, mock := testBot([]string{"111"}, command.NewRegistry())

	msg := makeMsg(111, "owner", "")
	b.receiveMessage(context.Background(), msg)

	if mock.sentCount() != 0 {
		t.Error("should not send reply to empty message")
	}
	if len(b.mq.Chan()) != 0 {
		t.Error("should not queue empty message")
	}
}

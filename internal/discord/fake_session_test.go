package discord

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/command"
	"foci/internal/platform"
	"foci/internal/session"

	"github.com/bwmarrin/discordgo"
)

// sentMsg records one ChannelMessageSend/SendComplex call.
type sentMsg struct {
	channelID  string
	content    string
	components []discordgo.MessageComponent
	fileNames  []string
	fileData   [][]byte
}

// editedMsg records one ChannelMessageEdit/EditComplex call.
type editedMsg struct {
	channelID  string
	msgID      string
	content    string
	components []discordgo.MessageComponent
}

// fakeSession implements messageSession for tests, recording all message I/O.
type fakeSession struct {
	mu sync.Mutex

	sends   []sentMsg
	edits   []editedMsg
	deletes []string // message IDs

	typingCalls         int
	interactionResponds int

	sendErr   error // returned by all send variants
	editErr   error
	deleteErr error

	nextID int // message ID counter
}

func (f *fakeSession) record(channelID, content string, components []discordgo.MessageComponent, files []*discordgo.File) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	m := sentMsg{channelID: channelID, content: content, components: components}
	for _, file := range files {
		m.fileNames = append(m.fileNames, file.Name)
		data, _ := io.ReadAll(file.Reader)
		m.fileData = append(m.fileData, data)
	}
	f.sends = append(f.sends, m)
	f.nextID++
	return &discordgo.Message{ID: fmt.Sprintf("%d", f.nextID), ChannelID: channelID}, nil
}

func (f *fakeSession) ChannelMessageSend(channelID string, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	return f.record(channelID, content, nil, nil)
}

func (f *fakeSession) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	return f.record(channelID, data.Content, data.Components, data.Files)
}

func (f *fakeSession) ChannelMessageEdit(channelID, messageID, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editErr != nil {
		return nil, f.editErr
	}
	f.edits = append(f.edits, editedMsg{channelID: channelID, msgID: messageID, content: content})
	return &discordgo.Message{ID: messageID, ChannelID: channelID}, nil
}

func (f *fakeSession) ChannelMessageEditComplex(m *discordgo.MessageEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editErr != nil {
		return nil, f.editErr
	}
	e := editedMsg{channelID: m.Channel, msgID: m.ID}
	if m.Content != nil {
		e.content = *m.Content
	}
	if m.Components != nil {
		e.components = *m.Components
	}
	f.edits = append(f.edits, e)
	return &discordgo.Message{ID: m.ID, ChannelID: m.Channel}, nil
}

func (f *fakeSession) ChannelMessageDelete(_, messageID string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes = append(f.deletes, messageID)
	return nil
}

func (f *fakeSession) ChannelTyping(string, ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typingCalls++
	return nil
}

func (f *fakeSession) InteractionRespond(*discordgo.Interaction, *discordgo.InteractionResponse, ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interactionResponds++
	return nil
}

// sendCount returns the number of recorded sends (thread-safe).
func (f *fakeSession) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

// lastSend returns the most recent send, failing the test if none happened.
func (f *fakeSession) lastSend(t *testing.T) sentMsg {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) == 0 {
		t.Fatal("expected at least one send")
	}
	return f.sends[len(f.sends)-1]
}

// lastEdit returns the most recent edit, failing the test if none happened.
func (f *fakeSession) lastEdit(t *testing.T) editedMsg {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) == 0 {
		t.Fatal("expected at least one edit")
	}
	return f.edits[len(f.edits)-1]
}

// unknownChannelErr builds the discordgo REST error for code 10003 "Unknown Channel".
func unknownChannelErr() error {
	return &discordgo.RESTError{
		Response:     &http.Response{StatusCode: 404},
		ResponseBody: []byte(`{"code": 10003}`),
		Message:      &discordgo.APIErrorMessage{Code: 10003, Message: "Unknown Channel"},
	}
}

// newTestBotMQ builds a small message queue for struct-literal test bots.
func newTestBotMQ() *platform.MessageQueue {
	return platform.NewMessageQueue(platform.MessageQueueConfig{Size: 64})
}

// testDiscordMessage builds a minimal inbound discordgo message for tests.
func testDiscordMessage(channelID, authorID, content string) *discordgo.Message {
	return &discordgo.Message{
		ChannelID: channelID,
		Content:   content,
		Author:    &discordgo.User{ID: authorID, Username: "user-" + authorID},
	}
}

// newTestBot builds a Bot wired to a fakeSession and a real session index in a
// temp dir. The bot is a primary bot for agentID with no allowed-user
// restrictions.
func newTestBot(t *testing.T, agentID string) (*Bot, *fakeSession, *session.SessionIndex) {
	t.Helper()
	r, idx := discordTestResolver(t, agentID)
	fs := &fakeSession{}
	b := &Bot{
		api:          fs,
		agentID:      agentID,
		sessionIndex: idx,
		chatmeta:     r,
		commands:     command.NewRegistry(),
		lastMsgStore: command.NewLastMessageStore(),
	}
	b.mq = newTestBotMQ()
	return b, fs, idx
}

// registerTestCommand registers a trivial command on the bot's registry that
// responds with the given text.
func registerTestCommand(b *Bot, name, reply string) {
	b.commands.Register(&command.Command{
		Name: name,
		Execute: func(context.Context, command.Request, command.CommandContext) (command.Response, error) {
			return command.Response{Text: reply}, nil
		},
	})
}

// commandTestContext builds a minimal CommandContext for dispatch tests.
func commandTestContext() command.CommandContext {
	return command.CommandContext{}
}

// agentEnvelope builds a minimal agent.Envelope carrying the given original
// platform message.
func agentEnvelope(original any, chatID int64) agent.Envelope {
	return agent.Envelope{
		SessionKey: fmt.Sprintf("a/c%d/123", chatID),
		ChatID:     chatID,
		Original:   original,
	}
}

// waitFor polls cond until it holds or the deadline expires (deterministic
// bounded wait for cross-goroutine effects).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

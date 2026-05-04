package telegram

import (
	"fmt"
	"sync"
	"time"

	"foci/internal/chatmeta"
	"foci/internal/command"
	"foci/internal/dispatch"
	"foci/internal/log"
	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// mockClient implements botClient for testing.
type mockClient struct {
	mu             sync.Mutex
	sends          int                  // counts SendMessage calls
	edits          int                  // counts EditMessageText calls
	deletes        int                  // counts DeleteMessage calls
	files          map[string]string    // fileId → filePath for GetFile mock
	setCmds        []gotgbot.BotCommand // last SetMyCommands call
	setCmdsErr     error                // error to return from SetMyCommands
	lastSendOpts   *gotgbot.SendMessageOpts  // last SendMessage opts
	lastSendInjected   string                    // last SendMessage text
	lastEditOpts   *gotgbot.EditMessageTextOpts // last EditMessageText opts
	lastEditText   string                    // last EditMessageText text
	answerCBCalls  int                       // counts AnswerCallbackQuery calls
	editErr        error                     // error to return from EditMessageText
	editErrOnce    bool                      // if true, only return editErr on first call
}

func (m *mockClient) SendMessage(chatId int64, text string, opts *gotgbot.SendMessageOpts) (*gotgbot.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends++
	m.lastSendInjected = text
	m.lastSendOpts = opts
	return &gotgbot.Message{MessageId: int64(m.sends)}, nil
}

func (m *mockClient) EditMessageText(text string, opts *gotgbot.EditMessageTextOpts) (*gotgbot.Message, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edits++
	m.lastEditText = text
	m.lastEditOpts = opts
	if m.editErr != nil {
		err := m.editErr
		if m.editErrOnce {
			m.editErr = nil
		}
		return nil, false, err
	}
	return &gotgbot.Message{}, true, nil
}

func (m *mockClient) SendDocument(chatId int64, document gotgbot.InputFileOrString, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendVoice(chatId int64, voice gotgbot.InputFileOrString, opts *gotgbot.SendVoiceOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendVideo(chatId int64, video gotgbot.InputFileOrString, opts *gotgbot.SendVideoOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendPhoto(chatId int64, photo gotgbot.InputFileOrString, opts *gotgbot.SendPhotoOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendAudio(chatId int64, audio gotgbot.InputFileOrString, opts *gotgbot.SendAudioOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendAnimation(chatId int64, animation gotgbot.InputFileOrString, opts *gotgbot.SendAnimationOpts) (*gotgbot.Message, error) {
	return &gotgbot.Message{}, nil
}

func (m *mockClient) SendChatAction(chatId int64, action string, opts *gotgbot.SendChatActionOpts) (bool, error) {
	return true, nil
}

func (m *mockClient) GetFile(fileId string, opts *gotgbot.GetFileOpts) (*gotgbot.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.files == nil {
		return nil, fmt.Errorf("file not found: %s", fileId)
	}
	fp, ok := m.files[fileId]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", fileId)
	}
	return &gotgbot.File{FileId: fileId, FilePath: fp}, nil
}

func (m *mockClient) SetMyCommands(commands []gotgbot.BotCommand, opts *gotgbot.SetMyCommandsOpts) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setCmds = commands
	if m.setCmdsErr != nil {
		return false, m.setCmdsErr
	}
	return true, nil
}

func (m *mockClient) AnswerCallbackQuery(callbackQueryId string, opts *gotgbot.AnswerCallbackQueryOpts) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answerCBCalls++
	return true, nil
}

func (m *mockClient) DeleteMessage(chatId int64, messageId int64, opts *gotgbot.DeleteMessageOpts) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	return true, nil
}

func (m *mockClient) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sends
}

func (m *mockClient) editCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.edits
}

func (m *mockClient) deleteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deletes
}

// testBot creates a Bot for testing with a mock client.
func testBot(allowedUsers []string, cmds *command.Registry) (*Bot, *mockClient) {
	mock := &mockClient{}
	allowed := make(map[string]bool)
	for _, u := range allowedUsers {
		allowed[u] = true
	}
	lg := log.NewComponentLogger("telegram:test")
	b := &Bot{
		log:          lg,
		client:       mock,
		commands:     cmds,
		lastMsgStore: command.NewLastMessageStore(),
		allowedUsers: allowed,
		sessionKey:   "agent:test:main",
		chatmeta: &chatmeta.Resolver{
			PlatformName: platformName,
			Logger:       func() *log.ComponentLogger { return defaultLogger },
		},
	}
	b.mq = platform.NewMessageQueue(platform.MessageQueueConfig{
		Size:       64,

		Logger:     lg,
	})
	b.dispatcher = dispatch.NewDispatcher(cmds, command.CommandContext{}, "test")
	return b, mock
}

func makeMsg(userID int64, username, text string) *gotgbot.Message {
	return &gotgbot.Message{
		From: &gotgbot.User{Id: userID, Username: username},
		Chat: gotgbot.Chat{Id: 12345},
		Text: text,
		Date: int64(time.Now().Unix()),
	}
}

// makeMsgWithPhoto creates a test message with a photo attachment.
func makeMsgWithPhoto(userID int64, username, caption string) *gotgbot.Message {
	return &gotgbot.Message{
		From:    &gotgbot.User{Id: userID, Username: username},
		Chat:    gotgbot.Chat{Id: 12345},
		Caption: caption,
		Date:    int64(time.Now().Unix()),
		Photo: []gotgbot.PhotoSize{
			{FileId: "small_id", Width: 90, Height: 90, FileSize: 1000},
			{FileId: "large_id", Width: 800, Height: 600, FileSize: 50000},
		},
	}
}

// makeMsgWithDocument creates a test message with a document attachment.
func makeMsgWithDocument(userID int64, username, mime string) *gotgbot.Message {
	return &gotgbot.Message{
		From: &gotgbot.User{Id: userID, Username: username},
		Chat: gotgbot.Chat{Id: 12345},
		Date: int64(time.Now().Unix()),
		Document: &gotgbot.Document{
			FileId:   "doc_id",
			MimeType: mime,
		},
	}
}

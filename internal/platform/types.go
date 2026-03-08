package platform

import (
	"context"
	"time"
)

type Message struct {
	ID        string
	Text      string
	SenderID  string
	ChatID    string
	Timestamp time.Time
	Media     []Attachment
	ReplyTo   *string
}

type Attachment struct {
	Type      string
	Data      []byte
	MimeType  string
	SavedPath string
}

type SendOptions struct {
	ParseMode string
	ReplyTo   string
}

type Sender interface {
	SendText(chatID, text string) error
	SendDocument(chatID, path string) error
	SendVoice(chatID, path string) error
	SendVideo(chatID, path string) error
	SendPhoto(chatID, path string) error
	SendAudio(chatID, path string) error
	SendAnimation(chatID, path string) error
	SendVoiceData(audioData []byte) error

	SendTextToChat(chatID int64, text string) error
	SendDocumentToChat(chatID int64, path string) error
	SendVoiceToChat(chatID int64, path string) error
	SendVideoToChat(chatID int64, path string) error
	SendPhotoToChat(chatID int64, path string) error
	SendAudioToChat(chatID int64, path string) error
	SendAnimationToChat(chatID int64, path string) error
	SendVoiceDataToChat(chatID int64, audioData []byte) error

	SessionKey() string
}

type Platform interface {
	Sender
	Receive(ctx context.Context) (<-chan Message, error)
	SessionKeyForChat(chatID string) string
	Start(ctx context.Context) error
	Stop() error
}

type TurnCallbacks struct {
	ReplyFunc          func(text string)
	ActivityFunc       func()
	ToolCallObserver   func(toolName string, params []byte)
	ToolResultObserver func(toolName string, result string, isError bool)
	ThinkingObserver   func(thinking string)
	TextDeltaObserver  func(delta string)
	SteerCheckFunc     func() string
	RetryNotifyFunc    func(retryAfter int)
	RetrySuccessFunc   func()
}

type MessageHandler interface {
	HandleMessage(ctx context.Context, sessionKey, userID, text string, attachments []Attachment, callbacks TurnCallbacks) (string, error)
	IsProcessing() bool
	TransformMessage(text string) string
}

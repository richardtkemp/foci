package platform

import (
	"context"
	"time"

	"foci/internal/warnings"
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
	SessionKey() string

	SendText(text string) error
	SendDocument(filePath string) error
	SendVoice(filePath string) error
	SendVideo(filePath string) error
	SendPhoto(filePath string) error
	SendAudio(filePath string) error
	SendAnimation(filePath string) error
	SendVoiceData(audioData []byte) error

	SendTextToChat(chatID int64, text string) error
	SendDocumentToChat(chatID int64, filePath string) error
	SendVoiceToChat(chatID int64, filePath string) error
	SendVideoToChat(chatID int64, filePath string) error
	SendPhotoToChat(chatID int64, filePath string) error
	SendAudioToChat(chatID int64, filePath string) error
	SendAnimationToChat(chatID int64, filePath string) error
	SendVoiceDataToChat(chatID int64, audioData []byte) error
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
	HandleMessage(ctx context.Context, sessionKey, text string) (string, error)
	HandleMessageWithAttachments(ctx context.Context, sessionKey, text string, attachments []Attachment) (string, error)
	IsProcessing() bool
	TransformMessage(text string) string
	Warnings() *warnings.Queue
}

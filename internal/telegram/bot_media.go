package telegram

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/log"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

func (b *Bot) SendDocument(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendDocumentToChat(chatID, filePath)
}

// SendVoice sends a voice note from a file to the last known chat.
func (b *Bot) SendVoice(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendVoiceToChat(chatID, filePath)
}

// SendVideo sends a video file to the last known chat.
func (b *Bot) SendVideo(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendVideoToChat(chatID, filePath)
}

// SendPhoto sends a photo to the last known chat.
func (b *Bot) SendPhoto(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendPhotoToChat(chatID, filePath)
}

// SendAudio sends an audio file to the last known chat.
func (b *Bot) SendAudio(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendAudioToChat(chatID, filePath)
}

// SendAnimation sends an animation (GIF) to the last known chat.
func (b *Bot) SendAnimation(filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendAnimationToChat(chatID, filePath)
}

// SendVoiceData sends audio bytes as a Telegram voice note to the last known chat.
func (b *Bot) SendVoiceData(audioData []byte) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return b.SendVoiceDataToChat(chatID, audioData)
}

// SendVoiceDataToChat sends audio bytes as a Telegram voice note to a specific chat.
func (b *Bot) SendVoiceDataToChat(chatID int64, audioData []byte) error {
	_, err := b.client.SendVoice(chatID, gotgbot.InputFileByReader("voice.mp3", bytes.NewReader(audioData)), nil)
	return err
}

// lastChatID returns the last known chat ID, or an error if none has been set.
func (b *Bot) lastChatID() (int64, error) {
	b.chatMu.Lock()
	chatID := b.chatID
	b.chatMu.Unlock()
	if chatID == 0 {
		return 0, fmt.Errorf("no chat ID — no messages received yet")
	}
	return chatID, nil
}

// SendTextToChat sends a text message to a specific chat ID without any header.
func (b *Bot) SendTextToChat(chatID int64, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	b.sendHTMLChunks(chatID, ConvertToTelegramHTML(text, b.tableOpts()), "", "")
	return nil
}

// SendInjectedToChat sends an injected/system text message to a specific chat ID.
// Prepends the configured InjectedMessageHeader (if non-empty).
func (b *Bot) SendInjectedToChat(chatID int64, text string) error {
	if b.injectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.injectedMessageHeader + "\n" + text
	}
	return b.SendTextToChat(chatID, text)
}

// sendMediaFile is a generic helper for sending media files to Telegram.
func (b *Bot) sendMediaFile(chatID int64, filePath, mediaType string, sendFn func(int64, gotgbot.InputFile, *gotgbot.SendDocumentOpts) (*gotgbot.Message, error)) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open %s file: %w", mediaType, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := sendFn(chatID, gotgbot.InputFileByReader(filepath.Base(filePath), f), nil); err != nil {
		return fmt.Errorf("send %s: %w", mediaType, err)
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		ChatID:    chatID,
		Text:      fmt.Sprintf("[%s %s]", mediaType, filePath),
		Session:   b.SessionKey(),
	})
	return nil
}

// SendDocumentToChat sends a file as a Telegram document to a specific chat ID.
func (b *Bot) SendDocumentToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath, "document", func(cid int64, file gotgbot.InputFile, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error) {
		return b.client.SendDocument(cid, file, opts)
	})
}

// SendVoiceToChat sends a voice note from a file to a specific chat ID.
func (b *Bot) SendVoiceToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath, "voice", func(cid int64, file gotgbot.InputFile, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error) {
		return b.client.SendVoice(cid, file, nil)
	})
}

// SendVideoToChat sends a video file to a specific chat ID.
func (b *Bot) SendVideoToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath, "video", func(cid int64, file gotgbot.InputFile, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error) {
		return b.client.SendVideo(cid, file, nil)
	})
}

// SendPhotoToChat sends a photo to a specific chat ID.
func (b *Bot) SendPhotoToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath, "photo", func(cid int64, file gotgbot.InputFile, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error) {
		return b.client.SendPhoto(cid, file, nil)
	})
}

// SendAudioToChat sends an audio file to a specific chat ID.
func (b *Bot) SendAudioToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath, "audio", func(cid int64, file gotgbot.InputFile, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error) {
		return b.client.SendAudio(cid, file, nil)
	})
}

// SendAnimationToChat sends an animation (GIF) to a specific chat ID.
func (b *Bot) SendAnimationToChat(chatID int64, filePath string) error {
	return b.sendMediaFile(chatID, filePath, "animation", func(cid int64, file gotgbot.InputFile, opts *gotgbot.SendDocumentOpts) (*gotgbot.Message, error) {
		return b.client.SendAnimation(cid, file, nil)
	})
}

// sendVoiceNote sends audio data as a Telegram voice note.
func (b *Bot) sendVoiceNote(chatID int64, userID string, username string, audioData []byte) {
	if _, err := b.client.SendVoice(chatID, gotgbot.InputFileByReader("voice.mp3", bytes.NewReader(audioData)), nil); err != nil {
		b.logger().Errorf("send voice note: %s", b.sanitizeError(err))
	}

	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		UserID:    userID,
		Username:  username,
		ChatID:    chatID,
		Text:      fmt.Sprintf("[voice note %d bytes]", len(audioData)),
		Session:   b.SessionKey(),
	})
}

// downloadFile downloads a file from Telegram by file ID.

func (b *Bot) downloadFile(fileID string) ([]byte, error) {
	file, err := b.client.GetFile(fileID, nil)
	if err != nil {
		return nil, fmt.Errorf("get file info: %w", err)
	}

	dlURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.botToken, file.FilePath)
	client := &http.Client{Timeout: 30 * time.Second}

	const maxAttempts = 3
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}

		resp, err := client.Get(dlURL)
		if err != nil {
			lastErr = fmt.Errorf("download file: %w", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("download file: status %d", resp.StatusCode)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return nil, lastErr
			}
			continue
		}

		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read file body: %w", err)
			continue
		}

		return data, nil
	}

	return nil, lastErr
}

// extForMediaType returns a file extension for the given media type.
func extForMediaType(mt string) string {
	switch mt {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	default:
		return ".bin"
	}
}

// extForVideo returns a file extension for video MIME types.
func extForVideo(mt string) string {
	switch mt {
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	case "video/webm":
		return ".webm"
	case "video/x-matroska":
		return ".mkv"
	case "video/avi", "video/x-msvideo":
		return ".avi"
	default:
		return ".mp4"
	}
}

// extForMIME returns a file extension for common MIME types.
func extForMIME(mt string) string {
	switch {
	case strings.HasPrefix(mt, "video/"):
		return extForVideo(mt)
	case mt == "application/pdf":
		return ".pdf"
	case mt == "application/json":
		return ".json"
	case mt == "text/plain":
		return ".txt"
	case mt == "text/csv":
		return ".csv"
	case mt == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case mt == "application/vnd.ms-excel":
		return ".xls"
	case mt == "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case mt == "application/msword":
		return ".doc"
	case strings.HasPrefix(mt, "audio/"):
		return ".mp3"
	default:
		return ".bin"
	}
}

// fileTooLargeError indicates a file exceeds the download size limit.
type fileTooLargeError struct {
	size int64
}

func (e *fileTooLargeError) Error() string {
	return fmt.Sprintf("file too large: %d bytes (limit 20MB)", e.size)
}

// isFileTooLarge returns true if the error is a file size limit error.
func isFileTooLarge(err error) bool {
	_, ok := err.(*fileTooLargeError)
	return ok
}

// downloadAndSaveMedia downloads a file from Telegram and saves it to disk.
// Returns the saved file path or an error (including fileTooLargeError if over 20MB).
func (b *Bot) downloadAndSaveMedia(fileID string, fileSize int64, mediaType string, chatID int64, ext string) (string, error) {
	const maxFileSize = 20 * 1024 * 1024 // 20MB Telegram Bot API limit

	if fileSize > maxFileSize {
		return "", &fileTooLargeError{size: fileSize}
	}

	if b.receivedFilesDir == "" {
		return "", fmt.Errorf("media save directory not configured")
	}

	data, err := b.downloadFile(fileID)
	if err != nil {
		return "", err
	}

	return b.saveMedia(data, mediaType, chatID, ext)
}

// saveMedia writes media data to disk and returns the saved file path.
func (b *Bot) saveMedia(data []byte, mediaType string, chatID int64, ext string) (string, error) {
	if err := os.MkdirAll(b.receivedFilesDir, 0o755); err != nil {
		return "", fmt.Errorf("create media dir: %w", err)
	}
	filename := fmt.Sprintf("%s_%s_chat-%d%s", time.Now().UTC().Format("2006-01-02T15-04-05Z"), mediaType, chatID, ext)
	path := filepath.Join(b.receivedFilesDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write media: %w", err)
	}
	return path, nil
}

// saveAttachment writes image data to disk and returns the saved file path.
func (b *Bot) saveAttachment(data []byte, mediaType string, chatID int64) (string, error) {
	if err := os.MkdirAll(b.receivedFilesDir, 0o755); err != nil {
		return "", fmt.Errorf("create image dir: %w", err)
	}
	ext := extForMediaType(mediaType)
	filename := fmt.Sprintf("%s_chat-%d%s", time.Now().UTC().Format("2006-01-02T15-04-05Z"), chatID, ext)
	path := filepath.Join(b.receivedFilesDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write image: %w", err)
	}
	return path, nil
}

// downloadAttachment downloads a file and returns it as an attachment,
// optionally saving to disk. Returns (attachment, true) on success.
func (b *Bot) downloadAttachment(fileID, mimeType string, chatID int64) (attachment, bool) {
	data, err := b.downloadFile(fileID)
	if err != nil {
		b.logger().Errorf("download image: %s", b.sanitizeError(err))
		if b.handler == nil || b.handler.Warnings() == nil {
			b.sendHTMLChunks(chatID, "Could not download image — please try again.", "", "")
		}
		return attachment{}, false
	}
	att := attachment{data: data, mediaType: mimeType}
	if b.receivedFilesDir != "" {
		if path, err := b.saveAttachment(data, mimeType, chatID); err != nil {
			b.logger().Warnf("save image: %v", err)
		} else {
			att.savedPath = path
			b.logger().Infof("saved image to %s", path)
		}
	}
	return att, true
}

// handleMediaMessage downloads and saves a media file (video, video note,
// document), prepending a status annotation to text. On success it prepends
// "[Label saved to: path]"; on file-too-large it prepends a size warning.
func (b *Bot) handleMediaMessage(text, fileID string, fileSize int64, mediaType, label string, chatID int64, ext string) string {
	path, err := b.downloadAndSaveMedia(fileID, fileSize, mediaType, chatID, ext)
	if err != nil {
		if isFileTooLarge(err) {
			return fmt.Sprintf("[%s too large to download (%d MB)]\n\n%s", label, fileSize/(1024*1024), text)
		}
		b.logger().Errorf("download %s: %s", mediaType, b.sanitizeError(err))
		if b.handler == nil || b.handler.Warnings() == nil {
			b.sendHTMLChunks(chatID, fmt.Sprintf("Could not download %s — please try again.", label), "", "")
		}
		return text
	}
	b.logger().Infof("saved %s to %s", mediaType, path)
	return fmt.Sprintf("[%s saved to: %s]\n\n%s", label, path, text)
}

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

	"foci/internal/timeutil"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// sendToLastChat resolves the last known chat ID and calls fn with it
// (caption-less variant — used by SendVoice).
func (b *Bot) sendToLastChat(fn func(int64, string) error, filePath string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return fn(chatID, filePath)
}

// sendCaptionedToLastChat is the captioned-file variant of sendToLastChat.
func (b *Bot) sendCaptionedToLastChat(fn func(int64, string, string) error, filePath, caption string) error {
	chatID, err := b.lastChatID()
	if err != nil {
		return err
	}
	return fn(chatID, filePath, caption)
}

func (b *Bot) SendDocument(filePath, caption string) error {
	return b.sendCaptionedToLastChat(b.SendDocumentToChat, filePath, caption)
}
func (b *Bot) SendVoice(filePath string) error { return b.sendToLastChat(b.SendVoiceToChat, filePath) }
func (b *Bot) SendVideo(filePath, caption string) error {
	return b.sendCaptionedToLastChat(b.SendVideoToChat, filePath, caption)
}
func (b *Bot) SendPhoto(filePath, caption string) error {
	return b.sendCaptionedToLastChat(b.SendPhotoToChat, filePath, caption)
}
func (b *Bot) SendAudio(filePath, caption string) error {
	return b.sendCaptionedToLastChat(b.SendAudioToChat, filePath, caption)
}
func (b *Bot) SendAnimation(filePath, caption string) error {
	return b.sendCaptionedToLastChat(b.SendAnimationToChat, filePath, caption)
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
// This is the single convergence point for all text delivery — every other
// send method (SendText, SendToSession, sendReply, etc.) delegates here.
// Sentinel filtering (IsSilent) is handled upstream by the renderer
// (OnReply/Finalize) for interactive turns and by SessionSink for
// injected/notify flows; this only guards against sending empty/whitespace
// to the platform API.
func (b *Bot) SendTextToChat(chatID int64, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	b.sendHTMLChunks(chatID, ConvertToTelegramHTML(text, b.tableOpts()))
	return nil
}

// SendInjectedToChat sends an injected/system text message to a specific chat ID.
// Prepends the configured InjectedMessageHeader (if non-empty).
func (b *Bot) SendInjectedToChat(chatID int64, text string) error {
	if b.display.InjectedMessageHeader != "" && strings.TrimSpace(text) != "" {
		text = b.display.InjectedMessageHeader + "\n" + text
	}
	return b.SendTextToChat(chatID, text)
}

// openMediaFile opens a file and returns it as a gotgbot InputFile.
// Caller is responsible for closing the underlying file.
func openMediaFile(filePath, mediaType string) (gotgbot.InputFile, *os.File, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s file: %w", mediaType, err)
	}
	return gotgbot.InputFileByReader(filepath.Base(filePath), f), f, nil
}

// SendDocumentToChat sends a file as a Telegram document to a specific chat ID.
// If caption is non-empty, it's sent inline as the document caption.
func (b *Bot) SendDocumentToChat(chatID int64, filePath, caption string) error {
	in, f, err := openMediaFile(filePath, "document")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opts := &gotgbot.SendDocumentOpts{Caption: caption}
	if _, err := b.client.SendDocument(chatID, in, opts); err != nil {
		return fmt.Errorf("send document: %w", err)
	}
	return nil
}

// SendVoiceToChat sends a voice note from a file to a specific chat ID.
func (b *Bot) SendVoiceToChat(chatID int64, filePath string) error {
	in, f, err := openMediaFile(filePath, "voice")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := b.client.SendVoice(chatID, in, nil); err != nil {
		return fmt.Errorf("send voice: %w", err)
	}
	return nil
}

// SendVideoToChat sends a video file to a specific chat ID.
func (b *Bot) SendVideoToChat(chatID int64, filePath, caption string) error {
	in, f, err := openMediaFile(filePath, "video")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opts := &gotgbot.SendVideoOpts{Caption: caption}
	if _, err := b.client.SendVideo(chatID, in, opts); err != nil {
		return fmt.Errorf("send video: %w", err)
	}
	return nil
}

// SendPhotoToChat sends a photo to a specific chat ID.
func (b *Bot) SendPhotoToChat(chatID int64, filePath, caption string) error {
	in, f, err := openMediaFile(filePath, "photo")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opts := &gotgbot.SendPhotoOpts{Caption: caption}
	if _, err := b.client.SendPhoto(chatID, in, opts); err != nil {
		return fmt.Errorf("send photo: %w", err)
	}
	return nil
}

// SendAudioToChat sends an audio file to a specific chat ID.
func (b *Bot) SendAudioToChat(chatID int64, filePath, caption string) error {
	in, f, err := openMediaFile(filePath, "audio")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opts := &gotgbot.SendAudioOpts{Caption: caption}
	if _, err := b.client.SendAudio(chatID, in, opts); err != nil {
		return fmt.Errorf("send audio: %w", err)
	}
	return nil
}

// SendAnimationToChat sends an animation (GIF) to a specific chat ID.
func (b *Bot) SendAnimationToChat(chatID int64, filePath, caption string) error {
	in, f, err := openMediaFile(filePath, "animation")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opts := &gotgbot.SendAnimationOpts{Caption: caption}
	if _, err := b.client.SendAnimation(chatID, in, opts); err != nil {
		return fmt.Errorf("send animation: %w", err)
	}
	return nil
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

	if b.display.ReceivedFilesDir == "" {
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
	if err := os.MkdirAll(b.display.ReceivedFilesDir, 0o755); err != nil {
		return "", fmt.Errorf("create media dir: %w", err)
	}
	filename := fmt.Sprintf("%s_%s_chat-%d%s", timeutil.FormatFilename(timeutil.Now()), mediaType, chatID, ext)
	path := filepath.Join(b.display.ReceivedFilesDir, filename)
	mode := b.fileMode
	if mode == 0 {
		mode = 0640
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return "", fmt.Errorf("write media: %w", err)
	}
	return path, nil
}

// downloadAttachment downloads a file and returns it as an attachment,
// optionally saving to disk. Returns (attachment, true) on success.
func (b *Bot) downloadAttachment(fileID, mimeType string, chatID int64) (attachment, bool) {
	data, err := b.downloadFile(fileID)
	if err != nil {
		b.logger().Errorf("download attachment: %s", b.sanitizeError(err))
		if b.handler == nil || b.handler.Warnings() == nil {
			b.sendHTMLChunks(chatID, "Could not download attachment — please try again.")
		}
		return attachment{}, false
	}
	att := attachment{data: data, mediaType: mimeType}
	if b.display.ReceivedFilesDir != "" {
		ext := extForMediaType(mimeType)
		if ext == ".bin" {
			ext = extForMIME(mimeType)
		}
		if path, err := b.saveMedia(data, "attachment", chatID, ext); err != nil {
			b.logger().Warnf("save attachment: %v", err)
		} else {
			att.savedPath = path
			b.logger().Infof("saved attachment to %s", path)
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
			b.sendHTMLChunks(chatID, fmt.Sprintf("Could not download %s — please try again.", label))
		}
		return text
	}
	b.logger().Infof("saved %s to %s", mediaType, path)
	return fmt.Sprintf("[%s saved to: %s]\n\n%s", label, path, text)
}

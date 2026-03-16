package discord

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"foci/internal/platform"

	"github.com/bwmarrin/discordgo"
)

// downloadAttachment downloads a file from Discord's CDN and returns it as an attachment.
// Returns (attachment, true) on success, or (zero, false) on failure.
func (b *Bot) downloadAttachment(att *discordgo.MessageAttachment) (attachment, bool) {
	url := att.ProxyURL
	if url == "" {
		url = att.URL
	}

	data, err := downloadURL(url)
	if err != nil {
		b.logger().Errorf("download attachment: %v", err)
		return attachment{}, false
	}

	mimeType := att.ContentType
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	normalizedMIME := platform.NormalizeMIME(mimeType)

	result := attachment{data: data, mediaType: normalizedMIME}

	// Save to disk if configured
	if b.display.ReceivedFilesDir != "" {
		ext := extForMediaType(normalizedMIME)
		if att.Filename != "" {
			ext = filepath.Ext(att.Filename)
			if ext == "" {
				ext = extForMediaType(normalizedMIME)
			}
		}
		if path, err := b.saveMedia(data, "attachment", ext); err != nil {
			b.logger().Warnf("save attachment: %v", err)
		} else {
			result.savedPath = path
			b.logger().Infof("saved attachment to %s", path)
		}
	}

	return result, true
}

// downloadURL performs an HTTP GET on a URL and returns the response body.
func downloadURL(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	const maxAttempts = 3
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}

		resp, err := client.Get(url)
		if err != nil {
			lastErr = fmt.Errorf("download: %w", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("download: status %d", resp.StatusCode)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return nil, lastErr
			}
			continue
		}

		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read body: %w", err)
			continue
		}

		return data, nil
	}

	return nil, lastErr
}

// saveMedia writes media data to disk and returns the saved file path.
func (b *Bot) saveMedia(data []byte, mediaType string, ext string) (string, error) {
	if err := os.MkdirAll(b.display.ReceivedFilesDir, 0o755); err != nil {
		return "", fmt.Errorf("create media dir: %w", err)
	}
	filename := fmt.Sprintf("%s_%s%s", time.Now().UTC().Format("2006-01-02T15-04-05Z"), mediaType, ext)
	path := filepath.Join(b.display.ReceivedFilesDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write media: %w", err)
	}
	return path, nil
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

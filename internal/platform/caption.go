package platform

// Caption length limits for the platforms that support file captions.
// Telegram caps a media caption at 1024 characters; Discord caps message
// content (which carries the caption) at 2000.
const (
	TelegramCaptionLimit = 1024
	DiscordCaptionLimit  = 2000
)

// SplitCaption decides how a caption should be attached to a media send, given
// the platform's caption length limit.
//
// If the caption fits within the limit it is returned as head with no overflow.
// If it exceeds the limit it is detached wholesale: head is empty and the entire
// caption is returned as overflow, to be sent as a follow-up text message after
// the file. Detaching the whole caption — rather than filling it to the limit
// and overflowing only the remainder — avoids splitting a markdown span or code
// fence across the caption/message boundary, which would corrupt the rendering.
//
// length is measured in bytes, matching the existing message chunkers'
// convention. For multibyte text this is conservative (it detaches slightly
// earlier than a strict rune/UTF-16 count would), which is safe.
func SplitCaption(caption string, limit int) (head, overflow string) {
	if len(caption) <= limit {
		return caption, ""
	}
	return "", caption
}

package telegram

import "regexp"

var (
	botTokenRe = regexp.MustCompile(`^\d{5,}:[A-Za-z0-9_-]{20,}$`)
	userIDRe   = regexp.MustCompile(`^\d{3,}$`)
)

// IsValidBotToken checks if a string looks like a Telegram bot token.
func IsValidBotToken(token string) bool {
	return botTokenRe.MatchString(token)
}

// IsValidUserID checks if a string is a numeric Telegram user ID.
func IsValidUserID(id string) bool {
	return userIDRe.MatchString(id)
}

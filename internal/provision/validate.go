package provision

import "regexp"

var (
	agentIDRe  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	botTokenRe = regexp.MustCompile(`^\d{5,}:[A-Za-z0-9_-]{20,}$`)
	userIDRe   = regexp.MustCompile(`^\d{3,}$`)
)

// IsValidAgentID checks if a string is a valid agent identifier (lowercase slug).
func IsValidAgentID(id string) bool {
	return agentIDRe.MatchString(id)
}

// IsValidBotToken checks if a string looks like a Telegram bot token.
func IsValidBotToken(token string) bool {
	return botTokenRe.MatchString(token)
}

// IsValidUserID checks if a string is a numeric Telegram user ID.
func IsValidUserID(id string) bool {
	return userIDRe.MatchString(id)
}

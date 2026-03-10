package telegram

import "foci/internal/platform"

func init() {
	platform.RegisterMessagingProvider("telegram", &telegramProvider{})
}

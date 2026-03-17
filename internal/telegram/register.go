package telegram

import (
	"foci/internal/agent"
	"foci/internal/platform"
)

func init() {
	platform.RegisterMessagingProvider("telegram", &telegramProvider{})
	agent.RegisterPlatformTrigger("telegram")
}

package discord

import (
	"foci/internal/agent"
	"foci/internal/platform"
)

func init() {
	platform.RegisterMessagingProvider("discord", &discordProvider{})
	agent.RegisterPlatformTrigger("discord")
}

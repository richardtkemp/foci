package discord

import "foci/internal/platform"

func init() {
	platform.RegisterMessagingProvider("discord", &discordProvider{})
}

package discord

import (
	"context"
	"runtime/debug"

	"github.com/bwmarrin/discordgo"
)

// Start spawns the bot's Run loop as a goroutine.
// Non-blocking; use Stop() to wait for shutdown.
func (b *Bot) Start(ctx context.Context) error {
	go b.Run(ctx)
	return nil
}

// Stop is a no-op for discord.Bot (shutdown is ctx-cancellation based).
func (b *Bot) Stop() error {
	return nil
}

// Run starts the gateway connection, registers message handlers, and blocks until ctx is cancelled.
// If the gateway connection fails, it recovers and logs the error.
func (b *Bot) Run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			b.logger().Errorf("panic in gateway: %v\n%s", r, debug.Stack())
		}
	}()

	// Set the bot's own user ID once connected
	if b.session.State != nil && b.session.State.User != nil {
		b.botUserID = b.session.State.User.ID
		b.logger().Infof("bot started as %s (%s)", b.session.State.User.Username, b.botUserID)
	} else {
		b.logger().Infof("bot started (user ID will be set on first event)")
	}

	// Register message handler
	removeHandler := b.session.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		b.onMessageCreate(ctx, m)
	})
	defer removeHandler()

	// Register interaction handler for button presses
	removeInteraction := b.session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type == discordgo.InteractionMessageComponent {
			b.handleComponentInteraction(ctx, i)
		}
	})
	defer removeInteraction()

	// Agent message pump — drains the platform queue and hands each message
	// to the agent's per-session inbox, where per-session workers handle
	// batching, in-flight tracking, and turn execution via Bot.Drive.
	// Commands continue to flow on this goroutine (priority drain).
	go b.agentMessagePump(ctx)

	// Block until context is cancelled
	<-ctx.Done()
	b.logger().Infof("bot shutting down")
}

// onMessageCreate handles incoming message events from the Discord gateway.
func (b *Bot) onMessageCreate(ctx context.Context, m *discordgo.MessageCreate) {
	// Ignore messages from the bot itself
	if m.Author == nil || m.Author.ID == b.botUserID {
		return
	}

	// Update bot user ID if not yet set
	if b.botUserID == "" && b.session.State != nil && b.session.State.User != nil {
		b.botUserID = b.session.State.User.ID
	}

	// Guild restriction check
	if b.guildID != "" && m.GuildID != "" && m.GuildID != b.guildID {
		return
	}

	b.receiveMessage(ctx, m.Message)
}

// messageContainsMention returns true if the message mentions the bot.
func (b *Bot) messageContainsMention(m *discordgo.Message) bool {
	for _, mention := range m.Mentions {
		if mention.ID == b.botUserID {
			return true
		}
	}
	return false
}

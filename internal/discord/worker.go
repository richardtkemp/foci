package discord

import (
	"context"
	"strconv"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/bwmarrin/discordgo"
)

// agentMessagePump drains the message queue's main channel and hands each
// message to the agent's per-session inbox. Discord prioritises commands
// over agent turns: pending commands are drained before each main-channel
// dequeue, preventing long turns from starving slash commands. The agent's
// per-session inbox handles batching, in-flight tracking, orphan-steer
// drain, and turn execution via Bot.Drive.
//
// One pump goroutine per bot fans out to per-session workers in the agent's
// Inbox — sessions on the same bot run their turns in parallel.
//
// Falls back to inline drop if no agent reference is set (test mode).
func (b *Bot) agentMessagePump(ctx context.Context) {
	for {
		// Priority: drain any pending commands before pulling the next
		// agent message. Commands run on the same goroutine as the pump
		// (matching pre-Phase-6 discord behaviour); turns proceed via
		// the agent's per-session workers, not on this goroutine.
		select {
		case cmd := <-b.mq.CmdChan():
			b.logger().Debugf("pump: dequeued command %q", cmd.Text)
			b.processQueuedCommand(ctx, cmd)
			continue
		default:
		}

		select {
		case <-ctx.Done():
			return
		case cmd := <-b.mq.CmdChan():
			b.logger().Debugf("pump: dequeued command %q", cmd.Text)
			b.processQueuedCommand(ctx, cmd)
		case qm := <-b.mq.Chan():
			b.handoffToAgent(qm)
		}
	}
}

// handoffToAgent converts a platform.QueuedMessage to an agent.Envelope
// and pushes it through the agent's per-session inbox. Each envelope
// carries this Bot as its Driver so the agent's worker can call back
// into Bot.Drive for renderer/tracker/sink construction.
func (b *Bot) handoffToAgent(qm platform.QueuedMessage) {
	if b.agentRef == nil {
		b.logger().Debugf("handoffToAgent: no agent ref, dropping message")
		return
	}
	sk := b.sessionKeyForQueuedMessage(qm)
	if sk == "" {
		b.logger().Debugf("handoffToAgent: no session key for channelID=%d", qm.ChatID)
		return
	}
	env := agent.Envelope{
		SessionKey:  sk,
		Text:        qm.Text,
		Attachments: qm.Attachments,
		UserID:      qm.UserID,
		SenderName:  qm.SenderName,
		ChatID:      qm.ChatID,
		IsGroupChat: qm.IsGroupChat,
		ReceivedAt:  qm.ReceivedAt,
		Original:    qm.Original,
		Driver:      b,
	}
	b.agentRef.Enqueue(env)
}

// sessionKeyForQueuedMessage resolves the session key using the same rules
// as processAgentMessage used to: secondary bots use their override,
// primary bots derive from channel ID, fallback to bot's default.
func (b *Bot) sessionKeyForQueuedMessage(qm platform.QueuedMessage) string {
	if b.isSecondary {
		return b.SessionKey()
	}
	if b.agentID != "" {
		return b.sessionKeyForMsg(qm.ChatID)
	}
	return b.SessionKey()
}

// processQueuedCommand dispatches a command that was routed via the command
// channel. See telegram/bot_worker.go for the rationale.
func (b *Bot) processQueuedCommand(ctx context.Context, qm platform.QueuedMessage) {
	origMsg, _ := qm.Original.(*discordgo.Message)
	if origMsg == nil {
		b.logger().Warnf("processQueuedCommand: missing original message")
		return
	}
	if b.dispatcher == nil {
		return
	}
	outcome := b.dispatcher.DispatchCommand(ctx, qm.Text, qm.ChatID, qm.UserID)
	if !outcome.NotHandled {
		b.renderCommandOutcome(origMsg, &outcome)
	}
}

// NewTurnSink implements agent.Driver. Builds the per-turn rendering glue
// (renderer + tracker + StreamingSink) from env. Returns (nil, nil) if
// env.Original isn't a *discordgo.Message. Part of TODO #746 Stage A.
func (b *Bot) NewTurnSink(env agent.Envelope) (turnevent.Sink, func()) {
	origMsg, _ := env.Original.(*discordgo.Message)
	if origMsg == nil {
		return nil, nil
	}
	channelIDStr := strconv.FormatInt(env.ChatID, 10)
	d := b.resolveDisplay(env.SessionKey)
	tracker := newToolCallTracker(b, channelIDStr, d)
	renderer := newTurnRenderer(b, origMsg, tracker, d)
	sink := turn.NewStreamingSink(renderer, tracker, b)
	return sink, renderer.Cleanup
}

// Connection implements agent.Driver. Returns the bot itself.
func (b *Bot) Connection() platform.Connection {
	return b
}

// WrapTurn implements agent.Driver. Discord-side lifecycle envelope
// around each agent turn — turnActive flag, notification drain,
// OnTurnEnd / OnTurnComplete hooks, error sanitisation. See the
// telegram Bot.WrapTurn for the equivalent on Telegram. TODO #746
// Stage C.
func (b *Bot) WrapTurn(fn func() error) error {
	b.turnActive.Store(true)
	defer func() {
		b.turnActive.Store(false)
		b.drainPendingNotifications()
		if b.OnTurnEnd != nil {
			b.OnTurnEnd()
		}
	}()

	err := fn()
	if err != nil {
		b.logger().Errorf("agent error: %s", b.sanitizeError(err))
	}

	if b.OnTurnComplete != nil {
		b.OnTurnComplete()
	}
	return err
}

// cancelTurn cancels the in-flight agent turn for this bot's primary
// session. Retained for /done compatibility — /stop now uses
// Agent.CancelSession(sk) directly. See TODO #746 Stage B.
func (b *Bot) cancelTurn() {
	if b.agentRef == nil {
		return
	}
	sk := b.SessionKey()
	if sk == "" {
		return
	}
	b.logger().Infof("cancelTurn → Agent.CancelSession sk=%s", sk)
	b.agentRef.CancelSession(sk)
}

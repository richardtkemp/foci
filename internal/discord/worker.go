package discord

import (
	"context"
	"fmt"
	"strconv"

	"foci/internal/agent"
	"foci/internal/agent/turnevent"
	"foci/internal/platform"
	"foci/internal/turn"

	"github.com/bwmarrin/discordgo"
)

// agentWorker processes queued messages, batching any that arrive while busy.
func (b *Bot) agentWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case qm := <-b.mq.Chan():
			// Batch with any other immediately-available messages.
			batch := append([]platform.QueuedMessage{qm}, b.mq.DrainQueue()...)
			b.processAgentMessage(ctx, batch)

			// After the turn: drain orphan steers + any newly queued messages.
			// Loop because new steers/messages can arrive during processing.
			for {
				orphans := b.mq.DrainSteer()
				extras := b.mq.DrainQueue()
				if len(orphans) == 0 && len(extras) == 0 {
					break
				}
				var followUp []platform.QueuedMessage
				for _, s := range orphans {
					followUp = append(followUp, platform.QueuedMessage{
						Original: qm.Original,
						UserID:   qm.UserID,
						Text:     s,
						ChatID:   qm.ChatID,
					})
				}
				followUp = append(followUp, extras...)
				b.logger().Infof("steer: processing %d orphan(s) + %d queued as follow-up turn", len(orphans), len(extras))
				b.processAgentMessage(ctx, followUp)
			}
		}
	}
}

// processAgentMessage handles a batched agent turn with a cancellable context.
// Session key, typing indicator, and renderer are derived from batch[0].
func (b *Bot) processAgentMessage(ctx context.Context, batch []platform.QueuedMessage) {
	first := batch[0]
	origMsg, _ := first.Original.(*discordgo.Message)
	if origMsg == nil {
		b.logger().Warnf("processAgentMessage: missing original message")
		return
	}

	channelID := first.ChatID
	channelIDStr := strconv.FormatInt(channelID, 10)

	var sk string
	if b.isSecondary {
		sk = b.SessionKey()
	} else if b.agentID != "" {
		sk = b.sessionKeyForMsg(channelID)
	} else {
		sk = b.SessionKey()
	}
	if sk == "" {
		return // no session assigned (idle secondary bot)
	}

	// Create a cancellable context for this turn
	turnCtx, cancel := context.WithCancel(ctx)

	b.turnMu.Lock()
	b.turnCancel = cancel
	b.turnMu.Unlock()

	defer func() {
		b.turnMu.Lock()
		b.turnCancel = nil
		b.turnMu.Unlock()
		cancel()
		b.drainPendingNotifications()
		if b.OnTurnEnd != nil {
			b.OnTurnEnd()
		}
	}()

	d := b.resolveDisplay(sk)
	tracker := newToolCallTracker(b, channelIDStr, d)
	renderer := newTurnRenderer(b, origMsg, tracker, d)
	defer renderer.Cleanup()

	sink := turn.NewStreamingSink(renderer, tracker, b)

	turnCtx = agent.WithTrigger(turnCtx, "discord")
	turnCtx = agent.WithTurnMetadata(turnCtx, &agent.TurnMetadata{
		UserID:   first.UserID,
		Username: origMsg.Author.Username,
		ChatID:   channelID,
	})

	// Collect texts and attachments across the batch.
	// Group chat messages get sender attribution.
	var texts []string
	var allAttachments []platform.Attachment
	for _, qm := range batch {
		text := qm.Text
		if qm.IsGroupChat && qm.SenderName != "" {
			text = fmt.Sprintf("[%s] %s", qm.SenderName, text)
		} else if qm.IsGroupChat && qm.UserID != "" {
			text = fmt.Sprintf("[user:%s] %s", qm.UserID, text)
		}
		texts = append(texts, text)
		allAttachments = append(allAttachments, qm.Attachments...)
	}

	err := turn.RunTurn(turnCtx, b.handler, sink, turnevent.SteererFunc(b.mq.DrainSteer), sk, texts, allAttachments)
	if err != nil && turnCtx.Err() != nil {
		b.logger().Infof("agent turn cancelled")
		return
	}
	if err != nil {
		b.logger().Errorf("agent error: %s", b.sanitizeError(err))
	}

	if b.OnTurnComplete != nil {
		b.OnTurnComplete()
	}
}

// cancelTurn cancels the in-flight agent turn, if any.
func (b *Bot) cancelTurn() {
	b.turnMu.Lock()
	defer b.turnMu.Unlock()
	if b.turnCancel != nil {
		b.logger().Infof("cancelling agent turn via /stop")
		b.turnCancel()
	}
}

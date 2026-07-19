package telegram

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"foci/internal/platform"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// subagentGroup tracks every Telegram message emitted by a single subagent run
// (one Task/Agent tool invocation, identified by its parent tool_use id). It
// backs the rolling "Hide this" button: the button always sits on the newest
// message, and clicking it deletes the whole set and suppresses any further
// messages from that subagent.
type subagentGroup struct {
	mu          sync.Mutex
	chatID      int64
	msgIDs      []int64   // every message sent for this subagent, in order
	buttonMsgID int64     // message currently carrying the Hide button (0 = none)
	suppressed  bool      // set once Hide is clicked → drop further messages
	lastActive  time.Time // for TTL reaping
}

// subagentGroupTTL bounds how long an un-hidden group's state is retained.
// Comfortably under Telegram's ~48h deleteMessage window; a click after expiry
// simply no-ops. Reaping is opportunistic (on each delivery), not timer-driven.
const subagentGroupTTL = time.Hour

// subagentToken derives the short callback token (and subagentStore key) for a
// subagent run from its chat and parent tool_use id. Hashing keeps callback_data
// within Telegram's 64-byte limit and avoids leaking internal ids into the chat.
func subagentToken(chatID int64, parentToolUseID string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", chatID, parentToolUseID)))
	return hex.EncodeToString(sum[:8]) // 16 hex chars
}

// DeliverSubagentText implements turn.SubagentDeliverer. It sends the subagent
// progress message carrying a "Hide this" button, then strips the button from
// the group's previous message so exactly one (the newest) shows it. Send-then-
// strip ordering guarantees a button is always visible (no zero-button window).
// The per-group mutex serialises fast bursts so the button can't land on a stale
// message.
func (b *telegramBackend) DeliverSubagentText(groupKey, text string) {
	b.bot.reapSubagentGroups()
	token := subagentToken(b.chatID, groupKey)
	gAny, _ := b.bot.subagentStore.LoadOrStore(token, &subagentGroup{chatID: b.chatID})
	g := gAny.(*subagentGroup)

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.suppressed {
		return // user hid this subagent — drop the rest
	}

	// text is already blockquoted by the renderer (SubagentTextRaw()==false), so
	// ConvertToTelegramHTML renders it as a <blockquote>.
	html := ConvertToTelegramHTML(text, b.opts)
	rows := buildButtonRows([]platform.ButtonChoice{{Label: "🙈 Hide this", Data: token}}, "sa:")
	markup := gotgbot.InlineKeyboardMarkup{InlineKeyboard: rows}
	sent, err := b.bot.client.SendMessage(b.chatID, html, &gotgbot.SendMessageOpts{
		ParseMode:   "HTML",
		ReplyMarkup: markup,
	})
	if err != nil {
		// Fallback to plain text (mirrors sendHTMLChunkIDs) but keep the button
		// so the control survives a formatting error.
		sent, err = b.bot.client.SendMessage(b.chatID, html, &gotgbot.SendMessageOpts{ReplyMarkup: markup})
		if err != nil {
			b.bot.logger().Errorf("subagent send: %s", b.bot.sanitizeError(err))
			return
		}
	}
	b.bot.refreshTyping()

	prev := g.buttonMsgID
	g.msgIDs = append(g.msgIDs, sent.MessageId)
	g.buttonMsgID = sent.MessageId
	g.lastActive = time.Now()
	if prev != 0 {
		b.bot.stripSubagentButton(b.chatID, prev)
	}
}

// DeliverSubagentStart / DeliverSubagentEnd are no-ops on Telegram: each subagent
// message already carries the agent-name header (composed by the renderer) and the
// rolling Hide button sits on the newest one, so there's no separate collapsed
// entry to open or close. SubagentTextRaw is false — Telegram wants the renderer's
// blockquoted-with-header presentation.
func (b *telegramBackend) DeliverSubagentStart(string, string, int, string) {}
func (b *telegramBackend) DeliverSubagentEnd(string, int)                   {}
func (b *telegramBackend) SubagentTextRaw() bool               { return false }

// stripSubagentButton removes the inline keyboard from a prior subagent message
// so only the newest message carries the rolling Hide button.
func (b *Bot) stripSubagentButton(chatID, msgID int64) {
	_, _, err := b.client.EditMessageReplyMarkup(&gotgbot.EditMessageReplyMarkupOpts{
		ChatId:      chatID,
		MessageId:   msgID,
		ReplyMarkup: gotgbot.InlineKeyboardMarkup{},
	})
	if err != nil {
		b.logger().Debugf("subagent strip button %d: %v", msgID, err)
	}
}

// handleSubagentHideCallback deletes every message from the subagent identified
// by token and marks the group suppressed so any further messages are dropped.
// Deletes are best-effort (a message may already be gone or past Telegram's 48h
// delete window).
func (b *Bot) handleSubagentHideCallback(chatID int64, token string) {
	gAny, ok := b.subagentStore.Load(token)
	if !ok {
		return
	}
	g := gAny.(*subagentGroup)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.suppressed = true
	for _, id := range g.msgIDs {
		if _, err := b.client.DeleteMessage(chatID, id, nil); err != nil {
			b.logger().Debugf("subagent hide delete %d: %v", id, err)
		}
	}
	g.msgIDs = nil
	g.buttonMsgID = 0
}

// reapSubagentGroups opportunistically drops group state older than the TTL to
// bound memory. Subagent tool_use ids never recur, so a reaped group can't be
// resurrected mid-run; a Hide click after reaping just no-ops.
func (b *Bot) reapSubagentGroups() {
	cutoff := time.Now().Add(-subagentGroupTTL)
	b.subagentStore.Range(func(k, v any) bool {
		g := v.(*subagentGroup)
		g.mu.Lock()
		stale := !g.lastActive.IsZero() && g.lastActive.Before(cutoff)
		g.mu.Unlock()
		if stale {
			b.subagentStore.Delete(k)
		}
		return true
	})
}

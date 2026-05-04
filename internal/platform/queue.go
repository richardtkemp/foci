package platform

import (
	"fmt"
	"time"

	"foci/internal/log"
)

// QueuedMessage is a platform-neutral queued message.
type QueuedMessage struct {
	UserID      string       // platform-specific user ID
	SenderName  string       // display name for group attribution
	Text        string       // extracted/processed message text
	Attachments []Attachment // downloaded attachments
	ChatID      int64        // for throttle keying + session routing
	IsGroupChat bool         // true = group/guild channel, false = DM
	IsMention   bool         // true if @mentions the bot
	Original    any          // *gotgbot.Message or *discordgo.Message
	// ReceivedAt is stamped at the platform receipt boundary (toPlatformMessage)
	// so that meta header timestamps reflect when the user actually sent the
	// message, not when it was later drained into a turn.
	ReceivedAt time.Time
}

// MessageQueue manages inbound message buffering and group-chat
// throttling. Thread-safe. Used by both Telegram and Discord bots.
//
// Concerns owned here are platform-side (filter + throttle + bounded
// channels). Agent-side concerns (steer buffering, in-flight tracking,
// urgent-dispatch decisions) live in agent.Inbox — see internal/agent/inbox.go.
// The bot's pump goroutine drains Chan() and hands messages to
// agent.Enqueue, which decides routing per-session.
type MessageQueue struct {
	ch             chan QueuedMessage
	cmdCh          chan QueuedMessage // commands bypass message routing rules
	throttle       *GroupThrottle     // nil = disabled
	requireMention bool
	log            *log.ComponentLogger
}

// MessageQueueConfig holds construction parameters for NewMessageQueue.
type MessageQueueConfig struct {
	Size           int
	CmdSize        int // command channel buffer size; 0 uses default (8)
	RequireMention bool
	Throttle       *GroupThrottle
	Logger         *log.ComponentLogger
}

// NewMessageQueue creates a new message queue.
func NewMessageQueue(cfg MessageQueueConfig) *MessageQueue {
	size := cfg.Size
	if size <= 0 {
		size = 64
	}
	cmdSize := cfg.CmdSize
	if cmdSize <= 0 {
		cmdSize = 8
	}
	mq := &MessageQueue{
		ch:             make(chan QueuedMessage, size),
		cmdCh:          make(chan QueuedMessage, cmdSize),
		throttle:       cfg.Throttle,
		requireMention: cfg.RequireMention,
		log:            cfg.Logger,
	}
	return mq
}

// Enqueue applies inbound filtering and routes the message to the main
// channel or the throttle. Routing rules:
//
//  1. Group chat + requireMention + not a mention + no throttle → drop silently.
//  2. Group chat + throttle active → throttle.Add (mention flushes
//     immediately, non-mention buffers until the throttle window closes).
//  3. Otherwise → push to main channel.
//
// Steer / urgent-dispatch / in-flight gating moved to agent.Inbox in
// Phase 6 (TODO #739) — those decisions need agent-level state that
// the platform shouldn't reach into.
func (q *MessageQueue) Enqueue(msg QueuedMessage) {
	// Rule 1: group + require_mention + not a mention + no throttle → drop.
	if msg.IsGroupChat && q.requireMention && !msg.IsMention && q.throttle == nil {
		if q.log != nil {
			q.log.Debugf("queue: dropping non-mention group message from %s", q.senderLabel(msg))
		}
		return
	}

	// Rule 2: group + throttle → buffer/flush via throttle.
	if msg.IsGroupChat && q.throttle != nil {
		q.throttle.Add(msg)
		return
	}

	// Rule 3: push to main channel.
	q.pushToChannel(msg)
}

// pushToChannel sends a message to the main queue channel, dropping with a
// warning if the channel is full.
func (q *MessageQueue) pushToChannel(msg QueuedMessage) {
	select {
	case q.ch <- msg:
	default:
		if q.log != nil {
			q.log.Warnf("message queue full, dropping message from %s", q.senderLabel(msg))
		}
	}
}

// Chan returns the receive-only channel for the bot's pump goroutine.
func (q *MessageQueue) Chan() <-chan QueuedMessage {
	return q.ch
}

// DrainQueue non-blocking drains all immediately available messages from
// the queue. Retained for parity with the per-bot pump pattern, even
// though batching now happens at the agent.Inbox level.
func (q *MessageQueue) DrainQueue() []QueuedMessage {
	var msgs []QueuedMessage
	for {
		select {
		case qm := <-q.ch:
			msgs = append(msgs, qm)
		default:
			return msgs
		}
	}
}

// SetThrottle sets the group throttle. Must be called before messages arrive.
func (q *MessageQueue) SetThrottle(t *GroupThrottle) {
	q.throttle = t
}

// SetRequireMention sets the require_mention flag. Must be called before messages arrive.
func (q *MessageQueue) SetRequireMention(v bool) {
	q.requireMention = v
}

// PushFlushed pushes a message from a throttle flush directly to the channel.
// Used as the throttle's flush callback target.
func (q *MessageQueue) PushFlushed(msg QueuedMessage) {
	q.pushToChannel(msg)
}

// EnqueueCommand sends a command message to the command channel, bypassing
// all message routing rules (group throttle, require-mention).
// Commands must always reach the worker so they are never silently dropped.
func (q *MessageQueue) EnqueueCommand(msg QueuedMessage) {
	select {
	case q.cmdCh <- msg:
	default:
		if q.log != nil {
			q.log.Warnf("command queue full, dropping command from %s", q.senderLabel(msg))
		}
	}
}

// CmdChan returns the receive-only command channel for the worker select loop.
func (q *MessageQueue) CmdChan() <-chan QueuedMessage {
	return q.cmdCh
}

// Stop stops throttle timers and releases resources.
func (q *MessageQueue) Stop() {
	if q.throttle != nil {
		q.throttle.Stop()
	}
}

// senderLabel returns a display label for log messages.
func (q *MessageQueue) senderLabel(msg QueuedMessage) string {
	if msg.SenderName != "" {
		return fmt.Sprintf("%s (%s)", msg.UserID, msg.SenderName)
	}
	return msg.UserID
}

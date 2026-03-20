package platform

import (
	"fmt"
	"sync"

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
}

// MessageQueue manages inbound message buffering, steer injection, and
// group chat throttling. Thread-safe. Used by both Telegram and Discord bots.
type MessageQueue struct {
	ch             chan QueuedMessage
	steerMu        sync.Mutex
	steerParts     []string
	throttle       *GroupThrottle // nil = disabled
	requireMention bool
	steerMode      bool
	log            *log.ComponentLogger

	// turnActive returns whether a turn is currently in progress.
	// Set by the platform bot at construction time.
	turnActive func() bool
}

// MessageQueueConfig holds construction parameters for NewMessageQueue.
type MessageQueueConfig struct {
	Size           int
	RequireMention bool
	SteerMode      bool
	Throttle       *GroupThrottle
	TurnActive     func() bool
	Logger         *log.ComponentLogger
}

// NewMessageQueue creates a new message queue.
func NewMessageQueue(cfg MessageQueueConfig) *MessageQueue {
	size := cfg.Size
	if size <= 0 {
		size = 64
	}
	mq := &MessageQueue{
		ch:             make(chan QueuedMessage, size),
		throttle:       cfg.Throttle,
		requireMention: cfg.RequireMention,
		steerMode:      cfg.SteerMode,
		turnActive:     cfg.TurnActive,
		log:            cfg.Logger,
	}
	return mq
}

// Enqueue routes a message to the appropriate destination: steer buffer,
// throttle, or main channel. The routing logic is:
//
//  1. Group chat + requireMention + not a mention + no throttle -> drop silently
//  2. Group chat + mention + steerMode + turn active -> AppendSteer (urgent redirect)
//  3. Group chat + throttle active -> throttle.Add (mention flushes immediately, non-mention buffers)
//  4. SteerMode + turn active + text-only -> AppendSteer
//  5. Otherwise -> push to channel (drop + warn if full)
func (q *MessageQueue) Enqueue(msg QueuedMessage) {
	isActive := q.turnActive != nil && q.turnActive()

	// Rule 1: group + require_mention + not a mention + no throttle -> drop
	if msg.IsGroupChat && q.requireMention && !msg.IsMention && q.throttle == nil {
		if q.log != nil {
			q.log.Debugf("queue: dropping non-mention group message from %s", q.senderLabel(msg))
		}
		return
	}

	// Rule 2: group + mention + steer + turn active -> steer (urgent redirect)
	if msg.IsGroupChat && msg.IsMention && q.steerMode && isActive && msg.Text != "" && len(msg.Attachments) == 0 {
		q.AppendSteer(msg.Text)
		if q.log != nil {
			q.log.Infof("steer: buffered mention from %s", q.senderLabel(msg))
		}
		return
	}

	// Rule 3: group + throttle -> buffer in throttle
	if msg.IsGroupChat && q.throttle != nil && !msg.IsMention {
		q.throttle.Add(msg)
		return
	}
	// Mentions with throttle active still go through throttle (they flush immediately)
	if msg.IsGroupChat && q.throttle != nil && msg.IsMention {
		q.throttle.Add(msg)
		return
	}

	// Rule 4: steer mode + turn active + text-only -> steer
	if q.steerMode && isActive && msg.Text != "" && len(msg.Attachments) == 0 {
		q.AppendSteer(msg.Text)
		if q.log != nil {
			q.log.Infof("steer: buffered message from %s", q.senderLabel(msg))
		}
		return
	}

	// Rule 5: push to channel
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

// Chan returns the receive-only channel for the worker select loop.
func (q *MessageQueue) Chan() <-chan QueuedMessage {
	return q.ch
}

// DrainQueue non-blocking drains all immediately available messages from the queue.
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

// AppendSteer adds text to the steer buffer.
func (q *MessageQueue) AppendSteer(text string) {
	q.steerMu.Lock()
	q.steerParts = append(q.steerParts, text)
	q.steerMu.Unlock()
}

// DrainSteer returns all pending steer parts and clears the buffer.
// Returns nil if no messages are pending.
func (q *MessageQueue) DrainSteer() []string {
	q.steerMu.Lock()
	defer q.steerMu.Unlock()
	if len(q.steerParts) == 0 {
		return nil
	}
	parts := q.steerParts
	q.steerParts = nil
	return parts
}

// SetThrottle sets the group throttle. Must be called before messages arrive.
func (q *MessageQueue) SetThrottle(t *GroupThrottle) {
	q.throttle = t
}

// SetRequireMention sets the require_mention flag. Must be called before messages arrive.
func (q *MessageQueue) SetRequireMention(v bool) {
	q.requireMention = v
}

// SetSteerMode sets the steer_mode flag. Must be called before messages arrive.
func (q *MessageQueue) SetSteerMode(v bool) {
	q.steerMode = v
}

// PushFlushed pushes a message from a throttle flush directly to the channel.
// Used as the throttle's flush callback target.
func (q *MessageQueue) PushFlushed(msg QueuedMessage) {
	q.pushToChannel(msg)
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

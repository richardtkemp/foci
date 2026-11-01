package slack

import (
	"encoding/json"
	"errors"
	"strconv"
	"sync"

	"foci/internal/chatmeta"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
)

// chatMetaKeySlackTarget is the chat_metadata key holding the JSON-encoded
// Target of a Slack chat: the reverse of the one-way chatID hash, so
// agent-initiated sends (send_to_session, notifications, wake results) reach
// the right Slack conversation after a restart.
const chatMetaKeySlackTarget = "slack_target"

// TargetKind discriminates which Slack conversation a session owns.
type TargetKind string

const (
	// TargetDM: the bot's direct-message conversation with one user. All DM
	// messages from that user are one session of the default agent.
	TargetDM TargetKind = "dm"
	// TargetThread: one thread of one channel. Each thread is one session of
	// the channel's mapped agent.
	TargetThread TargetKind = "thread"
)

// Target identifies the Slack conversation a session's messages belong in.
// Its identity string is the hash preimage for the chat ID, so two targets
// with different identities never share a session.
type Target struct {
	Kind     TargetKind `json:"kind"`
	Channel  string     `json:"channel"`             // DM channel id (D…) or channel id (C…)
	ThreadTS string     `json:"thread_ts,omitempty"` // thread targets only
	User     string     `json:"user,omitempty"`      // DM targets only
}

// identity is the stable string the chat ID hashes: one per DM user, one per
// channel thread.
func (t Target) identity() string {
	switch t.Kind {
	case TargetDM:
		return platformName + "/dm/" + t.User
	case TargetThread:
		return platformName + "/thread/" + t.Channel + "/" + t.ThreadTS
	default:
		return platformName + "/unknown/" + t.Channel + "/" + t.ThreadTS
	}
}

// ChatID is the stable int64 chat ID (agent/c<id> session keys derive from
// it). Deterministic, so the same DM or thread reaches the same session
// across restarts.
func (t Target) ChatID() int64 {
	return chatmeta.StableChatID(t.identity())
}

// threadTS returns t.ThreadTS for a thread target and "" for a DM (a DM
// reply is never threaded).
func (t Target) threadTS() string {
	if t.Kind == TargetThread {
		return t.ThreadTS
	}
	return ""
}

// targetFor builds the Target of an accepted inbound message.
func targetFor(msg inboundMessage) Target {
	if msg.Kind == inboundMention {
		return Target{Kind: TargetThread, Channel: msg.Channel, ThreadTS: msg.ThreadTS}
	}
	return Target{Kind: TargetDM, Channel: msg.Channel, User: msg.User}
}

// sessionKeyFor derives the session key of a target under an agent.
func sessionKeyFor(agentID string, t Target) string {
	return session.NewChatSessionKey(agentID, t.ChatID())
}

// errChatCollision reports a stored slack_target whose identity disagrees
// with the target just derived for the same chat ID — a hash collision
// between two different Slack conversations (probability ~2^-63). The message
// is refused rather than delivered into the other conversation's session.
var errChatCollision = errors.New("slack: chat id collision between two Slack conversations")

// targetStore is the durable DM/thread → chat ID reverse map. register
// persists a Target (before the message is enqueued, so any send the turn
// produces can route) plus the platform-ownership and never-default rows;
// lookup reads the cache, then chat_metadata. A nil store (no session index)
// is inert: register succeeds with a derived chat ID and caches the target,
// so routing stays stable within the process — at the cost of restart-stable
// routing, since there is nothing to persist to.
type targetStore struct {
	idx    platform.SessionIndex
	logger func() *log.ComponentLogger

	mu        sync.Mutex
	cache     map[string]Target             // agent + "\x00" + chatID → target
	resolvers map[string]*chatmeta.Resolver // per-agent row writers
}

func newTargetStore(idx platform.SessionIndex, logger func() *log.ComponentLogger) *targetStore {
	return &targetStore{
		idx:       idx,
		logger:    logger,
		cache:     make(map[string]Target),
		resolvers: make(map[string]*chatmeta.Resolver),
	}
}

func cacheKey(agentID string, chatID int64) string {
	return agentID + "\x00" + strconv.FormatInt(chatID, 10)
}

// resolver returns the per-agent chatmeta writer: RegisterChat persists the
// registered row (platform ownership, so SessionIndex.PlatformForChat routes
// outbound sends here) and, with NeverDefault set, the never_default row
// (Slack chats are never default chats, #2459). Idempotent and cached per
// chat; a failed write is retried on the next contact.
func (s *targetStore) resolver(agentID string) *chatmeta.Resolver {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.resolvers[agentID]
	if !ok {
		r = &chatmeta.Resolver{
			Index:        s.idx,
			AgentID:      agentID,
			PlatformName: platformName,
			Logger:       s.logger,
			NeverDefault: true,
		}
		s.resolvers[agentID] = r
	}
	return r
}

// register persists target under agentID and returns its chat ID. The
// writes are ordered so the route can only ever appear complete: first the
// never-default and registered rows (chatmeta.RegisterChat — a chat that
// could not be marked never-default must not become default-eligible,
// #2459 req 6), then the slack_target row (the hash preimage the reverse
// map resolves through), and only after every write has succeeded the
// in-memory cache — a failed write leaves no route behind (it can leave the
// earlier never-default/registered rows, which only exclude), so BotForSession
// stays nil and agent-initiated sends to the session are refused, not
// guessed at. Every failure propagates: the message is refused rather than
// accepted with an unroutable or under-marked conversation. A store with no
// session index has nothing to persist and caches immediately (routing is
// stable within the process only).
func (s *targetStore) register(agentID string, t Target) (int64, error) {
	chatID := t.ChatID()
	if s == nil {
		return chatID, nil
	}
	if s.idx != nil {
		if prev, ok := s.lookup(agentID, chatID); ok && prev.identity() != t.identity() {
			s.logger().Errorf("agent %s chat %d: %v (stored %s, derived %s)", agentID, chatID, errChatCollision, prev.identity(), t.identity())
			return 0, errChatCollision
		}
	}
	if s.idx == nil {
		s.mu.Lock()
		s.cache[cacheKey(agentID, chatID)] = t
		s.mu.Unlock()
		return chatID, nil
	}
	if err := s.resolver(agentID).RegisterChat(chatID); err != nil {
		s.logger().Errorf("agent %s chat %d: registration: %v", agentID, chatID, err)
		return 0, err
	}
	blob, err := json.Marshal(t)
	if err != nil {
		return 0, err
	}
	if err := s.idx.SetChatMetadata(agentID, platformName, chatID, chatMetaKeySlackTarget, string(blob)); err != nil {
		s.logger().Errorf("agent %s chat %d: persist %s: %v", agentID, chatID, chatMetaKeySlackTarget, err)
		return 0, err
	}
	s.mu.Lock()
	s.cache[cacheKey(agentID, chatID)] = t
	s.mu.Unlock()
	return chatID, nil
}

// lookup resolves the Target of an (agent, chatID). Cache first, then
// chat_metadata; it creates nothing — a chat that was never registered is
// simply unroutable (its sends are dropped and logged).
func (s *targetStore) lookup(agentID string, chatID int64) (Target, bool) {
	if s == nil || chatID == 0 {
		return Target{}, false
	}
	key := cacheKey(agentID, chatID)
	s.mu.Lock()
	t, ok := s.cache[key]
	s.mu.Unlock()
	if ok {
		return t, true
	}
	if s.idx == nil {
		return Target{}, false
	}
	blob, err := s.idx.GetChatMetadata(agentID, platformName, chatID, chatMetaKeySlackTarget)
	if err != nil || blob == "" {
		return Target{}, false
	}
	if err := json.Unmarshal([]byte(blob), &t); err != nil || t.Kind != TargetDM && t.Kind != TargetThread {
		return Target{}, false
	}
	s.mu.Lock()
	s.cache[key] = t
	s.mu.Unlock()
	return t, true
}

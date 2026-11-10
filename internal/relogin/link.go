package relogin

// The sign-in link message (#2384). Every attempt extracts a new sign-in URL,
// and only the newest is valid. Within one pending login, the link is therefore
// ONE updatable message: the first attempt sends it, each later attempt
// rewrites it in place with the new URL, and an attempt that fails rewrites it
// to say the link expired. Dick's ruling (2026-10-13): edit in place, so a
// later link fires no new push or unread, and no "edited" marker shows. Once
// the login completes the message is forgotten, so the next login sends a
// fresh one rather than editing a message far up the chat.
//
// The link goes to one chat. While it is valid, a user message in another
// chat (another session, or another Claude Code agent: all share the one
// login) gets the same link repeated there, and the code pasted in any chat
// that has the link completes the login (#3011, Dick 2026-11-10). No new
// attempt starts, so no new link replaces the first.

import (
	"errors"

	"foci/internal/log"
)

// errNoTarget: the chat has no connection to carry the link.
var errNoTarget = errors.New("no connection for the chat")

// LinkTarget is where one attempt's link message goes. The gateway builds it
// from the connection resolved for the triggering session.
type LinkTarget struct {
	// Key identifies the chat (platform + session). An existing link message
	// is rewritten only when the new attempt's Key matches; otherwise it is
	// marked expired and a new message goes to the new chat.
	Key string
	// SessionKey is the session of the chat the message goes to, used to
	// tell a reply in this chat (the login code) from one in another chat
	// (#3011). "" when the chat is not known: then any chat of the agent
	// counts as this one.
	SessionKey string
	// Send sends a new message. It returns an id that Edit accepts, or ""
	// when this connection cannot edit messages (the message is then
	// sent but not tracked).
	Send func(text string) (id string, err error)
	// Edit rewrites message id. nil when Send never returns an id.
	Edit func(id, text string) error
}

// linkMessage is the link message sent in the current pending login.
type linkMessage struct {
	key  string
	id   string
	edit func(id, text string) error
}

// chatRef names one chat: an agent and a session key.
type chatRef struct{ agentID, sessionKey string }

// linkChat is one chat that has the running attempt's sign-in link.
type linkChat struct {
	shown  bool        // the link is on screen there; false while it is being sent
	target *LinkTarget // nil for the chat the link was first sent to (g.link tracks that message)
	id     string      // the repeated message, "" when it cannot be edited
}

// chatLocked returns the chat entry for (agentID, sessionKey), or the entry of
// the first chat when that one's session is unknown and agentID matches; mu
// must be held.
func (g *Gate) chatLocked(agentID, sessionKey string) *linkChat {
	if c := g.chats[chatRef{agentID, sessionKey}]; c != nil {
		return c
	}
	return g.chats[chatRef{agentID, ""}]
}

// OfferLink repeats the running attempt's sign-in link in the chat (agentID,
// sessionKey), where a user message arrived that is not the code because the
// link was never shown there (#3011). From then on, that chat's next user
// message is the code. It does nothing when the capture window is closed (no
// link yet, or a code is already in), when the chat already has the link or
// is being sent it, or when target is nil (an agent that cannot carry the
// link). target builds where a message to that session goes; the link is
// sent outside the lock. Reports whether the link was repeated.
func (g *Gate) OfferLink(agentID, sessionKey string, target func(sessionKey string) *LinkTarget) bool {
	if target == nil {
		return false
	}
	ref := chatRef{agentID, sessionKey}
	g.mu.Lock()
	if !g.active || !g.captureOpen || g.linkText == "" || g.chatLocked(agentID, sessionKey) != nil {
		g.mu.Unlock()
		return false
	}
	c := &linkChat{}
	g.chats[ref] = c
	text, attempt := g.linkText, g.attempt
	g.mu.Unlock()

	t := target(sessionKey)
	var id string
	err := errNoTarget
	if t != nil {
		id, err = t.Send(text)
	}

	g.mu.Lock()
	if g.attempt != attempt || !g.active {
		// The attempt ended while the link was on its way: it is dead.
		g.mu.Unlock()
		if err == nil && id != "" && t.Edit != nil {
			_ = t.Edit(id, expiredLinkText)
		}
		return false
	}
	if err != nil {
		delete(g.chats, ref)
		g.mu.Unlock()
		log.NewComponentLogger(logComponent+"/"+agentID).Warnf("could not repeat the sign-in link in session %s: %v", sessionKey, err)
		return false
	}
	c.shown = true
	c.target = t
	if t.Edit != nil {
		c.id = id
	}
	g.mu.Unlock()
	return true
}

// notifyCodeChat sends text to the chat the code came from when that is not
// the chat the link was first sent to, which gets the driver's own notice.
// The user then sees how the login ended where they pasted the code.
func (g *Gate) notifyCodeChat(text string) error {
	g.mu.Lock()
	c := g.codeChat
	g.mu.Unlock()
	if c == nil || c.target == nil {
		return nil
	}
	_, err := c.target.Send(text)
	return err
}

// showLink puts text (the new sign-in link) in front of the user. It rewrites
// the pending login's link message in place when the target is the same chat.
// Otherwise it marks any old message expired and sends a new one.
func (g *Gate) showLink(t *LinkTarget, text string) error {
	g.mu.Lock()
	prev := g.link
	g.mu.Unlock()
	if prev != nil && prev.key == t.Key && prev.edit != nil {
		if err := prev.edit(prev.id, text); err == nil {
			return nil
		}
		// Edit failed (message deleted, too old to edit): send a new one.
	} else if prev != nil && prev.edit != nil {
		_ = prev.edit(prev.id, expiredLinkText)
	}
	id, err := t.Send(text)
	if err != nil {
		return err
	}
	g.mu.Lock()
	if id != "" && t.Edit != nil {
		g.link = &linkMessage{key: t.Key, id: id, edit: t.Edit}
	} else {
		g.link = nil
	}
	g.mu.Unlock()
	return nil
}

// expireLink rewrites the current link message to say it expired. The message
// stays tracked, so the next attempt's link replaces this text in place.
// The links repeated in other chats (#3011) are marked expired too.
func (g *Gate) expireLink() error {
	g.mu.Lock()
	prev := g.link
	var repeats []*linkChat
	for _, c := range g.chats {
		if c.target != nil && c.target.Edit != nil && c.id != "" {
			repeats = append(repeats, c)
		}
	}
	g.mu.Unlock()
	var firstErr error
	if prev != nil && prev.edit != nil {
		firstErr = prev.edit(prev.id, expiredLinkText)
	}
	for _, c := range repeats {
		if err := c.target.Edit(c.id, expiredLinkText); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

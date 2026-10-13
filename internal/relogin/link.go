package relogin

// The sign-in link message (#2384). Every attempt extracts a new sign-in URL,
// and only the newest is valid. Within one pending login, the link is therefore
// ONE updatable message: the first attempt sends it, each later attempt
// rewrites it in place with the new URL, and an attempt that fails rewrites it
// to say the link expired. Dick's ruling (2026-10-13): edit in place, so a
// later link fires no new push or unread, and no "edited" marker shows. Once
// the login completes the message is forgotten, so the next login sends a
// fresh one rather than editing a message far up the chat.

// LinkTarget is where one attempt's link message goes. The gateway builds it
// from the connection resolved for the triggering session.
type LinkTarget struct {
	// Key identifies the chat (platform + session). An existing link message
	// is rewritten only when the new attempt's Key matches; otherwise it is
	// marked expired and a new message goes to the new chat.
	Key string
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

// expiredLinkText replaces a link whose attempt ended without a login.
const expiredLinkText = "🔐 This Claude Code sign-in link has expired. Send /login for a new one."

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
func (g *Gate) expireLink() error {
	g.mu.Lock()
	prev := g.link
	g.mu.Unlock()
	if prev == nil || prev.edit == nil {
		return nil
	}
	return prev.edit(prev.id, expiredLinkText)
}

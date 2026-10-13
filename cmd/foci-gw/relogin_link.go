package main

import (
	"fmt"

	"foci/internal/platform"
	"foci/internal/relogin"
)

// reloginLinkTarget builds where one re-login attempt's sign-in link goes: an
// updatable message (platform.SessionMessageEditor) in the triggering
// session's chat, so later attempts rewrite it in place and only the newest
// link is visible (#2384). A connection without the capability still gets
// the link, but untracked. The connection is resolved at send time, the same
// way the driver's SendMessage resolves it.
func reloginLinkTarget(connMgr platform.ConnectionManager, agentID, sessionKey string) *relogin.LinkTarget {
	resolve := func() (platform.Connection, error) {
		conn := connMgr.ForSessionOrPrimary(sessionKey, agentID)
		if conn == nil {
			return nil, fmt.Errorf("no connection for agent %s", agentID)
		}
		return conn, nil
	}
	platformName := ""
	if conn, err := resolve(); err == nil {
		platformName = conn.PlatformName()
	}
	return &relogin.LinkTarget{
		Key: platformName + "|" + agentID + "|" + sessionKey,
		Send: func(text string) (string, error) {
			conn, err := resolve()
			if err != nil {
				return "", err
			}
			if ed, ok := conn.(platform.SessionMessageEditor); ok {
				return ed.SendEditableToSession(sessionKey, text)
			}
			return "", conn.SendToSession(sessionKey, text)
		},
		Edit: func(id, text string) error {
			conn, err := resolve()
			if err != nil {
				return err
			}
			ed, ok := conn.(platform.SessionMessageEditor)
			if !ok {
				return fmt.Errorf("connection %s cannot edit messages", conn.PlatformName())
			}
			return ed.EditSessionMessage(sessionKey, id, text)
		},
	}
}

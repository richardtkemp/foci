package main

import (
	"fmt"

	"foci/internal/app"
	"foci/internal/platform"
	"foci/internal/relogin"
)

// reloginLinkTarget builds where one re-login attempt's sign-in link goes: an
// updatable system notice (platform.SessionMessageEditor) in the triggering
// session's chat, so later attempts rewrite it in place and only the newest
// link is visible (#2384). A connection without the capability still gets
// the link, but untracked. The connection is resolved at send time, the same
// way the driver's SendMessage resolves it.
//
// An empty sessionKey (an automatic trigger has no chat) is resolved here to
// the default chat's session, so the gate can tell a reply in that chat (the
// code) from one in another chat, which gets the link repeated (#3011). When
// it cannot be resolved it stays empty, and any chat of the agent counts as
// the link's chat.
func reloginLinkTarget(connMgr platform.ConnectionManager, agentID, sessionKey string) *relogin.LinkTarget {
	if sessionKey == "" {
		if conn := connMgr.ForSessionOrPrimary("", agentID); conn != nil {
			sessionKey = defaultLinkSession(conn, agentID, app.CreateDefaultConversation)
		}
	}
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
		Key:        platformName + "|" + agentID + "|" + sessionKey,
		SessionKey: sessionKey,
		Send: func(text string) (string, error) {
			conn, err := resolve()
			if err != nil {
				return "", err
			}
			if ed, ok := conn.(platform.SessionMessageEditor); ok {
				return ed.SendEditableInjected(sessionKey, text)
			}
			return "", conn.SendInjectedMessage(sessionKey, text)
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
			return ed.EditInjectedMessage(sessionKey, id, text)
		},
	}
}

// defaultLinkSession is the session a send with no session key reaches on
// conn. The app sends it to the conversation its delivery ladder picks (or
// creates), which appCreate resolves; the chat platforms send it to their
// default chat. "" when it cannot be resolved.
func defaultLinkSession(conn platform.Connection, agentID string, appCreate func(agentID string) (string, error)) string {
	if conn.PlatformName() == "app" {
		sk, err := appCreate(agentID)
		if err != nil {
			return ""
		}
		return sk
	}
	return conn.DefaultSessionKey()
}

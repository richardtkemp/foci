package command

import (
	"context"
	"fmt"

	"foci/internal/session"
	"foci/prompts"
)

// forkMultiball forks the current session to a secondary multiball connection.
func forkMultiball(_ context.Context, req Request, cc CommandContext) (string, error) {
	if cc.ConnMgr == nil || !cc.ConnMgr.HasMultiball(cc.AgentConfig.ID) {
		return "", fmt.Errorf("no multiball bots configured")
	}
	secConn, ok := cc.ConnMgr.AcquireMultiball(cc.AgentConfig.ID)
	if !ok {
		return "", fmt.Errorf("all multiball bots are busy")
	}

	if cc.ConfigureMultiball != nil {
		cc.ConfigureMultiball(secConn)
	}

	parentKey := cc.DefaultSessionKey()
	if req.ChatID != 0 {
		if conn := cc.ConnMgr.Primary(cc.AgentConfig.ID); conn != nil {
			parentKey = conn.SessionKeyForChat(req.ChatID)
		} else {
			parentKey = session.NewChatSessionKey(cc.AgentConfig.ID, req.ChatID)
		}
	}
	if parentKey == "" {
		secConn.SetSessionKey("")
		return "", fmt.Errorf("no active session to fork from")
	}

	branchKey, err := session.BranchFromSession(parentKey)
	if err != nil {
		secConn.SetSessionKey("")
		return "", fmt.Errorf("create multiball key: %w", err)
	}

	orientPath := prompts.ResolveOrientPath(
		cc.AgentConfig.BranchOrientationMultiballPrompt, cc.Config.Sessions.BranchOrientationMultiballPrompt,
	)
	orientText := prompts.BuildBranchOrientation(orientPath, branchKey, parentKey, "multiball", true, cc.PromptSearchDirs)
	if err := cc.Sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
		OrientationMessage: orientText,
	}); err != nil {
		secConn.SetSessionKey("")
		return "", fmt.Errorf("create branch: %w", err)
	}

	// Multiball sessions default to no_compact=true (short-lived, shouldn't trigger compaction).
	if cc.AgentConfig.MultiballNoCompact == nil || *cc.AgentConfig.MultiballNoCompact {
		cc.Agent.SetSessionNoCompact(branchKey, true)
	}

	secConn.SetSessionKey(branchKey)
	if primary := cc.ConnMgr.Primary(cc.AgentConfig.ID); primary != nil {
		secConn.SetChatID(primary.ChatID())
	}
	secConn.SendNotification("🎱 Forked from main. What do you need?")

	return fmt.Sprintf("Forked to @%s (session: %s)", secConn.Username(), branchKey), nil
}


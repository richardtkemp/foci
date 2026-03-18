package command

import (
	"context"
	"fmt"

	"foci/internal/session"
	"foci/shared/prompts"
)

// forkFacet forks the current session to a secondary facet connection.
func forkFacet(_ context.Context, req Request, cc CommandContext) (string, error) {
	if cc.ConnMgr == nil || !cc.ConnMgr.HasFacet(cc.AgentConfig.ID) {
		return "", fmt.Errorf("no facet bots configured")
	}
	secConn, ok := cc.ConnMgr.AcquireFacet(cc.AgentConfig.ID)
	if !ok {
		return "", fmt.Errorf("all facet bots are busy")
	}

	if cc.ConfigureFacet != nil {
		cc.ConfigureFacet(secConn)
	}

	parentKey := req.SessionKey
	if parentKey == "" {
		parentKey = cc.DefaultSessionKey()
	}
	if parentKey == "" {
		secConn.SetSessionKey("")
		return "", fmt.Errorf("no active session to fork from")
	}

	branchKey, err := session.BranchFromSession(parentKey)
	if err != nil {
		secConn.SetSessionKey("")
		return "", fmt.Errorf("create facet key: %w", err)
	}

	orientPath := prompts.ResolveOrientPath(
		cc.AgentConfig.BranchOrientationFacetPrompt, cc.Config.Sessions.BranchOrientationFacetPrompt,
	)
	orientText := prompts.BuildBranchOrientation(orientPath, branchKey, parentKey, "facet", true, cc.PromptSearchDirs)
	if err := cc.Sessions.CreateBranchWithOptions(parentKey, branchKey, session.BranchOptions{
		OrientationMessage: orientText,
	}); err != nil {
		secConn.SetSessionKey("")
		return "", fmt.Errorf("create branch: %w", err)
	}

	// Facet sessions default to no_compact=true (short-lived, shouldn't trigger compaction).
	if cc.AgentConfig.FacetNoCompact == nil || *cc.AgentConfig.FacetNoCompact {
		cc.Agent.SetSessionNoCompact(branchKey, true)
	}

	secConn.SetSessionKey(branchKey)
	if primary := cc.ConnMgr.Primary(cc.AgentConfig.ID); primary != nil {
		secConn.SetChatID(primary.ChatID())
	}
	secConn.SendNotification("🎱 Forked from main. What do you need?")

	return fmt.Sprintf("Forked to @%s (session: %s)", secConn.Username(), branchKey), nil
}


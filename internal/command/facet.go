package command

import (
	"context"
	"fmt"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/shared/prompts"
)

// forkFacet forks the current session to a secondary facet connection.
func forkFacet(ctx context.Context, _ Request, cc CommandContext) (string, error) {
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

	parentKey := tools.SessionKeyFromContext(ctx)
	if parentKey == "" {
		secConn.SetSessionKey("")
		return "", fmt.Errorf("no active session to fork from")
	}

	orientPath := config.DerefStr(config.First(
		cc.AgentConfig.Sessions.BranchOrientationFacetPrompt, cc.Config.Sessions.BranchOrientationFacetPrompt,
	))
	orientTemplate := prompts.ResolveOrientationTemplate(orientPath, true, cc.PromptSearchDirs...)
	branchKey, err := cc.Sessions.CreateBranchWithOptions(parentKey, session.BranchOptions{
		BranchType:          "facet",
		OrientationTemplate: orientTemplate,
	})
	if err != nil {
		secConn.SetSessionKey("")
		return "", fmt.Errorf("create branch: %w", err)
	}
	cc.Agent.TouchRootCacheForBranch(branchKey) // branching warms root's shared prefix once

	// Facet sessions default to no_compact=true (short-lived, shouldn't trigger compaction).
	if cc.AgentConfig.Sessions.FacetNoCompact == nil || *cc.AgentConfig.Sessions.FacetNoCompact {
		cc.Agent.SetSessionNoCompact(branchKey, true)
	}

	secConn.SetSessionKey(branchKey)
	if primary := cc.ConnMgr.Primary(cc.AgentConfig.ID); primary != nil {
		chatID := primary.ChatID()
		log.Debugf("facet", "primary chatID=%d username=%s for agent %s", chatID, primary.Username(), cc.AgentConfig.ID)
		secConn.SetChatID(chatID)
	} else {
		log.Warnf("facet", "no primary connection for agent %s — notification may fail", cc.AgentConfig.ID)
	}
	secConn.SendNotification("🎱 Forked from main. What do you need?")

	return fmt.Sprintf("Forked to @%s (session: %s)", secConn.Username(), branchKey), nil
}

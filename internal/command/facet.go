package command

import (
	"context"
	"fmt"

	"foci/internal/config"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/shared/prompts"
)

// forkFacet forks the current session and surfaces it to the user. App clients
// get a new app conversation (opened on the requesting device); telegram/discord
// get a secondary facet bot. The fork itself is shared across both.
func forkFacet(ctx context.Context, req Request, cc CommandContext) (Response, error) {
	parentKey := tools.SessionKeyFromContext(ctx)
	if parentKey == "" {
		return Response{}, fmt.Errorf("no active session to fork from")
	}

	orientPath := config.DerefStr(config.First(
		cc.AgentConfig.Sessions.BranchOrientationFacetPrompt, cc.Config.Sessions.BranchOrientationFacetPrompt,
	))
	orientTemplate := prompts.ResolveOrientationTemplate(orientPath, true, cc.PromptSearchDirs...)
	opts := session.BranchOptions{
		BranchType:          "facet",
		OrientationTemplate: orientTemplate,
	}

	if req.Source == "app" {
		return forkFacetApp(ctx, cc, parentKey, opts)
	}
	return forkFacetBot(ctx, cc, parentKey, opts)
}

// forkFacetBranch forks parentKey through the single fork entry point. On a
// fork-capable delegated backend (CC/opencode) this is a REAL transcript fork
// carrying the parent's full conversation; an API agent — or a delegated backend
// whose session hasn't started yet — falls back to a plain history-reading branch
// (unchanged from the pre-fork behaviour).
func forkFacetBranch(ctx context.Context, cc CommandContext, parentKey string, opts session.BranchOptions) (string, error) {
	branchKey, forked, err := cc.Agent.ForkSession(ctx, parentKey, opts)
	if err != nil {
		return "", fmt.Errorf("fork session: %w", err)
	}
	if !forked {
		branchKey, err = cc.Sessions.CreateBranchWithOptions(parentKey, opts)
		if err != nil {
			return "", fmt.Errorf("create branch: %w", err)
		}
		cc.Agent.TouchRootCacheForBranch(branchKey) // ForkSession already warms root on success
	}

	// Facet sessions default to no_compact=true (short-lived, shouldn't trigger compaction).
	// Ladder: per-agent override, then the global [sessions] value, then the built-in default.
	facetNoCompact := config.First(cc.AgentConfig.Sessions.FacetNoCompact, cc.Config.Sessions.FacetNoCompact)
	if facetNoCompact == nil || *facetNoCompact {
		cc.Agent.SetSessionNoCompact(branchKey, true)
	}
	return branchKey, nil
}

// forkFacetApp surfaces the facet as a new app conversation and asks the
// requesting device to foreground it.
func forkFacetApp(ctx context.Context, cc CommandContext, parentKey string, opts session.BranchOptions) (Response, error) {
	if cc.MintFacetConversation == nil {
		return Response{}, fmt.Errorf("app facets not available")
	}
	branchKey, err := forkFacetBranch(ctx, cc, parentKey, opts)
	if err != nil {
		return Response{}, err
	}
	convID, err := cc.MintFacetConversation(cc.AgentConfig.ID, branchKey)
	if err != nil {
		return Response{}, fmt.Errorf("surface facet conversation: %w", err)
	}
	return Response{Text: "🎱 Forked to a new conversation.", OpenConversationID: convID}, nil
}

// forkFacetBot surfaces the facet on a secondary bot from the facet pool
// (telegram/discord).
func forkFacetBot(ctx context.Context, cc CommandContext, parentKey string, opts session.BranchOptions) (Response, error) {
	if cc.ConnMgr == nil || !cc.ConnMgr.HasFacet(cc.AgentConfig.ID) {
		return Response{}, fmt.Errorf("no facet bots configured")
	}
	secConn, ok := cc.ConnMgr.AcquireFacet(cc.AgentConfig.ID)
	if !ok {
		return Response{}, fmt.Errorf("all facet bots are busy")
	}
	if cc.ConfigureFacet != nil {
		cc.ConfigureFacet(secConn)
	}

	branchKey, err := forkFacetBranch(ctx, cc, parentKey, opts)
	if err != nil {
		secConn.SetSessionKey("")
		return Response{}, err
	}

	secConn.SetSessionKey(branchKey)
	if primary := cc.ConnMgr.Primary(cc.AgentConfig.ID); primary != nil {
		secConn.SetChatID(primary.ChatID())
	} else {
		facetLog.Warnf("no primary connection for agent %s — notification may fail", cc.AgentConfig.ID)
	}
	secConn.SendNotification("🎱 Forked from main. What do you need?")

	return Response{Text: fmt.Sprintf("Forked to @%s (session: %s)", secConn.Username(), branchKey)}, nil
}

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

	orientPath := resolveOrientPath(
		cc.AgentConfig.BranchOrientationMultiballPrompt, cc.Config.Sessions.BranchOrientationMultiballPrompt,
		cc.AgentConfig.BranchOrientationPrompt, cc.Config.Sessions.BranchOrientationPrompt,
	)
	orientText := BuildBranchOrientation(orientPath, branchKey, parentKey, "multiball", true, cc.PromptSearchDirs)
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

// BuildBranchOrientation constructs orientation text for a branch session.
// Resolves the prompt through ResolvePrompt: explicit path → search dirs → embedded default.
// Template variables: {branch_key}, {parent_key}, {branch_type}, {direct_chat}.
func BuildBranchOrientation(promptPath, branchKey, parentKey, branchType string, directChat bool, searchDirs []string) string {
	var filename, embedded string
	if directChat {
		filename = "branch-orientation-multiball.md"
		embedded = prompts.BranchOrientationMultiball()
	} else {
		filename = "branch-orientation-headless.md"
		embedded = prompts.BranchOrientationHeadless()
	}
	text := prompts.ResolvePrompt(promptPath, filename, embedded, searchDirs...)
	return prompts.ReplaceVars(text, map[string]string{
		"branch_key":  branchKey,
		"parent_key":  parentKey,
		"branch_type": branchType,
		"direct_chat": fmt.Sprintf("%v", directChat),
	})
}

// resolveOrientPath picks the first non-empty value from a priority list:
// specific type → global type → specific fallback → global fallback.
func resolveOrientPath(specific, globalSpecific, fallback, globalFallback string) string {
	if specific != "" {
		return specific
	}
	if globalSpecific != "" {
		return globalSpecific
	}
	if fallback != "" {
		return fallback
	}
	return globalFallback
}

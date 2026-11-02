package command

import (
	"context"
	"fmt"

	"foci/internal/session"
	"foci/internal/tools"
)

// forkBranchType is the branch_type of a /fork chat. It is not "facet": a fork
// is an ordinary chat, so it gets no orientation prompt and compaction stays on.
const forkBranchType = "fork"

// ForkCommand returns /fork [message] (#957, #2780): fork the current
// conversation at its end into a new app chat. Any text after /fork is sent
// into the new chat as the user's first message, as if typed there, so its turn
// runs in the new chat and not in this one. With no text the new chat waits.
//
// Forking AT an earlier message is not offered (Dick, 2026-11-01, #2748).
func ForkCommand() *Command {
	return &Command{
		Name:        "fork",
		Description: "Fork this conversation into a new chat; text after /fork is sent there",
		Category:    "session",
		// Same listing rule as /facet: offered only where the fork can run.
		Visible: branchCapableVisible,
		Execute: forkChat,
	}
}

// forkChat runs /fork. The parent session is not changed.
func forkChat(ctx context.Context, req Request, cc CommandContext) (Response, error) {
	parentKey := tools.SessionKeyFromContext(ctx)
	if parentKey == "" {
		return Response{}, fmt.Errorf("no active session to fork from")
	}
	if !branchCapable(cc) {
		return Response{}, fmt.Errorf("this agent's backend cannot branch, so it cannot fork")
	}
	if cc.MintFacetConversation == nil {
		return Response{}, fmt.Errorf("app chats not available")
	}

	branchKey, err := forkBranch(ctx, cc, parentKey, session.BranchOptions{BranchType: forkBranchType})
	if err != nil {
		return Response{}, err
	}
	convID, err := cc.MintFacetConversation(cc.AgentConfig.ID, branchKey, firstMessageFrom(req))
	if err != nil {
		return Response{}, fmt.Errorf("surface forked chat: %w", err)
	}
	return Response{
		Text:               fmt.Sprintf("🍴 Forked to a new chat: %s (session %s).", convID, branchKey),
		OpenConversationID: convID,
	}, nil
}

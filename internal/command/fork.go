package command

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/session"
	"foci/internal/tools"
)

// forkBranchType is the branch_type of a /fork chat. It is not "facet": a fork
// is an ordinary chat, so it gets no orientation prompt and compaction stays on.
const forkBranchType = "fork"

// ForkCommand returns /fork [message=<id>] (#957): fork the current
// conversation into a new app chat. Absent message= forks at the end (the
// conversation as it stands now).
//
// Forking AT a message is not built: foci keeps no record of where an app
// message id sits in a session's conversation (or in a backend's transcript),
// so it cannot turn the id into a cut point. A named message is refused, never
// quietly replaced by a fork from the end.
func ForkCommand() *Command {
	return &Command{
		Name:        "fork",
		Description: "Fork this conversation into a new chat",
		Category:    "session",
		// Same listing rule as /facet: offered only where the fork can run.
		Visible: branchCapableVisible,
		Execute: forkChat,
	}
}

// forkChat runs /fork. The parent session is not changed.
func forkChat(ctx context.Context, req Request, cc CommandContext) (Response, error) {
	messageID, err := parseForkArgs(req.Args)
	if err != nil {
		return Response{}, err
	}
	parentKey := tools.SessionKeyFromContext(ctx)
	if parentKey == "" {
		return Response{}, fmt.Errorf("no active session to fork from")
	}
	if !branchCapable(cc) {
		return Response{}, fmt.Errorf("this agent's backend cannot branch, so it cannot fork")
	}
	if messageID != "" {
		return Response{}, fmt.Errorf("cannot fork at message %s: forking at a message is not supported yet (foci does not record where an app message sits in the conversation). Use /fork with no argument to fork at the end", messageID)
	}
	if cc.MintFacetConversation == nil {
		return Response{}, fmt.Errorf("app chats not available")
	}

	branchKey, err := forkBranch(ctx, cc, parentKey, session.BranchOptions{BranchType: forkBranchType})
	if err != nil {
		return Response{}, err
	}
	convID, err := cc.MintFacetConversation(cc.AgentConfig.ID, branchKey)
	if err != nil {
		return Response{}, fmt.Errorf("surface forked chat: %w", err)
	}
	return Response{
		Text:               fmt.Sprintf("🍴 Forked to a new chat: %s (session %s).", convID, branchKey),
		OpenConversationID: convID,
	}, nil
}

// parseForkArgs reads /fork's arguments: nothing, or message=<id>.
func parseForkArgs(args string) (messageID string, err error) {
	const usage = "usage: /fork [message=<id>]"
	fields := strings.Fields(args)
	switch len(fields) {
	case 0:
		return "", nil
	case 1:
		v, ok := strings.CutPrefix(fields[0], "message=")
		if !ok || v == "" {
			return "", fmt.Errorf("%s", usage)
		}
		return v, nil
	default:
		return "", fmt.Errorf("%s", usage)
	}
}

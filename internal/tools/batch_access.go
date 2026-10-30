package tools

import (
	"context"
	"fmt"

	"foci/internal/delegator"
	"foci/internal/session"
)

// spawnToolNames are the spawn family. No one-shot gets them: a one-shot that
// could spawn would recurse with nobody watching (the API one-shots' tool
// sets skip spawn too, spawnToolSet).
var spawnToolNames = map[string]bool{
	"spawn":        true,
	"spawn_send":   true,
	"spawn_status": true,
	"spawn_stop":   true,
}

// parentBatchExcluded lists the tools a parent-access batch session's exec
// bridge (spawn character) leaves out of the agent's own set: the spawn
// family (above); close_session, which on a batch session (keyed under its
// owner's root) would close the OWNER's chat on a throwaway session's say-so
// (#2573); and ask, since nobody is there to answer a character spawn
// (#2643: "anything that would need the user is refused").
var parentBatchExcluded = map[string]bool{
	"close_session": true,
	"ask":           true,
}

// Subset returns a registry holding only the tools keep accepts. The tools
// are shared, not copied.
func (r *Registry) Subset(keep func(name string) bool) *Registry {
	out := NewRegistry()
	for name, t := range r.tools {
		if keep(name) {
			out.Register(t)
		}
	}
	return out
}

// RegistryForAccess returns the tools a session with this batch access may
// reach through its exec bridge, or nil for none (no bridge at all):
//   - default (every non-batch session, and foci's own batches): the agent's
//     whole registry;
//   - parent: the agent's registry less the spawn family, close_session and ask;
//   - isolated: none — the session runs no commands, so it has no shell to
//     reach a bridge from, and the bridge's tools are not confined to its
//     directory;
//   - read-only: the explore allowlist (spawnExploreAllowed), the same tools
//     an API explore spawn gets from the registry.
func RegistryForAccess(reg *Registry, access delegator.BatchAccess) *Registry {
	if reg == nil {
		return nil
	}
	switch access {
	case delegator.BatchAccessDefault:
		return reg
	case delegator.BatchAccessParent:
		return reg.Subset(func(name string) bool { return !spawnToolNames[name] && !parentBatchExcluded[name] })
	case delegator.BatchAccessReadOnly:
		return reg.Subset(func(name string) bool { return spawnExploreAllowed[name] })
	}
	return nil
}

// refuseCrossAgentSpawnMessage refuses send_to_session called from a
// cross-agent spawn's session (#2662): it runs as another agent but belongs
// to none of that agent's sessions, and its answer goes back to its caller
// as the spawn's result. (Its send_to_chat goes to the caller's chat
// instead: crossAgentChatSession.)
func refuseCrossAgentSpawnMessage(ctx context.Context, tool string) error {
	if session.IsCrossAgentSpawnKey(SessionKeyFromContext(ctx)) {
		return fmt.Errorf("%s: this session is a spawn run for another agent; it messages no session. To reach the agent that started you, use send_to_spawner, or put it in your final answer, which goes back to that agent", tool)
	}
	return nil
}

// refuseCrossAgentSpawnRemind refuses remind from a cross-agent spawn's
// session (Dick 2026-10-30 12:06): a wake would fire on a session no chat
// owns and a passive reminder would surface in the target's own chats, so it
// could never reach the caller.
func refuseCrossAgentSpawnRemind(ctx context.Context) error {
	if session.IsCrossAgentSpawnKey(SessionKeyFromContext(ctx)) {
		return fmt.Errorf("remind: this session is a spawn run for another agent; a reminder set here could never reach the agent that started you. Put it in your final answer instead")
	}
	return nil
}

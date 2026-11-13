package tools

import (
	"context"
	"fmt"

	"foci/internal/delegator"
	"foci/internal/session"
)

// spawnToolNames are the spawn family. A session gets them only while it may
// start a spawn under the spawn depth limit (#3061, SessionMaySpawn).
var spawnToolNames = map[string]bool{
	"spawn":        true,
	"spawn_send":   true,
	"spawn_status": true,
	"spawn_stop":   true,
}

// parentBatchExcluded lists the tools a parent-access batch session's exec
// bridge (spawn character) leaves out of the agent's own set: close_session, which on a batch session (keyed under its
// owner's root) would close the OWNER's chat on a throwaway session's say-so
// (#2573); and ask, since nobody is there to answer a character spawn
// (#2643). (Its permission requests go up to its spawner instead, #2684.)
// compact_session is left out too (#3040): an API character one-shot's tool
// calls run on its parent's context, so it would compact the PARENT's session.
// An API character one-shot leaves out the same (spawnCharacterBlacklist).
var parentBatchExcluded = map[string]bool{
	"close_session":   true,
	"compact_session": true,
	"ask":             true,
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
//   - parent: the agent's registry less close_session, compact_session and ask;
//   - isolated: none — the session runs no commands, so it has no shell to
//     reach a bridge from, and the bridge's tools are not confined to its
//     directory;
//   - read-only: the explore allowlist (spawnExploreAllowed), the same tools
//     an API explore spawn gets from the registry, with todo narrowed to its
//     read actions (readOnlyRegistry, #2702).
//
// maySpawn is whether the session may start a spawn under the spawn depth
// limit (#3061, SessionMaySpawn): when false, default and parent leave out
// the spawn family too. Read-only has none of it whatever maySpawn says.
func RegistryForAccess(reg *Registry, access delegator.BatchAccess, maySpawn bool) *Registry {
	if reg == nil {
		return nil
	}
	switch access {
	case delegator.BatchAccessDefault:
		if maySpawn {
			return reg
		}
		return reg.Subset(func(name string) bool { return !spawnToolNames[name] })
	case delegator.BatchAccessParent:
		return reg.Subset(func(name string) bool {
			return (maySpawn || !spawnToolNames[name]) && !parentBatchExcluded[name]
		})
	case delegator.BatchAccessReadOnly:
		return readOnlyRegistry(reg)
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

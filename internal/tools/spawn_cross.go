package tools

// Cross-agent spawns (#2662, Dick 2026-10-30): any agent may spawn any agent.
// The spawn runs AS the target: its system prompt and character files, its
// tools, workspace, permission rules, backend and model. Because it runs with
// the target's own character, a spawn as another agent is character mode
// only (raw, explore and clone name no character to run as).
//
// The caller's Spawner keeps what belongs to the caller: the tracker entry
// (keyed by the caller's session), the semaphore slot, the [SPAWN RESULT]
// delivery, spawn_send/spawn_status/spawn_stop. The target's Spawner (its
// registered one, RegisterSpawner) supplies what belongs to the target: the
// agent that runs the turns, its batch runner or API one-shot loop, its
// character branch, its usage reader. The target's sessions hang off
// session.CrossAgentSpawnRoot(target), which no chat of the target owns, so
// the target's chats see nothing of it.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/session"
)

// spawnerFor returns agentID's registered Spawner, or nil.
func spawnerFor(agentID string) *Spawner {
	spawnersMu.RLock()
	defer spawnersMu.RUnlock()
	return spawners[agentID]
}

// spawnAgentIDs lists the agents with a registered Spawner, sorted.
func spawnAgentIDs() []string {
	spawnersMu.RLock()
	defer spawnersMu.RUnlock()
	out := make([]string, 0, len(spawners))
	for id := range spawners {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// hostFor returns the Spawner the spawn runs on: s for the caller's own agent
// ("" or its own id), else the target's. A spawn as another agent is refused
// from inside a spawn, so no chain of agents spawning each other can start.
func (s *Spawner) hostFor(ctx context.Context, agentID string) (*Spawner, error) {
	if agentID == "" || agentID == s.deps.AgentID {
		return s, nil
	}
	caller := SessionKeyFromContext(ctx)
	if IsSpawnInherit(ctx) || IsSpawnCharacter(ctx) || s.isSpawnBranch(caller) || session.IsCrossAgentSpawnKey(caller) {
		return nil, fmt.Errorf("a spawn cannot start a spawn of another agent")
	}
	h := spawnerFor(agentID)
	if h == nil {
		var others []string
		for _, id := range spawnAgentIDs() {
			if id != s.deps.AgentID {
				others = append(others, id)
			}
		}
		return nil, fmt.Errorf("unknown agent %q, or it has no spawn tool (agents you can spawn: %s)", agentID, strings.Join(others, ", "))
	}
	return h, nil
}

// hostOf is the Spawner a tracked spawn runs on.
func (s *Spawner) hostOf(r RunningSpawn) *Spawner {
	if r.Agent == "" || r.Agent == s.deps.AgentID {
		return s
	}
	if h := spawnerFor(r.Agent); h != nil {
		return h
	}
	return nil
}

// agentOf is the agent that runs a tracked spawn's turns; nil when it is not
// available (its agent is gone).
func (s *Spawner) agentOf(r RunningSpawn) SpawnAgent {
	h := s.hostOf(r)
	if h == nil {
		return nil
	}
	return h.agentFn()
}

// crossAgentContext is ctx for a direct spawn run as another agent: its
// session is the target's cross-agent root (so the target's tools act as the
// target and reach none of its chats), it carries the caller's session (for
// send_to_chat: crossAgentChatSession), and it books its calls on a turn of
// its own, not on the caller's turn, since they are the target's spend.
func crossAgentContext(ctx context.Context, target, caller string) context.Context {
	ctx = WithSessionKey(ctx, session.CrossAgentSpawnRoot(target))
	ctx = context.WithValue(ctx, crossAgentCallerKey{}, caller)
	return accounting.WithTurn(ctx, accounting.Turn{})
}

type crossAgentCallerKey struct{}

// crossAgentCallerFrom is the caller session a direct cross-agent spawn's ctx
// carries ("" for any other ctx).
func crossAgentCallerFrom(ctx context.Context) string {
	c, _ := ctx.Value(crossAgentCallerKey{}).(string)
	return c
}

// crossAgentBatches maps the session key of a direct cross-agent spawn's
// delegated batch to its caller's session, while the batch runs. Its tool
// calls come back over the exec bridge with only the batch's session key, so
// this is how its send_to_chat finds the caller.
var crossAgentBatches sync.Map // batch session key -> caller session key

// crossAgentChatSession is the session a cross-agent spawn's send_to_chat
// goes to (Dick 2026-10-30 12:06: "route its send_to_chat into the caller's
// chat"): the caller's session, so the message lands in the caller's chat with
// its human. sessionKey is the spawn's own session. ok is false when no
// caller is known (the spawn has ended or the key is not a spawn's).
//
// A direct API spawn carries the caller in its ctx; a direct delegated batch
// is in crossAgentBatches; a background spawn's branch resolves through the
// target's tracker entry to its current owner (SpawnBranchOwner).
func crossAgentChatSession(ctx context.Context, sessionKey string) (string, bool) {
	if c := crossAgentCallerFrom(ctx); c != "" {
		return c, true
	}
	if c, ok := crossAgentBatches.Load(sessionKey); ok {
		return c.(string), true
	}
	return SpawnBranchOwner(sessionKey)
}

// runDirectAs runs a direct (non-background) spawn of mode on host: the
// caller's own agent, or the target of a cross-agent spawn.
func (s *Spawner) runDirectAs(ctx context.Context, host *Spawner, mode, prompt, model string, timeout time.Duration) (ToolResult, error) {
	owner := SessionKeyFromContext(ctx)
	if host != s {
		ctx = crossAgentContext(ctx, host.deps.AgentID, owner)
		owner = session.CrossAgentSpawnRoot(host.deps.AgentID)
	}
	if host.deps.Delegated != nil {
		return host.runBatchOneShot(ctx, mode, prompt, model, timeout, owner)
	}
	return host.runAPIOneShot(ctx, mode, prompt, model, timeout)
}

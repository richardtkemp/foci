package tools

// Spawn nesting (#3061, Dick 2026-11-13: "Default max depth 2"; "API spawns
// should be able to spawn, but governed by depth limit"). One rule decides
// whether a session may start a spawn, for every mode and backend and for a
// spawn as another agent:
//
//   - A session's spawn depth: a session no spawn started (a chat, a cron or
//     memory branch, a depth-mode leaf) is 0; a spawn of a depth-N session is
//     N+1 (RunningSpawn.Depth, persisted with the spawn).
//   - A session at depth d may start a spawn only if d+1 <= the limit
//     ([tools] max_spawn_depth, default 2, live: MaxSpawnDepth). With 2:
//     main -> spawn -> spawn, and the depth-2 spawn cannot spawn.
//   - A session that may not spawn is not offered the spawn tools
//     (SessionMaySpawn: the delegated exec bridge, RegistryForAccess; the
//     shell-tool listing in its prompt; the API turn's tool list,
//     ToolDefsForSession; an API one-shot's tool set). The spawn tool still
//     checks (checkSpawnDepth), for a session launched before the limit was
//     lowered, and its refusal names the limit and the session's depth.
//
// A foreground (direct) spawn may start no spawn at all, of any mode, at any
// depth (#3081, Dick 2026-11-13 18:13: "Let's go with foreground spawns may
// not respawn in any situation"): an API one-shot or a delegated batch that
// returns its result directly ends with its spawner's call, and its spawner
// blocks on it meanwhile. So it is not offered the spawn tools either
// (SessionMaySpawn is false for a direct batch's key; an API one-shot's tool
// set leaves them out), and the spawn tool refuses its call
// (checkDirectSpawnCaller). Background spawns (a background character spawn,
// a clone) may start any mode, a clone included (Q7: "only bg spawns can
// clone"), within the depth limit.
//
// Depth mode (fan-out trees, depth.go) is not counted: its leaves are not
// spawns, so a leaf is a depth-0 session here and its spawn is depth 1 (Dick
// 2026-11-13 01:27: a leaf's spawn counts "as if the leaf were a root
// session"), and depth mode keeps its own gate (depthApprovalLevel).
//
// How a session's depth is found (the same lookups send_to_spawner uses to
// find a spawn's spawner, spawnOf): a direct API one-shot's tool calls carry
// its spawnLink in ctx; a direct delegated batch's come over the exec bridge
// with only its key, which directSpawnBatches maps while it runs; a
// background spawn's branch key is in the tracker (ForBranch).

import (
	"context"
	"fmt"
	"sync/atomic"

	"foci/internal/provider"
)

// DefaultMaxSpawnDepth is the spawn depth limit when the config gives none
// (Dick 2026-11-13).
const DefaultMaxSpawnDepth = 2

// maxSpawnDepth is the live limit ([tools] max_spawn_depth), process-wide:
// a chain of spawns crosses agents (a spawn as another agent), so one number
// governs it.
var maxSpawnDepth atomic.Int64

func init() { maxSpawnDepth.Store(DefaultMaxSpawnDepth) }

// SetMaxSpawnDepth sets the spawn depth limit, effective for the next spawn
// call and the next session launch: at startup and from the live applier.
// A value below 1 counts as 1, so a session no spawn started can always spawn.
func SetMaxSpawnDepth(n int) {
	if n < 1 {
		n = 1
	}
	maxSpawnDepth.Store(int64(n))
}

// MaxSpawnDepth is the current spawn depth limit.
func MaxSpawnDepth() int { return int(maxSpawnDepth.Load()) }

// isSpawnFamily reports whether name is one of the spawn tools (spawn,
// spawn_send, spawn_status, spawn_stop): the tools a session that may not
// spawn is not offered.
func isSpawnFamily(name string) bool { return spawnToolNames[name] }

// IsSpawnFamilyShellFunc reports whether a shell function name (foci_<tool>)
// is one of the spawn tools'.
func IsSpawnFamilyShellFunc(name string) bool {
	const prefix = "foci_"
	return len(name) > len(prefix) && name[:len(prefix)] == prefix && isSpawnFamily(name[len(prefix):])
}

// spawnDepthIn is the spawn depth of the session sessionKey as tr and the
// direct-batch map know it: a direct delegated batch's or a background
// spawn's branch; 0 for any other session.
func spawnDepthIn(tr *SpawnTracker, sessionKey string) int {
	if sessionKey == "" {
		return 0
	}
	if v, ok := directSpawnBatches.Load(sessionKey); ok {
		return v.(spawnLink).info.Depth
	}
	if _, info, ok := tr.ForBranch(sessionKey); ok {
		return info.Depth
	}
	return 0
}

// SessionSpawnDepth is the spawn depth of the session sessionKey: 0 unless a
// spawn runs on it (a direct delegated batch, a background spawn's branch).
func SessionSpawnDepth(sessionKey string) int {
	return spawnDepthIn(Spawns, sessionKey)
}

// SessionMaySpawn reports whether the session sessionKey may start a spawn,
// so whether it is offered the spawn tools: not a direct spawn's delegated
// batch (#3081), and under the current depth limit.
func SessionMaySpawn(sessionKey string) bool {
	if _, direct := directSpawnBatches.Load(sessionKey); direct {
		return false
	}
	return SessionSpawnDepth(sessionKey) < MaxSpawnDepth()
}

// callerSpawnDepth is the spawn depth of the session a spawn tool call comes
// from: a direct API one-shot's from its ctx, else from its session key.
func (s *Spawner) callerSpawnDepth(ctx context.Context) int {
	if l, ok := spawnLinkFrom(ctx); ok {
		return l.info.Depth
	}
	key := SessionKeyFromContext(ctx)
	if d := spawnDepthIn(s.deps.Tracker, key); d > 0 || s.deps.Tracker == Spawns {
		return d
	}
	// A spawn as this agent from another one is tracked by its caller's
	// tracker: Spawns, in the gateway.
	return spawnDepthIn(Spawns, key)
}

// checkSpawnDepth refuses a spawn call from a session already at the limit.
func (s *Spawner) checkSpawnDepth(ctx context.Context) error {
	depth, limit := s.callerSpawnDepth(ctx), MaxSpawnDepth()
	if depth+1 > limit {
		return fmt.Errorf("NOT started: this session is a spawn at depth %d and the spawn depth limit is %d ([tools] max_spawn_depth), so it cannot start another spawn. Do the work yourself, or say in your result what is left", depth, limit)
	}
	return nil
}

// callerIsDirectSpawn reports whether the call comes from a direct spawn's
// session (an API one-shot, a delegated batch): one that ends with its
// spawner's call.
func callerIsDirectSpawn(ctx context.Context) bool {
	if _, ok := spawnLinkFrom(ctx); ok {
		return true
	}
	_, ok := directSpawnBatches.Load(SessionKeyFromContext(ctx))
	return ok
}

// checkDirectSpawnCaller refuses a spawn call from a foreground (direct)
// spawn, whatever the mode (#3081). It is not offered the spawn tools, so
// this catches only a session launched with them before the rule.
func checkDirectSpawnCaller(ctx context.Context) error {
	if callerIsDirectSpawn(ctx) {
		return fmt.Errorf("NOT started: you are a foreground spawn (your spawner waits for your result), and a foreground spawn cannot start a spawn of any kind. Do the work yourself, or say in your result what is left")
	}
	return nil
}

// ToolDefsForSession is ToolDefs for an API turn on sessionKey: without the
// spawn family when the session may not start a spawn (SessionMaySpawn). The
// list is fixed for a session (its depth does not change), so it stays a
// stable cached prefix.
func (r *Registry) ToolDefsForSession(sessionKey string) []provider.ToolDef {
	defs := r.ToolDefs()
	if SessionMaySpawn(sessionKey) {
		return defs
	}
	out := defs[:0:0]
	for _, d := range defs {
		if !isSpawnFamily(d.Name()) {
			out = append(out, d)
		}
	}
	return out
}

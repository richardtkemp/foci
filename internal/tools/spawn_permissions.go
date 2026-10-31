package tools

// Spawn permissions (#2684, Dick 2026-10-30: "Spawns (as they are always from
// interactive sessions) should not skip permissions, they should report their
// permission requests up to the spawner"). A spawn's own session is no longer
// a skip-permissions session: an ask its backend raises that the auto-approve
// rules do not settle is shown in its spawner's chat (the gateway's backend
// prompt routing, through SpawnBranchOwner), named by SpawnOfSession, and the
// answer goes back to the spawn's waiting call. Two exceptions run unattended
// as before: the spawn of a turn nobody attends (RunningSpawn.SpawnerUnattended)
// and a cross-agent spawn, whose session belongs to no chat of its agent.

// SpawnSession describes the spawn running on a session: the session it was
// started from (Parent), what the tracker shows for it, and whether the
// turn that started it was unattended.
type SpawnSession struct {
	ID                string
	Description       string
	Parent            string
	Direct            bool
	SpawnerUnattended bool
}

// SpawnOfSession reports the spawn that runs on sessionKey: a direct
// delegated spawn's batch session (while its call waits) or a background
// spawn's branch. ok is false for any other session.
func SpawnOfSession(sessionKey string) (SpawnSession, bool) {
	if sessionKey == "" {
		return SpawnSession{}, false
	}
	if v, ok := directSpawnBatches.Load(sessionKey); ok {
		l := v.(spawnLink)
		return SpawnSession{
			ID: l.info.ID, Description: l.info.Description, Parent: l.parent,
			Direct: true, SpawnerUnattended: l.info.SpawnerUnattended,
		}, true
	}
	parent, info, ok := Spawns.ForBranch(sessionKey)
	if !ok {
		return SpawnSession{}, false
	}
	return SpawnSession{
		ID: info.ID, Description: info.Description, Parent: parent,
		Direct: info.Direct, SpawnerUnattended: info.SpawnerUnattended,
	}, true
}

// directBatchOwner is BranchOwner for a direct spawn's batch session: the
// current owner of the session that started it. A cross-agent direct spawn
// (the batch runs as another agent than the one that started it) has none here,
// so its asks reach no chat of the agent it runs as.
func (s *Spawner) directBatchOwner(batchKey string) (owner string, ok bool) {
	v, found := directSpawnBatches.Load(batchKey)
	if !found {
		return "", false
	}
	l := v.(spawnLink)
	if l.s == nil || l.s.deps.AgentID != s.deps.AgentID {
		return "", false
	}
	return l.s.route(l.parent).Owner, true
}

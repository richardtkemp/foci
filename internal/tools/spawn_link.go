package tools

import (
	"context"
	"time"

	"foci/internal/delegator/accounting"
)

// Spawn links (#2992): each spawn is recorded against the turn that started
// it, so the app's Message info lists a turn's spawns from a record. The
// parent turn is the caller session's live turn (SpawnDeps.LiveTurn) when the
// spawn tool runs; where the spawn's spend is booked is noted as each spawn
// path learns it (noteSpawnBooking).

type spawnRecordKey struct{}

// recordSpawnLink stores a spawn of caller's live turn in the ledger and
// returns ctx carrying its id for noteSpawnBooking. With no live turn or no
// ledger nothing is stored, and the spawn is not listed under any turn.
func (s *Spawner) recordSpawnLink(ctx context.Context, caller, name, mode string) context.Context {
	l := accounting.Live()
	if l == nil || caller == "" || s.deps.LiveTurn == nil {
		return ctx
	}
	parent := s.deps.LiveTurn(caller)
	if parent == "" {
		return ctx
	}
	id := newSpawnID()
	if err := l.RecordSpawn(accounting.SpawnLink{SpawnID: id, ParentTurnID: parent, ParentSession: caller,
		Name: name, Mode: mode, StartedAt: time.Now()}); err != nil {
		spawnLog.Warnf("spawn link: %v", err)
		return ctx
	}
	return context.WithValue(ctx, spawnRecordKey{}, id)
}

// spawnIDFor returns the id of the spawn link ctx carries, so the running
// spawn takes the same id as its record (#2996): the app's transcript of a
// spawn is keyed by the running id (its subagent group key), and Message info
// names a spawn by its link id, so a finished spawn's row can open its
// transcript only when the two are one. With no recorded link it mints one.
func spawnIDFor(ctx context.Context) string {
	if id, _ := ctx.Value(spawnRecordKey{}).(string); id != "" {
		return id
	}
	return newSpawnID()
}

// noteSpawnBooking records where the spend of the spawn ctx carries is
// booked: its own session, its own ledger turn, or the parent turn itself
// (inParent). No-op for a spawn with no recorded link.
func noteSpawnBooking(ctx context.Context, childSession, childTurn string, inParent bool) {
	id, _ := ctx.Value(spawnRecordKey{}).(string)
	if id == "" {
		return
	}
	if err := accounting.Live().SetSpawnBooking(id, childSession, childTurn, inParent); err != nil {
		spawnLog.Warnf("spawn link: %v", err)
	}
}

package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"foci/internal/session"
)

// A user message accepted but not yet run survives a restart (#2999
// requirement 2).
//
// Enqueue writes every user message it puts on a session's queue to the
// session's queued_input metadata (session.MetaKeyQueuedInput) BEFORE it
// queues it, and the message leaves the store when it is settled:
//   - its turn starts (driveOnce: the batch it heads or joins);
//   - a /stop drops it (stoppedQueued, a dropped slot wait);
//   - the usage hold parks it (parkUsageHeld: it moves to usage_held);
//   - the queue was full (Enqueue drops it).
//
// A shutdown settles nothing: the worker exits with the message still queued
// and its record stays, so RestoreCutTurns (after the resume of a cut turn
// on the same session) queues it again, driven by UsageHeldDriver like a
// restored held message. A restored copy keeps its id, so it is stored once.
//
// Not stored: injections (each kind has its own durable record, see
// usage_hold_store.go), a held message restored by RestoreUsageHeld (it is
// in usage_held), and a restart's resume line (its cut_turn record brings it
// back). A message folded into a running turn is input of that turn: the
// backend's own record (#2050 on claude-code) or the cut turn's resume
// covers it.

var queuedInputSeq atomic.Uint64

func newQueuedInputID() string {
	return fmt.Sprintf("qi-%d-%d", time.Now().UnixNano(), queuedInputSeq.Add(1))
}

// queuedInputStored reports whether env belongs in the queued-input store.
func queuedInputStored(env Envelope) bool {
	return env.Inject == nil && env.usageHeldID == "" && !env.restartResume && !env.usageResume
}

// storeQueuedInput writes env to its session's queued input, giving it an id
// first. A restored copy (it has an id) is already stored.
func (a *Agent) storeQueuedInput(env *Envelope) {
	if a.SessionIndex == nil || !queuedInputStored(*env) || env.queuedID != "" {
		return
	}
	env.queuedID = newQueuedInputID()
	rec := usageHeldRecordOf(*env)
	rec.ID, rec.Kind = env.queuedID, usageHeldUser
	a.queuedInputMu.Lock()
	defer a.queuedInputMu.Unlock()
	list := a.loadQueuedInputLocked(env.SessionKey)
	a.saveQueuedInputLocked(env.SessionKey, append(list, rec))
}

// forgetQueuedInput drops the stored records of envs, which are settled.
func (a *Agent) forgetQueuedInput(envs ...Envelope) {
	if a.SessionIndex == nil {
		return
	}
	bySession := map[string]map[string]bool{}
	for _, env := range envs {
		if env.queuedID == "" {
			continue
		}
		if bySession[env.SessionKey] == nil {
			bySession[env.SessionKey] = map[string]bool{}
		}
		bySession[env.SessionKey][env.queuedID] = true
	}
	if len(bySession) == 0 {
		return
	}
	a.queuedInputMu.Lock()
	defer a.queuedInputMu.Unlock()
	for sk, ids := range bySession {
		list := a.loadQueuedInputLocked(sk)
		kept := list[:0]
		for _, rec := range list {
			if !ids[rec.ID] {
				kept = append(kept, rec)
			}
		}
		if len(kept) != len(list) {
			a.saveQueuedInputLocked(sk, kept)
		}
	}
}

func (a *Agent) loadQueuedInputLocked(sk string) []usageHeldRecord {
	return a.loadInputRecords(sk, session.MetaKeyQueuedInput, "queued input")
}

func (a *Agent) saveQueuedInputLocked(sk string, list []usageHeldRecord) {
	a.saveInputRecords(sk, session.MetaKeyQueuedInput, "queued input", list)
}

// restoreQueuedInput queues again, in arrival order, the user messages the
// last process accepted on this agent's sessions and never started. Each
// keeps its stored id. Without a driver hook they stay stored.
func (a *Agent) restoreQueuedInput() {
	keys, err := a.SessionIndex.SessionKeysWithMetadata(session.MetaKeyQueuedInput)
	if err != nil {
		a.logger().Warnf("queued input: restore: %v", err)
		return
	}
	for _, sk := range keys {
		if parsed, err := session.ParseSessionKey(sk); err != nil || parsed.AgentID != a.AgentID {
			continue
		}
		if a.UsageHeldDriver == nil {
			a.logger().Warnf("queued input: no driver for %s — kept for the next start", sk)
			continue
		}
		a.queuedInputMu.Lock()
		list := a.loadQueuedInputLocked(sk)
		a.queuedInputMu.Unlock()
		for _, rec := range list {
			env := rec.envelope(sk, a.UsageHeldDriver(sk))
			env.queuedID, env.usageHeldID, env.usageResume = rec.ID, "", false
			a.logger().Infof("queued input: restoring message %s sk=%s (%dB, %d attachment(s))", rec.ID, sk, len(rec.Text), len(rec.Attachments))
			if !a.Enqueue(env) {
				a.logger().Warnf("queued input: inbox refused restored message %s on %s — kept", rec.ID, sk)
			}
		}
	}
}

type queuedInputsKey struct{}

// withQueuedInputs carries the queued messages a turn drives on ctx, so the
// turn settles them once its input reaches the backend (trackCutTurn).
func withQueuedInputs(ctx context.Context, envs []Envelope) context.Context {
	var ids []Envelope
	for _, env := range envs {
		if env.queuedID != "" {
			ids = append(ids, Envelope{SessionKey: env.SessionKey, queuedID: env.queuedID})
		}
	}
	if len(ids) == 0 {
		return ctx
	}
	return context.WithValue(ctx, queuedInputsKey{}, ids)
}

func queuedInputsFromContext(ctx context.Context) []Envelope {
	if ctx == nil {
		return nil
	}
	envs, _ := ctx.Value(queuedInputsKey{}).([]Envelope)
	return envs
}

// settleDrivenQueuedInput settles the queued messages of a batch whose turn
// has returned. During a shutdown it settles nothing: a message whose input
// reached the backend was settled then (trackCutTurn), and one that did not
// (its turn was cut first, or refused a backend while draining) stays
// stored and runs after the restart. Every other end (the turn ran, failed,
// was stopped, or the text answered an ask) settles them.
func (a *Agent) settleDrivenQueuedInput(batch []Envelope) {
	if a.ShuttingDown() {
		return
	}
	a.forgetQueuedInput(batch...)
}

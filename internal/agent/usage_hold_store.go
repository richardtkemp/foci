package agent

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"foci/internal/delegator"
	"foci/internal/platform"
	"foci/internal/session"
)

// Held input survives a restart (#2491 requirement 4, holdout H7).
//
// Everything a usage hold parks on a session is written to the session's
// usage_held metadata (session.MetaKeyUsageHeld) as it is parked, and taken
// off as it runs: the user messages (every text, attachment and sender field
// the turn reads), the hidden resume line of a turn the hold interrupted, and
// the system injections that have a serialisable form (DurableInject). At
// startup RestoreUsageHeld puts each back on its session's inbox, where the
// hold parks it again or, if the hold lifted meanwhile, it runs.
//
// What is NOT kept, and why:
//   - Envelope.Original (the platform's raw message) and Envelope.Driver: they
//     are live platform objects. A restored message gets UsageHeldDriver's
//     driver, which renders to the session's chat as an HTTP send does; the
//     reply lands in the same chat, but not as a threaded reply to the
//     original platform message.
//   - Injections with no DurableInject: their Run is a closure over live
//     state. The ones that matter have their own durable record already: a
//     foci_remind fire keeps its row until it runs (a held fire is refused at
//     shutdown and fires again after the restart), a deferred send keeps its
//     sweep row, and an input the backend never consumed is redelivered
//     (#2050). Keepalive, reflection and the other periodic passes are not
//     input: their schedulers fire them again after the lift.

// DurableInject is the serialisable form of a system injection: enough for
// the gateway to build it again after a restart (Agent.UsageHeldInjectRestore).
// Kind names the gateway path that built it; Args are that path's own
// parameters.
type DurableInject struct {
	Kind    string            `json:"kind"`
	Trigger string            `json:"trigger,omitempty"`
	Text    string            `json:"text"`
	Args    map[string]string `json:"args,omitempty"`
}

// usageHeldKind is what a stored record holds.
const (
	usageHeldUser   = "user"
	usageHeldResume = "resume"
	usageHeldInject = "inject"
)

// usageHeldRecord is one parked input as stored.
type usageHeldRecord struct {
	ID          string                   `json:"id"`
	Kind        string                   `json:"kind"`
	Text        string                   `json:"text,omitempty"`
	Attachments []platform.Attachment    `json:"attachments,omitempty"`
	UserID      string                   `json:"user_id,omitempty"`
	Username    string                   `json:"username,omitempty"`
	SenderName  string                   `json:"sender_name,omitempty"`
	ChatID      int64                    `json:"chat_id,omitempty"`
	IsGroupChat bool                     `json:"group,omitempty"`
	ReceivedAt  time.Time                `json:"received_at"`
	Voice       bool                     `json:"voice,omitempty"`
	Steer       platform.SteerPreference `json:"steer,omitempty"`
	Human       bool                     `json:"human,omitempty"`
	Automated   bool                     `json:"automated,omitempty"`
	Ref         delegator.InputRef       `json:"ref"`
	Inject      *DurableInject           `json:"inject,omitempty"`
}

var usageHeldSeq atomic.Uint64

// newUsageHeldID names one parked input, unique across restarts.
func newUsageHeldID() string {
	return fmt.Sprintf("uh-%d-%d", time.Now().UnixNano(), usageHeldSeq.Add(1))
}

// SetUsageHeldID marks env as the restored copy of parked input id, so
// parking it again neither stores it twice nor tells its sender twice. The
// gateway's inject restore (UsageHeldInjectRestore) calls it.
func (env *Envelope) SetUsageHeldID(id string) { env.usageHeldID = id }

// recordOf is env's stored form.
func usageHeldRecordOf(env Envelope) usageHeldRecord {
	rec := usageHeldRecord{
		ID: env.usageHeldID, Kind: usageHeldUser, Text: env.Text, Attachments: env.Attachments,
		UserID: env.UserID, Username: env.Username, SenderName: env.SenderName, ChatID: env.ChatID,
		IsGroupChat: env.IsGroupChat, ReceivedAt: env.ReceivedAt, Voice: env.Voice, Steer: env.Steer,
		Human: env.Human, Automated: env.Automated, Ref: env.Ref,
	}
	switch {
	case env.usageResume:
		rec.Kind = usageHeldResume
	case env.Inject != nil:
		rec.Kind = usageHeldInject
		rec.Inject = env.Inject.Durable
	}
	return rec
}

// envelope is rec rebuilt as a user envelope on sk, driven by driver.
func (rec usageHeldRecord) envelope(sk string, driver Driver) Envelope {
	return Envelope{
		SessionKey: sk, Text: rec.Text, Attachments: rec.Attachments, UserID: rec.UserID,
		Username: rec.Username, SenderName: rec.SenderName, ChatID: rec.ChatID, IsGroupChat: rec.IsGroupChat,
		ReceivedAt: rec.ReceivedAt, Voice: rec.Voice, Steer: rec.Steer, Human: rec.Human,
		Automated: rec.Automated, Ref: rec.Ref, Driver: driver,
		usageHeldID: rec.ID, usageResume: rec.Kind == usageHeldResume,
	}
}

// storeUsageHeld writes env to sk's held list (replacing a record with its
// id). It gives env an id first when it has none, and returns it.
func (a *Agent) storeUsageHeld(env *Envelope) {
	if env.usageHeldID == "" {
		env.usageHeldID = newUsageHeldID()
	}
	if a.SessionIndex == nil {
		return
	}
	rec := usageHeldRecordOf(*env)
	a.usageHold.storeMu.Lock()
	defer a.usageHold.storeMu.Unlock()
	list := a.loadUsageHeldLocked(env.SessionKey)
	for i := range list {
		if list[i].ID == rec.ID {
			return // already stored (a restored copy parked again)
		}
	}
	if rec.Kind == usageHeldResume {
		list = append([]usageHeldRecord{rec}, list...)
	} else {
		list = append(list, rec)
	}
	a.saveUsageHeldLocked(env.SessionKey, list)
}

// forgetUsageHeld drops the stored records of envs that are about to run.
func (a *Agent) forgetUsageHeld(envs ...Envelope) {
	if a.SessionIndex == nil {
		return
	}
	bySession := map[string]map[string]bool{}
	for _, env := range envs {
		if env.usageHeldID == "" {
			continue
		}
		if bySession[env.SessionKey] == nil {
			bySession[env.SessionKey] = map[string]bool{}
		}
		bySession[env.SessionKey][env.usageHeldID] = true
	}
	if len(bySession) == 0 {
		return
	}
	a.usageHold.storeMu.Lock()
	defer a.usageHold.storeMu.Unlock()
	for sk, ids := range bySession {
		list := a.loadUsageHeldLocked(sk)
		kept := list[:0]
		for _, rec := range list {
			if !ids[rec.ID] {
				kept = append(kept, rec)
			}
		}
		if len(kept) != len(list) {
			a.saveUsageHeldLocked(sk, kept)
		}
	}
}

func (a *Agent) loadUsageHeldLocked(sk string) []usageHeldRecord {
	raw, err := a.SessionIndex.GetSessionMetadata(sk, session.MetaKeyUsageHeld)
	if err != nil || raw == "" {
		return nil
	}
	var list []usageHeldRecord
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		a.logger().Warnf("usage hold: held input of %s unreadable, discarding: %v", sk, err)
		return nil
	}
	return list
}

func (a *Agent) saveUsageHeldLocked(sk string, list []usageHeldRecord) {
	if len(list) == 0 {
		if err := a.SessionIndex.DeleteSessionMetadata(sk, session.MetaKeyUsageHeld); err != nil {
			a.logger().Warnf("usage hold: clear held input of %s: %v", sk, err)
		}
		return
	}
	raw, err := json.Marshal(list)
	if err != nil {
		a.logger().Warnf("usage hold: encode held input of %s: %v", sk, err)
		return
	}
	if err := a.SessionIndex.SetSessionMetadata(sk, session.MetaKeyUsageHeld, string(raw)); err != nil {
		a.logger().Warnf("usage hold: persist held input of %s: %v", sk, err)
	}
}

// RestoreUsageHeld puts back, after a restart, the input a usage hold had
// parked on this agent's sessions: user messages and resume lines onto the
// inbox (driven by UsageHeldDriver), injections through
// UsageHeldInjectRestore. Each keeps its stored id, so parking it again does
// not store it twice. Call once at startup, after the inboxes and platforms
// are up. A session whose record cannot be rebuilt (no driver hook) keeps it
// for the next start.
func (a *Agent) RestoreUsageHeld() {
	if a.SessionIndex == nil {
		return
	}
	keys, err := a.SessionIndex.SessionKeysWithMetadata(session.MetaKeyUsageHeld)
	if err != nil {
		a.logger().Warnf("usage hold: restore held input: %v", err)
		return
	}
	for _, sk := range keys {
		if parsed, err := session.ParseSessionKey(sk); err != nil || parsed.AgentID != a.AgentID {
			continue
		}
		a.usageHold.storeMu.Lock()
		list := a.loadUsageHeldLocked(sk)
		a.usageHold.storeMu.Unlock()
		for _, rec := range list {
			a.restoreUsageHeldOne(sk, rec)
		}
	}
}

func (a *Agent) restoreUsageHeldOne(sk string, rec usageHeldRecord) {
	switch rec.Kind {
	case usageHeldInject:
		if rec.Inject == nil || a.UsageHeldInjectRestore == nil {
			a.logger().Warnf("usage hold: held injection %s on %s cannot be rebuilt — kept", rec.ID, sk)
			return
		}
		a.logger().Infof("usage hold: restoring held %s injection %s sk=%s", rec.Inject.Trigger, rec.ID, sk)
		a.UsageHeldInjectRestore(sk, *rec.Inject, rec.ID)
	default:
		if a.UsageHeldDriver == nil {
			a.logger().Warnf("usage hold: held message %s on %s has no driver — kept", rec.ID, sk)
			return
		}
		env := rec.envelope(sk, a.UsageHeldDriver(sk))
		a.logger().Infof("usage hold: restoring held %s %s sk=%s (%dB, %d attachment(s))", rec.Kind, rec.ID, sk, len(rec.Text), len(rec.Attachments))
		if !a.Enqueue(env) {
			a.logger().Warnf("usage hold: inbox refused restored message %s on %s — kept", rec.ID, sk)
		}
	}
}

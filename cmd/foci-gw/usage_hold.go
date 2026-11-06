package main

import (
	"context"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/platform"
	"foci/internal/route"
	"foci/internal/session"
	"foci/internal/turnevent"
	"foci/internal/usagehold"
)

// usageHoldAgentID is the agent_metadata owner of the account-wide usage hold
// state (#2491): it belongs to an account, not to one agent.
const usageHoldAgentID = "_usage_hold"

// usageHoldMetaPrefix heads the agent_metadata key of one account's state.
const usageHoldMetaPrefix = "usage_hold:"

// usageHoldStore persists usagehold account state in agent_metadata.
type usageHoldStore struct{ idx *session.SessionIndex }

func (s usageHoldStore) LoadUsageHold(account string) (string, bool) {
	v, err := s.idx.GetAgentMetadata(usageHoldAgentID, usageHoldMetaPrefix+account)
	return v, err == nil && v != ""
}

func (s usageHoldStore) SaveUsageHold(account, value string) {
	key := usageHoldMetaPrefix + account
	if value == "" {
		_ = s.idx.DeleteAgentMetadata(usageHoldAgentID, key)
		return
	}
	_ = s.idx.SetAgentMetadata(usageHoldAgentID, key, value)
}

// setupUsageHold wires the process-wide usage hold at startup: its threshold
// from the global [usage_hold] section, and its store.
func setupUsageHold(cfg *config.Config, idx *session.SessionIndex) {
	applyUsageHoldThreshold(cfg)
	if idx != nil {
		usagehold.Default.SetStore(usageHoldStore{idx: idx})
	}
}

// applyUsageHoldThreshold sets the hold threshold from cfg; the live applier
// calls it on an edit of usage_hold.threshold, so the next usage event uses it.
func applyUsageHoldThreshold(cfg *config.Config) {
	pct := usagehold.DefaultThresholdPercent
	if cfg.UsageHold.Threshold != nil {
		pct = *cfg.UsageHold.Threshold
	}
	usagehold.Default.SetThresholdPercent(pct)
}

// wireUsageHeldRestore sets how ag drives the input a usage hold parked when
// no live platform message carries it (#2491): the hidden resume line of an
// interrupted turn, and held input restored after a restart (its platform
// object did not survive). Both run as a user message through
// usageHoldDriver; a held injection goes back through the path that first
// built it, under its stored id.
func wireUsageHeldRestore(ag *agent.Agent, ctx context.Context, connMgr platform.ConnectionManager, agentID string, resolver func(string) *agentInstance, deliverChit chitDeliverer) {
	inst := &agentInstance{id: agentID, ag: ag}
	ag.UsageHeldDriver = func(sk string) agent.Driver {
		return &usageHoldDriver{agentID: agentID, sessionKey: sk, connMgr: connMgr}
	}
	ag.UsageHeldInjectRestore = func(sk string, d agent.DurableInject, id string) {
		switch d.Kind {
		case durableSessionChat:
			deliverToSessionChatThen(ag, ctx, d.Trigger, connMgr, agentID, sk, d.Text, nil, "", nil, withUsageHeldID(id))
		case durableSessionRelay:
			restoreHeldRelay(ag, ctx, connMgr, agentID, resolver, deliverChit, sk, d, id)
		case durableHTTPAsync:
			sendCtx := withUsageHeldIDContext(agent.WithTrigger(ctx, d.Trigger), id)
			if !deliverBufferedQueued(inst, connMgr, sendCtx, sk, d.Text, d.Args["log_tag"], d.Args["silent"] == "true", route.Policy(d.Args["policy"])) {
				mainLog.Warnf("usage hold: inbox refused restored held send %s on %s", id, sk)
			}
		default:
			mainLog.Warnf("usage hold: held injection %s on %s has unknown kind %q — kept", id, sk, d.Kind)
		}
	}
}

// usageHoldResumedBranch is the one periodic branch type a usage hold
// interrupts and resumes (Dick 2026-11-02: "Allow the background turns to
// complete when the gate hits ... Except for the background work on to-dos,
// that must stop and be resumed"). Reflection, consolidation and the
// compactions run to completion (agent.interruptForUsageHold).
const usageHoldResumedBranch = "background"

// The strategies a background-work turn runs by, as a paused record names
// them (agent.PausedBackground.Strategy).
const (
	strategyInPlace     = "in_place"
	strategyFork        = "fork"
	strategyIndependent = "independent"
)

// runHoldResumable runs first on key through run and, for a background-work
// turn, resumes it across a usage hold (#2491, agent.RunBackgroundResumable):
// a turn the hold interrupts pauses (key joins the keepalive warm set, and a
// record keeps it across a restart), waits for the lift, and goes on with the
// hidden resume line, cold cache or not (Dick 2026-11-02: "Accept resuming
// them from cold if that fails"). Any other branch type runs once. The
// scheduler's run flag stays set the whole time, so no second background
// turn starts during the pause.
func runHoldResumable(ag *agent.Agent, ctx context.Context, branchType, key, strategy, first string, run func(context.Context, string) error) error {
	if branchType != usageHoldResumedBranch {
		return run(ctx, first)
	}
	return ag.RunBackgroundResumable(ctx, agent.PausedBackground{Key: key, Strategy: strategy, BranchType: branchType}, first, run)
}

// resumePausedBackground resumes, after a restart, a background-work turn a
// usage hold had paused: at the lift it gets the resume line on its own
// session by the strategy it ran by. A fork's or independent session's
// backend is gone with the old process, so its turn rebuilds it from the
// session's resume id (a cold start, which the ruling accepts), and the
// session is closed afterwards as the original run would have.
func resumePausedBackground(inst *agentInstance, ctx context.Context, pb agent.PausedBackground) {
	ag := inst.ag
	turnCtx := agent.WithTrigger(ctx, pb.BranchType)
	run := func(c context.Context, text string) error {
		return ag.HandleMessage(withContextOf(turnCtx, c), pb.Key, []string{text}, nil)
	}
	if pb.Strategy == strategyInPlace {
		run = func(c context.Context, text string) error {
			var runErr error
			if err := ag.EnqueueInjectWait(c, pb.Key, pb.BranchType, func() {
				runErr = ag.HandleMessage(withContextOf(turnCtx, c), pb.Key, []string{text}, nil)
			}); err != nil {
				return err
			}
			return runErr
		}
	}
	work := func() {
		if err := ag.ResumePausedBackground(ctx, pb, run); err != nil {
			mainLog.Warnf("usage hold: resumed background work on %s: %v", pb.Key, err)
		}
		if pb.Strategy != strategyInPlace && ctx.Err() == nil && ag.DelegatedManager != nil {
			ag.DelegatedManager.ResetSession(pb.Key)
		}
	}
	if inst.kaRunner == nil || !inst.kaRunner.RunBackgroundWork(work) {
		go work()
	}
}

// withContextOf returns base (its values: trigger, sink) ending when either
// base or c ends. A turn built inside an injection keeps the trigger and
// values of the branch while a usage hold can still interrupt it.
func withContextOf(base, c context.Context) context.Context {
	merged, cancel := context.WithCancelCause(base)
	context.AfterFunc(c, func() { cancel(context.Cause(c)) })
	return merged
}

// usageHoldDriver drives a user message foci composes on the user's behalf
// in a usage hold (#2491): the resume line ("simulate a user message saying
// \"resume what you were doing\"", Dick 2026-11-02) and held input restored
// after a restart. Unlike httpDriver, whose nil Connection makes a turn read
// as an external send, it reports the session's own platform connection, so
// the turn gets a user turn's trigger and [meta] via. It renders to that
// connection; the resume line itself stays out of what clients show
// (agent: withHiddenLeadingTexts). The envelope is Automated, so it never
// answers an ask or a login prompt for the user.
type usageHoldDriver struct {
	agentID    string
	sessionKey string
	connMgr    platform.ConnectionManager
}

func (d *usageHoldDriver) conn() platform.Connection {
	if d.connMgr == nil {
		return nil
	}
	conn, _ := route.ConnFor(d.connMgr, d.agentID, d.sessionKey, route.PolicyFallback)
	return conn
}

// Connection implements agent.Driver: the session's platform connection (or
// the agent's primary when the session has none live).
func (d *usageHoldDriver) Connection() platform.Connection { return d.conn() }

// NewTurnSink implements agent.Driver: render to the connection resolved at
// turn time.
func (d *usageHoldDriver) NewTurnSink(env agent.Envelope) (turnevent.Sink, func()) {
	conn := d.conn()
	if conn == nil {
		mainLog.Warnf("usage hold: no connection for session %s — the held turn's reply is not delivered", env.SessionKey)
		return turnevent.NopSink{}, nil
	}
	return rawTurnSinkForConn(conn, env.SessionKey, "usage-hold")
}

// WrapTurn implements agent.Driver.
func (d *usageHoldDriver) WrapTurn(ctx context.Context, fn func() error) error {
	err := fn()
	if err != nil && ctx.Err() == nil {
		mainLog.Errorf("usage hold: held turn on %s: %v", d.sessionKey, err)
	}
	return err
}

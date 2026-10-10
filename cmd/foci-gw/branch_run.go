package main

import (
	"errors"
	"fmt"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/route"
	"foci/internal/session"
	"foci/shared/prompts"
)

// This file is the ONE "create and run a branch" implementation (#1272),
// shared by handleBranch (immediate requests) and the deferred sweep
// (delivery time) — the fork/fresh/fallback ladder, model override, NoCompact
// latch and dispatch exist exactly once.

// branchTurnOptions is the deferrable subset of a branch request: everything
// needed to create the branch session and run its first turn, with no HTTP
// concerns. A deferred branch carries it in its defersend.Record.
type branchTurnOptions struct {
	Text        string
	Model       string
	NoCompact   bool
	NoResetHook bool
	Silent      bool
}

// branchTurnResult reports where the branch turn ran and, for sync runs, the
// agent's reply.
type branchTurnResult struct {
	// Receipt names the session the turn ran on: the new branch session
	// (Via "branch"), or the parent session when the backend cannot branch
	// at all and the request degrades to a send — a fallback only the
	// agent's DEFAULT session may take; any other parent is refused with
	// errBranchUnbranchableParent (#2284). On that fallback the receipt is
	// the caller's resolution receipt.
	Receipt route.Receipt
	// Resp is the turn's final text, set for sync runs only.
	Resp string
}

var (
	// errBranchBadModel wraps a rejected model override so the HTTP handler
	// answers 400 with the resolver's reason.
	errBranchBadModel = errors.New("bad model")
	// errBranchInboxFull reports that the target session's inbox is full
	// (async dispatch refused). The HTTP handler answers 503.
	errBranchInboxFull = errors.New("session inbox full")
	// errBranchUnbranchableParent reports that the agent's backend cannot
	// branch at all AND the resolved parent is not the agent's default
	// session: the cannot-branch fallback would run the turn on — and
	// interrupt — the very session the caller asked to leave alone, so the
	// request is refused before anything runs (#2284). The HTTP handler
	// answers 422; the deferred sweep logs it like any delivery error.
	errBranchUnbranchableParent = errors.New("backend cannot branch this session")
)

// branchSession names the session a branch turn runs on and how it was made.
type branchSession struct {
	// key is the session the turn runs on: the new branch key, or the
	// parent key when the backend cannot branch at all (a fallback only the
	// agent's default session may take — runBranchTurn refuses every other
	// parent with errBranchUnbranchableParent, #2284).
	key string
	// inherited reports that a delegated branch carries the parent's
	// backend conversation (a real fork) rather than starting fresh.
	inherited bool
	// fellBack reports that the backend cannot branch AT ALL, so the
	// request degrades to a send on the parent. Distinct from a fresh
	// branch (a parent with nothing to fork still yields a real branch).
	fellBack bool
}

// createBranchSession creates the session a branch turn runs on, forking the
// parent's state at CALL time (a deferred branch therefore inherits the
// parent's state when it is delivered, not when it was enqueued):
//
//   - delegated agents use Agent.ForkOrFreshBranch — a backend fork of the
//     parent's conversation when possible, else a fresh branch (a parent with
//     no backend session to clone still yields a real branch, just one with
//     nothing inherited), and only a backend that cannot branch at all sets
//     fellBack. A branch is wanted whenever branching is possible AT ALL —
//     conflating "nothing to fork" with "cannot branch" used to send a reset
//     agent's branch INTO its main session and silently drop
//     no_compact/no_reset_hook/silent (#1634).
//   - API agents fork through the session store.
//
// The fellBack shape (a send on the parent) is taken only from the
// agent's DEFAULT session — runBranchTurn refuses every other parent
// (#2284), because that fallback would interrupt the named session the
// caller explicitly asked to leave alone.
//
// NoResetHook and the orientation template are branch-CREATION options, so
// they are resolved here (once — handleBranch used to resolve the template
// twice).
func createBranchSession(d httpHandlerDeps, inst *agentInstance, parentKey string, noResetHook bool) (branchSession, error) {
	orientPath := config.DerefStr(config.First(inst.agentCfg.Sessions.BranchOrientationHeadlessPrompt, d.cfg.Sessions.BranchOrientationHeadlessPrompt))
	orientTemplate := prompts.ResolveOrientationTemplate(orientPath, false, inst.promptSearchDirs...)
	branchOpts := session.BranchOptions{
		NoResetHook:         noResetHook,
		BranchType:          "branch",
		OrientationTemplate: orientTemplate,
	}

	if inst.ag.DelegatedManager != nil {
		branchKey, inherited, err := inst.ag.ForkOrFreshBranch(d.ctx, parentKey, branchOpts)
		if err != nil {
			branchLog.Errorf("agent %q fork error: %v", inst.id, err)
			return branchSession{}, err
		}
		if branchKey != "" {
			return branchSession{key: branchKey, inherited: inherited}, nil
		}
		// Reaching here means ONE thing: this agent's backend does not
		// implement delegator.BackendBrancher, so no branch of any kind is
		// possible. "Parent has nothing to fork" was recovered above. Fall
		// through to /send semantics against the parent.
		return branchSession{key: parentKey, fellBack: true}, nil
	}

	branchKey, err := d.sessions.CreateBranchWithOptions(parentKey, branchOpts)
	if err != nil {
		branchLog.Errorf("branch error: %v", err)
		return branchSession{}, err
	}
	return branchSession{key: branchKey}, nil
}

// runBranchTurn creates the branch session for parentKey (at call time) and
// runs o.Text as its first turn with trigger "branch". fallbackRcpt is the
// receipt used when the backend cannot branch and the request degrades to a
// send on the parent — a fallback allowed only when parentKey IS the agent's
// default session at run time; any other (named) parent is refused with
// errBranchUnbranchableParent before a model override, the NoCompact latch
// or any dispatch happens, so nothing runs and the parent gets no message
// and no turn (#2284). sync selects the run shape, exactly as the two
// handleBranch paths always did: sync runs the turn queued and returns its
// reply; async schedules it on the session's inbox (streaming to the chat
// unless silent) and returns no reply. human marks a request whose caller
// declared a human sent it (#1130): the turn context then carries the
// human-source marker, so the turn's entry write stamps last_user_activity_at
// on the branch session. Deliberately a parameter, NOT a branchTurnOptions
// field — the options are the deferrable subset, and the human flag is
// intentionally NOT deferrable: the deferred sweep always passes false (the
// human may be gone by delivery time).
func runBranchTurn(d httpHandlerDeps, inst *agentInstance, parentKey string, fallbackRcpt route.Receipt, o branchTurnOptions, sync, human bool) (branchTurnResult, error) {
	bs, err := createBranchSession(d, inst, parentKey, o.NoResetHook)
	if err != nil {
		return branchTurnResult{}, err
	}

	// The run key and receipt: the branch session, or the parent + the
	// caller's resolution receipt on the cannot-branch fallback.
	runKey := bs.key
	rcpt := route.Receipt{SessionKey: bs.key, Via: "branch"}
	switch {
	case bs.fellBack:
		if parentKey != defaultSessionKey(d, inst.id) {
			// R1 ruling (option B): "main" is the resolved parent key
			// equalling the agent's default session key at run time. A
			// named parent must never take the send fallback — it would
			// run the turn on, and interrupt, the session the caller
			// asked to leave alone. Refuse before the model override,
			// the NoCompact latch and both dispatch shapes below: the
			// parent gets no message and no turn.
			err := fmt.Errorf("%w: agent %q session %q: the agent's backend cannot branch, and the fallback would run the request on that session — nothing ran",
				errBranchUnbranchableParent, inst.id, parentKey)
			branchLog.Warnf("refused branch on named parent: agent=%s session=%s (backend %q cannot branch)", inst.id, parentKey, inst.ag.Backend)
			return branchTurnResult{}, err
		}
		runKey, rcpt = parentKey, fallbackRcpt
		branchLog.Warnf("agent %q backend %q cannot branch — falling through to send (branching options ignored: no_compact=%v no_reset_hook=%v silent=%v)",
			inst.id, inst.ag.Backend, o.NoCompact, o.NoResetHook, o.Silent)
	case inst.ag.DelegatedManager != nil:
		kind := "fresh branch (parent had no backend session)"
		if bs.inherited {
			kind = "backend fork"
		}
		branchLog.Infof("delegated %s %s from %s, text=%s no_compact=%v async=%v silent=%v",
			kind, bs.key, parentKey, previewForLog(o.Text), o.NoCompact, !sync, o.Silent)
	default:
		branchLog.Infof("branch %s from %s, text=%s no_compact=%v no_reset_hook=%v async=%v silent=%v",
			bs.key, parentKey, previewForLog(o.Text), o.NoCompact, o.NoResetHook, !sync, o.Silent)
	}

	// The model override targets the session the turn actually runs on. The
	// NoCompact latch is a branch-session property — the send fallback never
	// had a branch session to latch, so it ignores it (as it always did).
	if o.Model != "" {
		if err := applyModelOverride(inst, runKey, o.Model, d.cfg.Models); err != nil {
			return branchTurnResult{}, fmt.Errorf("%w: %w", errBranchBadModel, err)
		}
	}
	if o.NoCompact && !bs.fellBack {
		inst.ag.SetSessionNoCompact(bs.key, true)
	}

	branchCtx := agent.WithTrigger(d.ctx, "branch")
	if human {
		branchCtx = agent.WithHumanSource(branchCtx)
	}
	if !sync {
		if !deliverBufferedQueued(inst, d.connMgr, branchCtx, runKey, o.Text, "branch", o.Silent, route.PolicyFallback) {
			return branchTurnResult{}, errBranchInboxFull
		}
		return branchTurnResult{Receipt: rcpt}, nil
	}
	resp, err := runAgentQueued(branchCtx, inst.ag, runKey, o.Text)
	if err != nil {
		branchLog.Errorf("branch turn error: %v", err)
		return branchTurnResult{}, err
	}
	return branchTurnResult{Receipt: rcpt, Resp: resp}, nil
}

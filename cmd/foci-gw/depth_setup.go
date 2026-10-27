package main

// Depth mode wiring (#2588): builds an agent's tools.DepthManager from the
// gateway's pieces (forking, injected turns, the ask router, the ledger, the
// app surface) and registers its four tools. Off unless [depth] enabled, the
// app platform is configured and the backend can fork a session.

import (
	"context"
	"errors"

	"foci/internal/agent"
	"foci/internal/app"
	"foci/internal/config"
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/platform"
	"foci/internal/question"
	"foci/internal/route"
	"foci/internal/tools"
	"foci/internal/turnevent"
	"foci/shared/prompts"
)

// depthEnabled gates the depth_* rows: the experimental switch, an app
// platform to show the feed on, and a backend that can fork (always on the
// API path).
func depthEnabled(d *toolDeps) bool {
	if !d.p.resolved.Depth.Enabled || d.p.cfg == nil || d.p.cfg.Platform("app") == nil { // static-cfg:ignore: decides which tools are registered, once at startup
		return false
	}
	if d.path == pathAPI {
		return true
	}
	s, ok := delegator.SpecFor(d.p.acfg.Backend)
	return ok && s.Supports(delegator.CapBranch)
}

// getDepth builds the path's DepthManager once.
func (d *toolDeps) getDepth() *tools.DepthManager {
	if d.depth != nil {
		return d.depth
	}
	acfg := d.p.acfg
	agentID := acfg.ID
	orientPath := config.DerefStr(config.First(acfg.Sessions.BranchOrientationDepthPrompt, d.p.cfg.Sessions.BranchOrientationDepthPrompt))
	ag := func() *agent.Agent {
		if d.agLazy == nil {
			return nil
		}
		return d.agLazy()
	}
	var store tools.SpawnStore
	if d.p.sessionIndex != nil {
		store = d.p.sessionIndex
	}
	d.depth = tools.NewDepthManager(tools.DepthDeps{
		AgentID:             agentID,
		Store:               store,
		Surface:             app.DepthSurface{},
		Sessions:            &sessionBranchAdapter{store: d.p.sessions, ag: d.agLazy},
		OrientationTemplate: prompts.ResolveDepthOrientationTemplate(orientPath, d.promptSearchDirs...),
		CanBranch: func() bool {
			a := ag()
			return a != nil && (a.DelegatedManager == nil || a.DelegatedManager.BackendCanBranch())
		},
		SetNoCompact: func(sk string, v bool) {
			if a := ag(); a != nil {
				a.SetSessionNoCompact(sk, v)
			}
		},
		RunTurn:       newDepthRunTurn(ag, d.p.ctx, d.connMgr, agentID),
		Notifier:      d.notifier,
		MaxConcurrent: d.p.resolved.Tools.MaxConcurrentSpawns, // static-cfg:ignore: sizes a semaphore at construction, like spawn's
		CancelAsk: func(sk string) {
			r := d.out.askRouter
			if r == nil || r.PendingForSession == nil || r.HandleResponse == nil {
				return
			}
			if reqID := r.PendingForSession(sk); reqID != "" {
				r.HandleResponse(reqID, question.CancelData)
			}
		},
		StopTurn: func(sk string) {
			a := ag()
			if a == nil {
				return
			}
			a.CancelSession(sk)
			if a.DelegatedManager != nil {
				_ = a.DelegatedManager.StopSession(context.Background(), sk)
			}
		},
		CostUSD: func(sk string) (float64, bool) {
			l := accounting.Live()
			if l == nil {
				return 0, false
			}
			st, err := l.SessionStats(sk)
			if err != nil || st == nil {
				return 0, false
			}
			return st.TotalCost, true
		},
		ContextTokens: func(sk string) int {
			l := accounting.Live()
			if l == nil {
				return 0
			}
			st, err := l.SessionStats(sk)
			if err != nil || st == nil {
				return 0
			}
			return st.ContextTokens
		},
		CanResume: func(bk string) bool {
			a := ag()
			return a != nil && a.HasResumableSession(bk)
		},
		Approve: newDepthApproveFn(agentID, d.connMgr),
	})
	app.SetDepthController(tools.DepthDispatch{})
	if d.out != nil {
		d.out.depth = d.depth
	}
	return d.depth
}

// newDepthRunTurn runs a depth-mode turn (a leaf's first turn, the feed's Ack,
// an approval answer) as an injection on the session's inbox, its output
// rendered to the session's own app binding: for a leaf, its feed item.
func newDepthRunTurn(ag func() *agent.Agent, ctx context.Context, connMgr platform.ConnectionManager, agentID string) func(sk, text, trigger string, started func(), done func(bool)) {
	return func(sk, text, trigger string, started func(), done func(bool)) {
		a := ag()
		if a == nil {
			done(false)
			return
		}
		enqueueInject(a, sk, trigger, "", func() {
			if started != nil {
				started()
			}
			var err error
			defer func() { done(!errors.Is(err, agent.ErrShuttingDown)) }()
			turnCtx := agent.WithTrigger(ctx, trigger)
			// Strict: a leaf's output belongs in its own item, never in the
			// agent's default chat.
			if conn, _ := route.ConnFor(connMgr, agentID, sk, route.PolicyStrict); conn != nil {
				sink, cleanup := turnSinkForConn(a, conn, sk, trigger)
				if cleanup != nil {
					defer cleanup()
				}
				turnCtx = turnevent.WithSink(turnCtx, sink)
			}
			if err = a.HandleMessage(turnCtx, sk, []string{text}, nil); err != nil {
				logInjectionError(trigger, err, "depth turn on %s", sk)
			}
		}, onInjectRefused(func() { done(false) }))
	}
}

// newDepthApproveFn shows the level-5+ fan-out approval as a two-button prompt
// in the requesting node's chat (its feed item). An approval left unanswered
// until the prompt expires counts as a denial.
func newDepthApproveFn(agentID string, connMgr platform.ConnectionManager) func(sk, promptID, text string, cb func(bool)) error {
	return func(sk, promptID, text string, cb func(bool)) error {
		resolve := func() platform.Connection {
			c, _ := route.ConnFor(connMgr, agentID, sk, route.PolicyStrict)
			return c
		}
		_, err := platform.SendInteractiveMessageWithID(resolve, promptID, "Depth fan-out", text,
			[]platform.ButtonChoice{{Label: "Approve", Data: "approve"}, {Label: "Deny", Data: "deny"}},
			func(ch platform.ButtonChoice) string {
				ok := ch.Data == "approve"
				go cb(ok)
				if ok {
					return "✅ Fan-out approved"
				}
				return "❌ Fan-out denied"
			},
			func() { go cb(false) })
		return err
	}
}

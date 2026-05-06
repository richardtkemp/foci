package command

import (
	"context"
	"fmt"

	"foci/internal/tools"
)

// StopCommand returns a /stop command that cancels the current agent turn.
func StopCommand() *Command {
	return &Command{
		Name:        "stop",
		Description: "Cancel the current agent turn",
		Category:    "operations",
		Immediate:   true, // must run in polling goroutine to cancel a live turn
		Execute: func(ctx context.Context, _ Request, cc CommandContext) (Response, error) {
			// Delegated mode: send Escape×2 + Ctrl-C to CC's TUI.
			if cc.Agent != nil && cc.Agent.DelegatedManager != nil {
				sk := tools.SessionKeyFromContext(ctx)
				if sk == "" {
					return Response{}, fmt.Errorf("no active session")
				}

				// If there's a pending AskUserQuestion, cancel it
				// instead of stopping the entire CC session.
				if cancelled := cc.Agent.CancelPendingQuestion(ctx, sk); cancelled {
					return Response{Text: "Question cancelled."}, nil
				}

				if err := cc.Agent.DelegatedManager.StopSession(ctx, sk); err != nil {
					return Response{}, fmt.Errorf("stop delegated: %w", err)
				}
				// Cancel foci's per-session turn ctx (TODO #743 — was a
				// single bot.cancelTurn field; now precise per session via
				// Agent.CancelSession).
				cc.Agent.CancelSession(sk)
				return Response{Text: "Stopped."}, nil
			}

			// Traditional mode (API backend): per-session cancel via the
			// inbox.
			if sk := tools.SessionKeyFromContext(ctx); sk != "" && cc.Agent != nil {
				cc.Agent.CancelSession(sk)
			} else if cc.StopFunc != nil {
				// Fallback for callers without a session key in context.
				cc.StopFunc()
			}
			return Response{Text: "Stopped."}, nil
		},
	}
}

// DoneCommand returns a /done command that detaches a secondary bot from its session.
func DoneCommand() *Command {
	return &Command{
		Name:        "done",
		Description: "Detach a secondary bot from its session",
		Category:    "operations",
		Hidden:      true,
		Execute: func(ctx context.Context, _ Request, cc CommandContext) (Response, error) {
			if !cc.IsSecondaryBot {
				return Response{Text: "Nothing to detach — this is the main session."}, nil
			}
			if tools.SessionKeyFromContext(ctx) == "" {
				return Response{Text: "Already idle."}, nil
			}
			if cc.StopFunc != nil {
				cc.StopFunc()
			}
			if cc.ReleaseFunc != nil {
				cc.ReleaseFunc()
			}
			return Response{Text: "Session ended."}, nil
		},
	}
}

// ResetCommand returns a /reset command that clears session history.
//
// Default (`/reset`) saves memories before clearing and refuses while the
// agent is processing — see Agent.ResetSession.
//
// `/reset hard` cancels the in-flight turn (if any), skips memory formation,
// destroys the backend, and rotates the key. Marked Immediate at the
// subcommand level so it runs inline in the polling goroutine and can
// actually cancel a live turn (the worker is blocked while a turn runs).
func ResetCommand() *Command {
	softExec := func(ctx context.Context, _ Request, cc CommandContext) (Response, error) {
		sk := tools.SessionKeyFromContext(ctx)
		if sk == "" {
			return Response{}, fmt.Errorf("no active session to reset")
		}
		if _, err := cc.Agent.ResetSession(ctx, sk); err != nil {
			return Response{}, err
		}
		if cc.Agent.DelegatedManager != nil {
			return Response{Text: "Session reset — memories saved, fresh CC session will start on next message."}, nil
		}
		return Response{Text: "Session cleared."}, nil
	}

	hardExec := func(ctx context.Context, _ Request, cc CommandContext) (Response, error) {
		sk := tools.SessionKeyFromContext(ctx)
		if sk == "" {
			return Response{}, fmt.Errorf("no active session to reset")
		}
		if _, err := cc.Agent.ResetSessionHard(ctx, sk); err != nil {
			return Response{}, err
		}
		return Response{Text: "Session reset (hard) — turn cancelled, no memories saved."}, nil
	}

	cmd := &Command{
		Name:        "reset",
		Description: "Clear session history",
		Category:    "operations",
		Subcommands: []Subcommand{
			{
				Name:        "hard",
				Description: "Reset immediately, cancel in-flight turn, skip memory formation",
				Immediate:   true, // must run inline to cancel a live turn
				Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
					return hardExec(ctx, req, cc)
				},
			},
		},
		DefaultExecute: softExec,
	}
	cmd.buildSubcommandDispatch()
	return cmd
}

// CompactCommand creates a /compact command that triggers manual session compaction.
func CompactCommand() *Command {
	compactExec := func(ctx context.Context, _ Request, cc CommandContext, dryRun bool) (Response, error) {
		sk := tools.SessionKeyFromContext(ctx)
		result, err := cc.Agent.CompactSession(ctx, sk, dryRun)
		if err != nil {
			return Response{}, err
		}
		// Delegated agents: CC owns the session file, so there's no
		// foci-side message count to report.
		if cc.Agent.DelegatedManager != nil {
			return Response{Text: "Context compacted (delegated)."}, nil
		}
		if dryRun {
			return Response{Text: fmt.Sprintf("Dry-run complete — %d messages would be summarised. Summary sent.", result.OldMessageCount)}, nil
		}
		return Response{Text: fmt.Sprintf("Context compacted — %d messages summarised.", result.OldMessageCount)}, nil
	}

	cmd := &Command{
		Name:        "compact",
		Description: "Trigger manual context compaction",
		Category:    "operations",
		Subcommands: []Subcommand{
			{
				Name:        "run",
				Label:       "compact",
				Description: "Run context compaction",
				Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
					return compactExec(ctx, req, cc, false)
				},
			},
			{
				Name:        "dry-run",
				Description: "Preview compaction without applying",
				Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
					return compactExec(ctx, req, cc, true)
				},
			},
		},
		// Bare /compact (no args) runs compaction directly.
		DefaultExecute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			return compactExec(ctx, req, cc, false)
		},
	}
	cmd.buildSubcommandDispatch()
	return cmd
}

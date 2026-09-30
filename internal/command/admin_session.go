package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"foci/internal/agent"
	"foci/internal/delegator"
	"foci/internal/tools"
)

// StopCommand returns a /stop command that cancels the current agent turn.
// An argument picks what to stop instead (#2138, #2140):
//   - "subagents" stops only the running Agent-tool subagents;
//   - "commands" stops only the agent's own shell commands, foreground and
//     background;
//   - "all" stops the subagents, the commands and the turn.
//
// Neither "subagents" nor "commands" ends the turn. Plain /stop ends the turn,
// and on Claude Code that also stops the subagents and the foreground command
// but NOT a background command (verified live, CC 2.1.285, #2140).
func StopCommand() *Command {
	return &Command{
		Name:        "stop",
		Description: "Cancel the current agent turn (or: subagents, commands, all)",
		Category:    "operations",
		Immediate:   true, // must run in polling goroutine to cancel a live turn
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			switch mode := strings.ToLower(strings.TrimSpace(req.Args)); mode {
			case "":
				return stopTurn(ctx, cc, true)
			case "subagents", "commands":
				k := subagentTasks
				if mode == "commands" {
					k = commandTasks
				}
				n, err := stopTasks(ctx, cc, k)
				if err != nil {
					return Response{}, err
				}
				return Response{Text: k.stoppingText(n)}, nil
			case "all":
				// The tasks first, each by its own stop, then the turn: an interrupt
				// sent first would already have stopped the subagents, and their
				// stop count would name tasks that were no longer running. A backend
				// that cannot stop tasks on their own still gets its turn stopped.
				var parts []string
				for _, k := range []taskKind{subagentTasks, commandTasks} {
					n, err := stopTasks(ctx, cc, k)
					if err != nil && !errors.Is(err, k.unsupported) {
						return Response{}, err
					}
					if n > 0 {
						parts = append(parts, k.stoppingText(n))
					}
				}
				turn, err := stopTurn(ctx, cc, false)
				if err != nil {
					return Response{}, err
				}
				return Response{Text: strings.Join(append(parts, turn.Text), " ")}, nil
			default:
				return Response{}, fmt.Errorf("unknown /stop argument %q: use subagents, commands or all", mode)
			}
		},
	}
}

// stopTurn cancels the agent's current turn. With cancelQuestion, a pending
// AskUserQuestion is cancelled INSTEAD of the turn (plain /stop); /stop all
// passes false, since it asked to stop everything.
func stopTurn(ctx context.Context, cc CommandContext, cancelQuestion bool) (Response, error) {
	// Delegated mode: send Escape×2 + Ctrl-C to CC's TUI.
	if cc.Agent != nil && cc.Agent.DelegatedManager != nil {
		sk := tools.SessionKeyFromContext(ctx)
		if sk == "" {
			return Response{}, fmt.Errorf("no active session")
		}

		// If there's a pending AskUserQuestion, cancel it
		// instead of stopping the entire CC session.
		if cancelQuestion {
			if cancelled := cc.Agent.CancelPendingQuestion(ctx, sk); cancelled {
				return Response{Text: "Question cancelled."}, nil
			}
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
}

// taskKind is one kind of task /stop can stop on its own, leaving the turn.
type taskKind struct {
	singular, plural string
	unsupported      error
	stop             func(m *agent.DelegatedManager, ctx context.Context, sessionKey string) (int, error)
}

var (
	subagentTasks = taskKind{"subagent", "subagents", agent.ErrStopSubagentsUnsupported, (*agent.DelegatedManager).StopSubagents}
	commandTasks  = taskKind{"command", "commands", agent.ErrStopCommandsUnsupported, (*agent.DelegatedManager).StopCommands}
)

// stopTasks stops the session's running tasks of one kind without touching
// its turn, returning how many stop requests were sent. Only a delegated
// backend runs them.
func stopTasks(ctx context.Context, cc CommandContext, k taskKind) (int, error) {
	if cc.Agent == nil || cc.Agent.DelegatedManager == nil {
		return 0, nil
	}
	sk := tools.SessionKeyFromContext(ctx)
	if sk == "" {
		return 0, fmt.Errorf("no active session")
	}
	n, err := k.stop(cc.Agent.DelegatedManager, ctx, sk)
	if errors.Is(err, k.unsupported) {
		return 0, err
	}
	if err != nil && n == 0 {
		return 0, fmt.Errorf("stop %s: %w", k.plural, err)
	}
	return n, nil
}

// stoppingText is the reply for n stop requests sent.
func (k taskKind) stoppingText(n int) string {
	switch n {
	case 0:
		return "No " + k.plural + " to stop."
	case 1:
		return "Stopping 1 " + k.singular + "."
	default:
		return fmt.Sprintf("Stopping %d %s.", n, k.plural)
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
		outcome, err := cc.Agent.ResetSession(ctx, sk)
		if err != nil {
			return Response{}, err
		}
		if cc.Agent.DelegatedManager != nil {
			switch outcome {
			case agent.ResetMemoryAlreadySaved:
				return Response{Text: "Session reset — fresh session ready. Memories from the previous session have already been saved."}, nil
			case agent.ResetMemoryReflecting:
				return Response{Text: "Session reset — fresh session ready. Memories from the previous session are being saved in the background."}, nil
			default:
				return Response{Text: "Session reset — fresh session ready."}, nil
			}
		}
		return Response{Text: "Session cleared."}, nil
	}

	hardExec := func(ctx context.Context, _ Request, cc CommandContext) (Response, error) {
		sk := tools.SessionKeyFromContext(ctx)
		if sk == "" {
			return Response{}, fmt.Errorf("no active session to reset")
		}
		if err := cc.Agent.ResetSessionHard(ctx, sk); err != nil {
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
		// Suppress the auto-wired keyboard: bare /reset must execute the soft
		// reset directly via DefaultExecute, not prompt the user to pick from
		// a one-option `[hard]` keyboard. `/reset hard` is still discoverable
		// from /help and the subcommand usage string.
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption { return nil },
	}
	cmd.buildSubcommandDispatch()
	return cmd
}

// CompactCommand creates a /compact command that triggers manual session compaction.
func CompactCommand() *Command {
	compactExec := func(ctx context.Context, _ Request, cc CommandContext, dryRun bool) (Response, error) {
		sk := tools.SessionKeyFromContext(ctx)
		result, err := cc.Agent.CompactSession(ctx, sk, dryRun)
		if errors.Is(err, delegator.ErrCompactionNoBoundary) {
			// Backend declined to compact (e.g. too few messages). Not an
			// error to surface — report the no-op plainly (#1267).
			return Response{Text: "Nothing to compact — session too short."}, nil
		}
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
		// Fallback for non-empty args that match no subcommand (e.g. `/compact foo`):
		// run compaction rather than print usage. Bare `/compact` (no args) does NOT
		// reach here — it shows the run/dry-run confirmation keyboard instead (unlike
		// /reset, which suppresses its keyboard to run directly).
		DefaultExecute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			return compactExec(ctx, req, cc, false)
		},
	}
	cmd.buildSubcommandDispatch()
	return cmd
}

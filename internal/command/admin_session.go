package command

import (
	"context"
	"fmt"
	"os"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/tempdir"
	"foci/internal/platform"
	"foci/internal/tools"
	"foci/shared/prompts"
)

// StopCommand returns a /stop command that cancels the current agent turn.
func StopCommand() *Command {
	return &Command{
		Name:        "stop",
		Description: "Cancel the current agent turn",
		Category:    "operations",
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			if cc.StopFunc != nil {
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

// ResetCommand returns a /reset command that clears session history with memory formation.
func ResetCommand() *Command {
	return &Command{
		Name:        "reset",
		Description: "Clear session history",
		Category:    "operations",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			if cc.Agent.IsProcessing() {
				return Response{}, fmt.Errorf("agent is processing — send stop first, then reset")
			}
			sk := tools.SessionKeyFromContext(ctx)
			if sk == "" {
				return Response{}, fmt.Errorf("no active session to reset")
			}

			// Backend mode: memory formation runs IN the CC session (can't branch),
			// then the CC session is destroyed and a fresh one started.
			if cc.Agent.BackendManager != nil {
				return resetBackendSession(ctx, sk, cc)
			}

			// Traditional mode: fire memory formation as a branch, then rotate.
			resetOrientPath := config.DerefStr(config.First(
				cc.AgentConfig.Sessions.BranchOrientationHeadlessPrompt, cc.Config.Sessions.BranchOrientationHeadlessPrompt,
			))
			orientTemplate := prompts.ResolveOrientationTemplate(resetOrientPath, false, cc.PromptSearchDirs...)
			agent.FireSessionEndMemory(cc.Agent, cc.Sessions, sk, cc.Resolved.MemoryFormation,
				orientTemplate, cc.PromptSearchDirs, ctx, false)
			newKey, err := cc.Sessions.RotateKey(sk)
			if err != nil {
				return Response{}, err
			}
			cc.Agent.RotateSession(sk, newKey)
			cc.Bootstrap.Reload()
			cc.Agent.InvalidateSystemCaches()
			return Response{Text: "Session cleared."}, nil
		},
	}
}

// resetBackendSession handles /reset for backend-managed agents (CC mode).
// Sends memory formation prompt to the live CC session, waits for completion,
// then destroys the CC session and starts fresh.
func resetBackendSession(ctx context.Context, sk string, cc CommandContext) (Response, error) {
	// Notify user — this takes a moment.
	if cc.ConnMgr != nil {
		if conn := cc.ConnMgr.ForSessionOrPrimary(sk, cc.AgentConfig.ID); conn != nil {
			_ = platform.SendText(conn, "⏳ Session reset in progress — writing memories, please wait...")
		}
	}

	// Send memory formation prompt to the live CC session.
	if cc.Resolved.MemoryFormation.SessionEndEnabled {
		prompt := prompts.ResolvePrompt(
			cc.Resolved.MemoryFormation.SessionEndPrompt,
			"memory-formation.md", prompts.MemoryFormation(),
			cc.PromptSearchDirs...)
		if prompt != "" {
			log.Infof("reset", "sending memory formation to backend session %s", sk)
			if _, err := cc.Agent.HandleMessage(ctx, sk, prompt); err != nil {
				log.Warnf("reset", "memory formation failed for %s: %v", sk, err)
			}
		}
	}

	// Close the CC session WITHOUT saving resume ID (fresh start).
	cc.Agent.BackendManager.ResetSession(sk)

	// Rotate foci session key, reload, invalidate caches.
	newKey, err := cc.Sessions.RotateKey(sk)
	if err != nil {
		return Response{}, err
	}
	cc.Agent.RotateSession(sk, newKey)
	cc.Bootstrap.Reload()
	cc.Agent.InvalidateSystemCaches()

	return Response{Text: "Session reset — memories saved, fresh CC session will start on next message."}, nil
}

// CompactCommand creates a /compact command that triggers manual session compaction.
func CompactCommand() *Command {
	compactExec := func(ctx context.Context, _ Request, cc CommandContext, dryRun bool) (Response, error) {
		if cc.Agent.Compactor == nil {
			return Response{}, fmt.Errorf("compaction is not configured")
		}
		sk := tools.SessionKeyFromContext(ctx)
		oldCount, err := runCompaction(ctx, cc, sk, dryRun)
		if err != nil {
			return Response{}, err
		}
		if dryRun {
			return Response{Text: fmt.Sprintf("Dry-run complete — %d messages would be summarised. Summary sent.", oldCount)}, nil
		}
		return Response{Text: fmt.Sprintf("Context compacted — %d messages summarised.", oldCount)}, nil
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

// runCompaction executes manual context compaction.
// Caller must verify cc.Agent.Compactor != nil before calling.
func runCompaction(ctx context.Context, cc CommandContext, sk string, dryRun bool) (int, error) {
	if sk == "" {
		return 0, fmt.Errorf("no active session to compact")
	}
	mc, _ := cc.Sessions.MessageCount(sk)
	if mc < 5 {
		return 0, fmt.Errorf("too few messages to compact (%d)", mc)
	}
	if dryRun {
		for _, fn := range cc.Agent.CompactionStartFunc {
			fn(sk, "⏳ Running compaction dry-run...")
		}
	} else {
		for _, fn := range cc.Agent.CompactionStartFunc {
			fn(sk, "⏳ Compacting context...")
		}
	}

	system := cc.Bootstrap.SystemBlocks()
	summaryPrompt := prompts.ResolvePrompt(cc.Agent.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), cc.PromptSearchDirs...)
	handoffMsg := cc.Agent.CompactionHandoffMsg
	if handoffMsg == "" {
		handoffMsg = prompts.ResolvePrompt("", "compaction-handoff.md", prompts.CompactionHandoff(), cc.PromptSearchDirs...)
	}

	compactClient, compactModel, compactFormat := cc.Agent.ResolveCallSite(config.CallCompaction, sk)
	summary, newKey, err := cc.Agent.Compactor.Compact(ctx, compactClient, sk, compactModel, compactFormat, system, summaryPrompt, handoffMsg, dryRun)
	if err != nil {
		return 0, fmt.Errorf("compaction failed: %w", err)
	}

	if dryRun {
		if len(cc.Agent.CompactionDebugFunc) > 0 && summary != "" {
			for _, fn := range cc.Agent.CompactionDebugFunc {
				fn(sk, summary)
			}
		} else if summary != "" {
			if cc.ConnMgr != nil {
				if conn := cc.ConnMgr.Primary(cc.AgentConfig.ID); conn != nil {
					f, tmpErr := os.CreateTemp(tempdir.Dir(), "compaction-dryrun-*.md")
					if tmpErr == nil {
						if _, writeErr := f.WriteString(summary); writeErr == nil {
							_ = f.Close()
							if sendErr := conn.SendDocument(f.Name()); sendErr != nil {
								log.Warnf("agent", "dry-run: send document: %v", sendErr)
							}
						} else {
							_ = f.Close()
						}
						_ = os.Remove(f.Name())
					}
				}
			}
		}
		for _, fn := range cc.Agent.CompactionNotifyFunc {
			fn(sk, "✅ Dry-run complete — summary sent.")
		}
	} else {
		if newKey != "" {
			cc.Agent.RotateSession(sk, newKey)
		}
		for _, fn := range cc.Agent.CompactionNotifyFunc {
			fn(sk, fmt.Sprintf("✅ Context compacted — %d messages summarised.", mc))
		}
		if summary != "" {
			for _, fn := range cc.Agent.CompactionDebugFunc {
				fn(sk, summary)
			}
		}
		cc.Bootstrap.Reload()
		cc.Agent.InvalidateSystemCaches()
		resetKey := sk
		if newKey != "" {
			resetKey = newKey
		}
		cc.Agent.ResetCacheBaseline(resetKey)
	}
	return mc, nil
}

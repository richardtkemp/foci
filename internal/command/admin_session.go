package command

import (
	"context"
	"fmt"
	"os"
	"strings"

	"foci/internal/agent"
	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/tempdir"
	"foci/prompts"
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
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			if !cc.IsSecondaryBot {
				return Response{Text: "Nothing to detach — this is the main session."}, nil
			}
			sk := cc.DefaultSessionKey()
			if sk == "" {
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
		Execute: func(ctx context.Context, _ Request, cc CommandContext) (Response, error) {
			if cc.Agent.IsProcessing() {
				return Response{}, fmt.Errorf("agent is processing — send stop first, then reset")
			}
			sk := cc.DefaultSessionKey()
			if sk == "" {
				return Response{}, fmt.Errorf("no active session to reset")
			}
			resetOrientPath := prompts.ResolveOrientPath(
				cc.AgentConfig.BranchOrientationHeadlessPrompt, cc.Config.Sessions.BranchOrientationHeadlessPrompt,
			)
			agent.FireSessionEndMemory(cc.Agent, cc.Sessions, sk, cc.AgentConfig.MemoryFormation, func(bk, pk, bt string) string {
				return prompts.BuildBranchOrientation(resetOrientPath, bk, pk, bt, false, cc.PromptSearchDirs)
			}, cc.PromptSearchDirs, ctx, false)
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

// CompactCommand creates a /compact command that triggers manual session compaction.
func CompactCommand() *Command {
	return &Command{
		Name:        "compact",
		Description: "Trigger manual context compaction",
		Category:    "operations",
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{
				{Label: "compact", Data: "run"},
				{Label: "dry-run", Data: "dry-run"},
			}
		},
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			dryRun := strings.TrimSpace(req.Args) == "dry-run"
			oldCount, err := runCompaction(ctx, cc, dryRun)
			if err != nil {
				return Response{}, err
			}
			if dryRun {
				return Response{Text: fmt.Sprintf("Dry-run complete — %d messages would be summarised. Summary sent.", oldCount)}, nil
			}
			return Response{Text: fmt.Sprintf("Context compacted — %d messages summarised.", oldCount)}, nil
		},
	}
}

// runCompaction executes manual context compaction.
func runCompaction(ctx context.Context, cc CommandContext, dryRun bool) (int, error) {
	if cc.Agent.Compactor == nil {
		return 0, fmt.Errorf("compaction is not configured")
	}
	sk := cc.DefaultSessionKey()
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

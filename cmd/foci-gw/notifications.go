package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/startup"
	"foci/shared/prompts"
)

// checkDelegatedReadiness probes each delegated agent's backend readiness at
// startup and triggers recovery (re-login) for any that are not ready.
//
// Backends are created lazily per-session, so none exists yet at startup; the
// probe builds a throwaway backend via the manager's NewBackend factory, which
// carries the same claude_binary config and onAuthFailure (re-login) wiring the
// real per-session backend will. For ccstream this runs `claude auth status`
// and, if not authenticated, fires the interactive re-login flow (whose gate
// then pauses delegated message processing on its own). cctmux backends report
// ready unconditionally; API agents (no DelegatedManager) are skipped.
//
// Probes run concurrently but the pass waits for all to settle before
// returning, so a not-authenticated agent's re-login gate is reliably active
// before handleRestartAndFirstRun injects any startup turns. The per-probe
// auth-status check is near-instant; its 15s timeout only bites on a wedged
// binary.
func checkDelegatedReadiness(ctx context.Context, agents map[string]*agentInstance, agentOrder []string) {
	var wg sync.WaitGroup
	for _, agentID := range agentOrder {
		inst := agents[agentID]
		dm := inst.ag.DelegatedManager
		if dm == nil || dm.NewBackend == nil {
			continue // API agent, or no backend factory wired
		}
		// opencode's CheckReady guards on b.server (populated only by
		// Start) and the probe deliberately never calls Start — so the
		// probe would always log "backend has no server" WARN for every
		// opencode-backed agent at every restart. The probe's value for
		// opencode is near-zero anyway: /global/health doesn't surface
		// per-provider auth state, so it can't catch a missing key the
		// way `claude auth status` does for ccstream. Skip it; real
		// auth failures surface on the first turn.
		if inst.agentCfg.Backend == "opencode" {
			continue
		}
		wg.Add(1)
		agentID := agentID
		go func() {
			defer wg.Done()
			be, err := dm.NewBackend()
			if err != nil {
				log.Warnf("main", "[%s] readiness probe: build backend: %v", agentID, err)
				return
			}
			ready, err := be.CheckReady(ctx)
			switch {
			case err != nil:
				log.Warnf("main", "[%s] readiness check could not be performed: %v", agentID, err)
			case ready:
				log.Infof("main", "[%s] backend ready", agentID)
			default:
				log.Warnf("main", "[%s] backend not ready — recovery initiated (see relogin logs)", agentID)
			}
		}()
	}
	wg.Wait()
}

// systemInjectionTargets resolves where agent-less system injections land.
// With a master agent configured: the master receives both the
// welcome/changelog and the (only) restart-context injection. Without one:
// the first agent gets the changelog and every agent gets a restart
// injection (restartOnly == "" means "no restriction").
func systemInjectionTargets(masterAgent string, agentOrder []string) (welcomeTarget, restartOnly string) {
	if masterAgent != "" {
		return masterAgent, masterAgent
	}
	if len(agentOrder) > 0 {
		welcomeTarget = agentOrder[0]
	}
	return welcomeTarget, ""
}

// handleRestartAndFirstRun delivers restart notifications (with optional
// welcome/changelog content) and first-run onboarding prompts.
//
// Restart notifications are delivered as proper agent turns via HandleMessage,
// so the session maintains role alternation (user→assistant). The welcome file
// content, if present, is included in the same turn as the restart notification
// for the primary agent.
func handleRestartAndFirstRun(
	agents map[string]*agentInstance,
	agentOrder []string,
	sessionIndex *session.SessionIndex,
	cfg *config.Config,
	ctx context.Context,
	connMgr platform.ConnectionManager,
	diagnosis *startup.DiagnosisResult,
) {
	// Read and consume the welcome/changelog file (written by setup.sh on update).
	welcomeContent := readAndConsumeWelcomeFile(cfg.WelcomeFile)

	// System injections not addressed to a specific agent route to the
	// configured master agent when one is set: the restart-context turn goes
	// ONLY to the master, which also receives the welcome/changelog.
	// Config-validated, so a set master always exists.
	welcomeTarget, restartOnly := systemInjectionTargets(cfg.MasterAgent, agentOrder)

	needsRestart := diagnosis != nil &&
		diagnosis.Class != startup.ClassClean &&
		diagnosis.Class != startup.ClassUnknown

	// Platform-level notification: short "restarted at ..." message to the user.
	if needsRestart {
		for _, id := range agentOrder {
			inst := agents[id]
			for _, conn := range connMgr.AllForAgent(id) {
				if !inst.resolved.PlatformNotify(conn.PlatformName()).StartupNotify {
					continue
				}
				name := conn.Username()
				if name == "" {
					name = "foci"
				}
				text := fmt.Sprintf("%s restarted at %s", name, time.Now().Format("15:04:05"))
				if extra := diagnosis.FormatNotification(); extra != "" {
					text += "\n\n" + extra
				}
				conn.SendNotification(text)
			}
		}
	}

	// Session-level injection: restart context as a proper agent turn.
	for _, agentID := range agentOrder {
		agentWelcome := ""
		if agentID == welcomeTarget {
			agentWelcome = welcomeContent
		}

		inst := agents[agentID]

		// Respect startup_notify config: skip restart injection if all
		// platform connections for this agent have it disabled.
		agentNeedsRestart := needsRestart && (restartOnly == "" || agentID == restartOnly)
		if agentNeedsRestart {
			hasStartupNotify := false
			for _, conn := range connMgr.AllForAgent(agentID) {
				if inst.resolved.PlatformNotify(conn.PlatformName()).StartupNotify {
					hasStartupNotify = true
					break
				}
			}
			if !hasStartupNotify {
				agentNeedsRestart = false
			}
		}

		if !agentNeedsRestart && agentWelcome == "" {
			continue // nothing to inject for this agent
		}
		agentID := agentID
		go func() {
			sk := defaultSessionKeyFor(inst.ag, agentID)
			if sk == "" {
				log.Warnf("main", "[%s] no active session for restart injection, skipping", agentID)
				return
			}

			tag := "SYSTEM RESTART"
			var body string
			switch {
			case agentNeedsRestart && agentWelcome != "":
				// Both restart + welcome: combine into one message
				body = agentWelcome + "\n\n---\n" + diagnosis.Summary
			case agentNeedsRestart:
				body = diagnosis.Summary
			default:
				// Welcome-only (clean restart with code update)
				tag = "SYSTEM UPDATE"
				body = agentWelcome
			}

			msg := prompts.FormatInjectedMessage(tag, time.Now(), body)
			deliverToSessionChat(inst.ag, ctx, "restart", connMgr, agentID, sk, msg)
		}()
	}

	// First-run onboarding — store the prompt so it gets prepended to the first
	// turn that builds a message (API or claude-code). We mark
	// first_run_completed only when the message is ACTUALLY consumed
	// (OnFirstRunConsumed), not on generic activity: a generic OnActivity
	// callback fired on any first turn, including internal ones that never
	// delivered the onboarding, marking it done while it was still pending —
	// silently losing onboarding on claude-code agents (#853).
	for _, agentID := range agentOrder {
		inst := agents[agentID]
		if msg := checkFirstRun(sessionIndex, inst.agentCfg); msg != "" {
			inst.ag.FirstRunMessage.Store(msg)
			agentID := agentID
			inst.ag.OnFirstRunConsumed = func() {
				if err := sessionIndex.SetAgentMetadata(agentID, "first_run_completed", "true"); err != nil {
					log.Errorf("main", "set first_run_completed for %s: %v", agentID, err)
				}
				log.Infof("main", "first-run onboarding completed for %s", agentID)
			}
		}
	}
}

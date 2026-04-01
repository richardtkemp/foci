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

	// Deliver restart notification to each agent's default session.
	// The welcome content is included only for the primary (first) agent.
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
	for i, agentID := range agentOrder {
		isPrimary := i == 0
		agentWelcome := ""
		if isPrimary {
			agentWelcome = welcomeContent
		}

		inst := agents[agentID]

		// Respect startup_notify config: skip restart injection if all
		// platform connections for this agent have it disabled.
		agentNeedsRestart := needsRestart
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
			sk := mostRecentSessionKey(inst.ag, connMgr, agentID)
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
			deliverInjectedTurn(inst.ag, ctx, "restart", connMgr, agentID, sk, msg)
		}()
	}

	// First-run onboarding — store the prompt so it gets prepended to the
	// first inbound message as a separate content block. The agent clears it
	// after consumption, and we mark first_run_completed via OnActivity.
	for _, agentID := range agentOrder {
		inst := agents[agentID]
		if msg := checkFirstRun(sessionIndex, inst.agentCfg); msg != "" {
			inst.ag.FirstRunMessage.Store(msg)
			agentID := agentID
			var once sync.Once
			inst.ag.OnActivity.Add(func(_ string) {
				once.Do(func() {
					if err := sessionIndex.SetAgentMetadata(agentID, "first_run_completed", "true"); err != nil {
						log.Errorf("main", "set first_run_completed for %s: %v", agentID, err)
					}
					log.Infof("main", "first-run onboarding completed for %s", agentID)
				})
			})
		}
	}
}

package main

import (
	"foci/internal/session"
	"foci/shared/prompts"
)

// First-run import (#3049): the first agent on a NEW install gets extra
// onboarding instructions to find the human's rules in other AI tools (Claude
// Code, Cursor, Codex, ...) and offer them through one ask. Exactly one agent
// per install gets it, so the decision is an install-level marker in the
// session index's system_state, not the per-agent first_run_completed flag.
//
// Marker values:
//   - absent: this code has not run on the install yet
//   - firstRunImportPending: a new install; the next onboarding delivered takes it
//   - firstRunImportExisting: the install had onboarded an agent before this
//     code first ran (ruling 5, "new installs only"); nobody ever gets it
//   - firstRunImportDelivered+<agentID>: delivered once; nobody gets it again
//
// The claim happens when the onboarding is actually consumed into a turn
// (Agent.FirstRunAddendum), the same moment first_run_completed is written. So
// a restart before delivery leaves the marker pending and the import is still
// delivered, and concurrent first turns of several fresh agents race on one
// SQL compare-and-swap that exactly one wins.
const (
	firstRunImportKey       = "first_run_import"
	firstRunImportPending   = "pending"
	firstRunImportExisting  = "existing_install"
	firstRunImportDelivered = "delivered:"
)

// initFirstRunImportMarker records, once per install, whether the install may
// still give the import: pending when no agent has ever completed onboarding,
// existing_install otherwise. It is idempotent (insert-if-absent) and must run
// before any onboarding of this process is consumed; armFirstRun calls it
// first. On a read error it writes nothing, which leaves the import off.
func initFirstRunImportMarker(idx *session.SessionIndex) {
	if idx == nil {
		return
	}
	cur, err := idx.GetSystemState(firstRunImportKey)
	if err != nil {
		mainLog.Errorf("first-run import: read install marker: %v", err)
		return
	}
	if cur != "" {
		return
	}
	existing, err := idx.AnyAgentMetadataEquals("first_run_completed", "true")
	if err != nil {
		mainLog.Errorf("first-run import: check for onboarded agents: %v", err)
		return
	}
	val := firstRunImportPending
	if existing {
		val = firstRunImportExisting
	}
	if _, err := idx.SwapSystemState(firstRunImportKey, "", val); err != nil {
		mainLog.Errorf("first-run import: write install marker: %v", err)
		return
	}
	mainLog.Infof("first-run import: install marker set to %q", val)
}

// claimFirstRunImport moves the install marker from pending to delivered for
// agentID and reports whether this call won it.
func claimFirstRunImport(idx *session.SessionIndex, agentID string) bool {
	if idx == nil {
		return false
	}
	ok, err := idx.SwapSystemState(firstRunImportKey, firstRunImportPending, firstRunImportDelivered+agentID)
	if err != nil {
		mainLog.Errorf("first-run import: claim for %s: %v", agentID, err)
		return false
	}
	if ok {
		mainLog.Infof("first-run import: delivered to %s", agentID)
	}
	return ok
}

// firstRunImportAddendum is the Agent.FirstRunAddendum hook armFirstRun sets:
// on the consume that delivers an agent's onboarding, it claims the install's
// import and, on a win, returns the import instructions with the ask tool
// named as this agent sees it.
func firstRunImportAddendum(idx *session.SessionIndex, agentID string, delegated func() bool) func() string {
	return func() string {
		if !claimFirstRunImport(idx, agentID) {
			return ""
		}
		askTool := "ask"
		if delegated() {
			askTool = "foci_ask"
		}
		return prompts.FirstRunImport(askTool)
	}
}

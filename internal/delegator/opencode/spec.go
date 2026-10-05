package opencode

import (
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
)

// backendName is the [agents].backend value this package registers.
const backendName = "opencode"

// spec declares the OpenCode backend: what it can do and its static data
// (#2154, internal/delegator/capabilities.go).
var spec = delegator.Spec{
	Name:        backendName,
	DisplayName: "OpenCode",
	New:         newFromConfig,
	Prototype:   (*Backend)(nil),
	Wizard:      true,

	Caps: map[delegator.Capability]delegator.Support{
		delegator.CapPostToolNudge:  delegator.No("opencode exposes no per-tool point to inject a nudge at"),
		delegator.CapPreAnswerNudge: delegator.No("the onSessionIdle re-dispatch exists but is unverified live and drops sendPrompt's error (#2176)"),
		delegator.CapStreaming:      delegator.Yes(),

		delegator.CapControl:               delegator.Yes(),
		delegator.CapControlModel:          delegator.Yes(),
		delegator.CapControlEffort:         delegator.No("opencode has no effort setting; SendControl accepts and ignores the request"),
		delegator.CapControlPermissionMode: delegator.Yes(),
		delegator.CapThinkingControl:       delegator.No("the session thinking setting is read only by the API transport"),
		delegator.CapModelResolve:          delegator.No("model names pass through to opencode unresolved"),
		delegator.CapVoiceMode:             delegator.No("opencode has no effort setting to lower"),

		delegator.CapCompactionWait:      delegator.Yes(),
		delegator.CapCompactionStartWait: delegator.Yes(),
		delegator.CapCompactionSummary:   delegator.Yes(),

		delegator.CapActivity:       delegator.Yes(),
		delegator.CapAutonomousRuns: delegator.No("does not track background work"),
		delegator.CapTurnAdoption:   delegator.NotApplicable("no autonomous runs to adopt"),
		delegator.CapStopSubagents:  delegator.No("no API to stop a running subagent"),
		delegator.CapStopCommands:   delegator.No("no API to stop a background command"),
		delegator.CapSubagentStatus: delegator.Yes(),

		delegator.CapPermissionResponse: delegator.Yes(),
		delegator.CapPermissionRules:    delegator.No("\"always\" is sent as opencode's own remember flag, not a foci rule"),
		delegator.CapQuestions:          delegator.Yes(),
		delegator.CapElicitation:        delegator.No("opencode does not surface MCP elicitation requests"),
		delegator.CapPlanPermission:     delegator.No("plan approval is not a permission request on opencode"),

		delegator.CapFoldAttachments:  delegator.No("a message folded into a running turn carries text only"),
		delegator.CapDeliveryTracking: delegator.No("inputs are fire-and-forget: no proof of consumption"),
		delegator.CapThreadNaming:     delegator.No("foci does not take opencode's session titles"),
		delegator.CapContextWindow:    delegator.Yes(),

		delegator.CapBranch:        delegator.Yes(),
		delegator.CapScopedCleanup: delegator.Yes(),

		delegator.CapUnstartedReadinessProbe:  delegator.No("CheckReady needs the server that only Start creates"),
		delegator.CapPreToolRules:             delegator.No("opencode has no PreToolUse hook"),
		delegator.CapStopRules:                delegator.No("opencode has no Stop hook"),
		delegator.CapRelogin:                  delegator.No("the re-login driver logs Claude Code in"),
		delegator.CapCommandApprovalAllowlist: delegator.Yes(),
		delegator.CapPlanMode:                 delegator.Yes(),
		delegator.CapUsageQuery:               delegator.No("no plan usage to report"),
	},

	// DefaultModel is empty: opencode takes its model from opencode.json
	// (TODO #1163).
	ModelcapsKey: backendName,
	LedgerKey:    accounting.BackendOpencode,
	ConfigFamily: delegator.ConfigFamilyOpencode,

	PlanDelivery: planDelivery,

	OnGatewayStart:    func() { ReapOrphanedServers() },
	OnGatewayShutdown: CloseAllServers,
}

func init() { delegator.Register(spec) }

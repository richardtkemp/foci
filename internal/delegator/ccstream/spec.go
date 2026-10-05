package ccstream

import (
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelcaps"
)

// backendName is the [agents].backend value this package registers.
const backendName = "claude-code"

const (
	// batchDefaultModel: a batch run that names no model (consolidation, nudge
	// extraction) runs on sonnet rather than the agent's own, usually larger,
	// model.
	batchDefaultModel = "sonnet"
	// batchCheapModel: a batch that asks for the cheap tier (foci_summary, the
	// /prompts diff summary) runs on haiku.
	batchCheapModel = "haiku"
)

// spec declares the Claude Code backend: what it can do and its static data
// (#2154, internal/delegator/capabilities.go).
var spec = delegator.Spec{
	Name:        backendName,
	DisplayName: "Claude Code",
	New:         newFromConfig,
	Prototype:   (*Backend)(nil),
	Wizard:      true,

	Caps: map[delegator.Capability]delegator.Support{
		delegator.CapPostToolNudge:  delegator.Yes(),
		delegator.CapPreAnswerNudge: delegator.Yes(),
		delegator.CapStreaming:      delegator.Yes(),

		delegator.CapControl:               delegator.Yes(),
		delegator.CapControlModel:          delegator.Yes(),
		delegator.CapControlEffort:         delegator.Yes(),
		delegator.CapControlPermissionMode: delegator.Yes(),
		delegator.CapThinkingControl:       delegator.No("the session thinking setting is read only by the API transport"),
		delegator.CapModelResolve:          delegator.No("CC resolves model aliases itself"),
		delegator.CapVoiceMode:             delegator.Yes(),

		delegator.CapCompactionWait:      delegator.Yes(),
		delegator.CapCompactionStartWait: delegator.Yes(),
		delegator.CapCompactionSummary:   delegator.Yes(),

		delegator.CapActivity:       delegator.Yes(),
		delegator.CapAutonomousRuns: delegator.Yes(),
		delegator.CapTurnAdoption:   delegator.Yes(),
		delegator.CapStopSubagents:  delegator.Yes(),
		delegator.CapStopCommands:   delegator.Yes(),
		delegator.CapSubagentStatus: delegator.Yes(),

		delegator.CapPermissionResponse: delegator.Yes(),
		delegator.CapPermissionRules:    delegator.Yes(),
		delegator.CapQuestions:          delegator.Yes(),
		delegator.CapElicitation:        delegator.Yes(),
		delegator.CapPlanPermission:     delegator.Yes(),

		delegator.CapFoldAttachments:  delegator.Yes(),
		delegator.CapDeliveryTracking: delegator.Yes(),
		delegator.CapThreadNaming:     delegator.No("CC does not name its sessions; agents get the set_session_alias tool instead"),
		delegator.CapContextWindow:    delegator.Yes(),

		delegator.CapBranch:        delegator.Yes(),
		delegator.CapScopedCleanup: delegator.No("fork and cleanup are local transcript file operations, no server needed"),

		delegator.CapHostHooks:                delegator.Yes(),
		delegator.CapUnstartedReadinessProbe:  delegator.Yes(),
		delegator.CapPreToolRules:             delegator.Yes(),
		delegator.CapStopRules:                delegator.Yes(),
		delegator.CapRelogin:                  delegator.Yes(),
		delegator.CapCommandApprovalAllowlist: delegator.Yes(),
		delegator.CapPlanMode:                 delegator.Yes(),
		delegator.CapUsageQuery:               delegator.Yes(),
	},

	DefaultModel:       "opus",
	ModelcapsKey:       modelcaps.BackendCCStream,
	LedgerKey:          accounting.BackendCCStream,
	ClosesTurnActivity: true,
	CacheTTL:           ccStreamCacheTTL,
	BatchDefaultModel:  batchDefaultModel,
	BatchCheapModel:    batchCheapModel,
	ConfigFamily:       delegator.ConfigFamilyClaudeCode,

	TranscriptChecker: InputInTranscript,
	ResumeRetention:   CleanupPeriod,
	PlanDelivery:      planDelivery,
	UsageQuery:        QueryUsage,
}

func init() { delegator.Register(spec) }

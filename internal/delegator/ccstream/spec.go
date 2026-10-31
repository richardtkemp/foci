package ccstream

import (
	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelcaps"
)

// backendName is the [agents].backend value this package registers.
const backendName = "claude-code"

// displayName is the backend's human name (Spec.DisplayName). A constant so
// QueryUsage can name its backend without referring to spec, which refers
// to QueryUsage (an initialization cycle).
const displayName = "Claude Code"

// spec declares the Claude Code backend: what it can do and its static data
// (#2154, internal/delegator/capabilities.go).
var spec = delegator.Spec{
	Name:        backendName,
	DisplayName: displayName,
	New:         newFromConfig,
	Prototype:   (*Backend)(nil),
	PermissionModes: []delegator.PermissionMode{
		{Name: "normal", Value: "default", Description: "default", Aliases: []string{"default"}},
		{Name: "accept", Value: "acceptEdits", Description: "auto-accept edits", Aliases: []string{"edits"}},
		{Name: "plan", Value: "plan", Description: "read-only"},
		{Name: "auto", Value: "auto", Description: "classifier decides"},
	},
	Wizard: true,

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
		delegator.CapQuestionPrompt:     delegator.Yes(),

		delegator.CapFoldAttachments:  delegator.Yes(),
		delegator.CapDeliveryTracking: delegator.Yes(),
		delegator.CapThreadNaming:     delegator.No("CC does not name its sessions; agents get the set_session_alias tool instead"),
		delegator.CapContextWindow:    delegator.Yes(),
		delegator.CapLiveModel:        delegator.Yes(),

		delegator.CapBranch:        delegator.Yes(),
		delegator.CapScopedCleanup: delegator.No("fork and cleanup are local transcript file operations, no server needed"),
		delegator.CapIsolatedBatch: delegator.Yes(),
		delegator.CapReadOnlyBatch: delegator.Yes(),

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
	ConfigFamily:       delegator.ConfigFamilyClaudeCode,
	ConfigKeys:         []string{"allowed_tools", "binary", "skip_permissions"},
	StartFields: []string{
		"WorkDir", "SystemPrompt", "Model", "AgentID", "Label", "ResumeSessionID", "SessionKey",
		"SkipPermissions", "BatchAccess", "Env", "AutoApproveRules", "SubagentMaxAge", "Effort",
	},

	TranscriptChecker: InputInTranscript,
	ResumeRetention:   CleanupPeriod,
	PlanDelivery:      planDelivery,
	UsageQuery:        QueryUsage,
	UsageOperatorWide: true,
}

func init() { delegator.Register(spec) }

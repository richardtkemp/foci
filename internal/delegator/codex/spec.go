package codex

import (
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelcaps"
)

// backendName is the [agents].backend value this package registers.
const backendName = "codex"

// cacheTTL is the prompt-cache time-to-live for OpenAI's API. OpenAI's prompt
// caching is shorter and less documented than Anthropic's; 5 minutes is a
// conservative estimate.
const cacheTTL = 5 * time.Minute

// spec declares the Codex backend: what it can do and its static data
// (#2154, internal/delegator/capabilities.go).
var spec = delegator.Spec{
	Name:        backendName,
	DisplayName: "Codex CLI",
	New:         newFromConfig,
	Prototype:   (*Backend)(nil),
	Wizard:      true,

	Caps: map[delegator.Capability]delegator.Support{
		delegator.CapPostToolNudge:  delegator.No("no mid-turn injection point is wired for the app-server"),
		delegator.CapPreAnswerNudge: delegator.No("no mid-turn injection point is wired for the app-server"),
		delegator.CapStreaming:      delegator.Yes(),

		delegator.CapControl:               delegator.Yes(),
		delegator.CapControlModel:          delegator.Yes(),
		delegator.CapControlEffort:         delegator.Yes(),
		delegator.CapControlPermissionMode: delegator.Yes(),
		delegator.CapThinkingControl:       delegator.No("the session thinking setting is read only by the API transport"),
		delegator.CapModelResolve:          delegator.Yes(),
		delegator.CapVoiceMode:             delegator.Yes(),

		delegator.CapCompactionWait:      delegator.Yes(),
		delegator.CapCompactionStartWait: delegator.No("the app-server reports only compaction completion"),
		delegator.CapCompactionSummary:   delegator.No("compaction output is server-side encrypted"),

		delegator.CapActivity:       delegator.Yes(),
		delegator.CapAutonomousRuns: delegator.No("does not track background work"),
		delegator.CapTurnAdoption:   delegator.NotApplicable("no autonomous runs to adopt"),
		delegator.CapStopSubagents:  delegator.NotApplicable("codex has no subagents"),
		delegator.CapStopCommands:   delegator.No("no API to stop a background command"),
		delegator.CapSubagentStatus: delegator.NotApplicable("codex has no subagents"),

		delegator.CapPermissionResponse: delegator.Yes(),
		delegator.CapPermissionRules:    delegator.No("approvals are accept or decline only"),
		delegator.CapQuestions:          delegator.No("the app-server has no question tool"),
		delegator.CapElicitation:        delegator.No("the app-server does not surface MCP elicitation requests"),
		delegator.CapPlanPermission:     delegator.NotApplicable("no plan mode"),

		delegator.CapFoldAttachments:  delegator.No("a message folded into a running turn carries text only"),
		delegator.CapDeliveryTracking: delegator.No("inputs are fire-and-forget: no proof of consumption"),
		delegator.CapThreadNaming:     delegator.Yes(),
		delegator.CapContextWindow:    delegator.Yes(),
		delegator.CapLiveModel:        delegator.No("its model reaches foci only through thread start and turn results"),

		delegator.CapBranch:        delegator.Yes(),
		delegator.CapScopedCleanup: delegator.Yes(),

		delegator.CapHostHooks:                delegator.No("reports no auth failures or rate limits foci acts on, and has no pretool or stop hooks"),
		delegator.CapUnstartedReadinessProbe:  delegator.Yes(),
		delegator.CapPreToolRules:             delegator.No("no PreToolUse rule engine is wired to the codex hook"),
		delegator.CapStopRules:                delegator.No("no Stop hook"),
		delegator.CapRelogin:                  delegator.No("the re-login driver logs Claude Code in"),
		delegator.CapCommandApprovalAllowlist: delegator.Yes(),
		delegator.CapPlanMode:                 delegator.No("no /plan delivery"),
		delegator.CapUsageQuery:               delegator.No("no plan usage to report"),
	},

	ModelcapsKey:     modelcaps.BackendCodex,
	LedgerKey:        accounting.BackendCodex,
	CacheTTL:         cacheTTL,
	ForkNeedsRunning: true,
	// sandbox and api_key are read but no config path supplies them
	// (docs/BACKENDS.md footnote 48). foci_version is added by the gateway.
	ConfigKeys: []string{"binary", "model", "sandbox", "api_key", delegator.FociVersionConfigKey},
	StartFields: []string{
		"WorkDir", "SystemPrompt", "Model", "AgentID", "Label", "ResumeSessionID", "SessionKey",
		"BatchOnly", "Env", "AutoApproveRules", "Effort", "CompactionPromptFunc",
	},
}

func init() { delegator.Register(spec) }

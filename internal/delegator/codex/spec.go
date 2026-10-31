package codex

import (
	"time"

	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/modelcaps"
)

// backendName is the [agents].backend value this package registers.
const backendName = "codex"

// cacheTTL is the prompt-cache lifetime codex sessions get. OpenAI documents
// a 30-minute minimum for GPT-5.6 and later on the API (prompt_cache_options.ttl
// default "30m"; 5-10 minutes for earlier in-memory models), and says nothing
// for ChatGPT-login codex. Measured on foci's own ledger (2026-10-27, codex
// 0.164.0, gpt-6.1-sol, ChatGPT login): calls after 60 and 90 minutes idle read
// 93% and 99% of their input from cache; after ~5h only the shared prefix
// survived. 1h sits inside the observed warm range. A [models.*] cache_ttl
// overrides it (agent.CacheTTL).
const cacheTTL = time.Hour

// spec declares the Codex backend: what it can do and its static data
// (#2154, internal/delegator/capabilities.go).
var spec = delegator.Spec{
	Name:        backendName,
	DisplayName: "Codex CLI",
	New:         newFromConfig,
	Prototype:   (*Backend)(nil),
	PermissionModes: []delegator.PermissionMode{
		{Name: "normal", Value: "user", Description: "human review; applies next turn", Aliases: []string{"default"}},
		{Name: "auto", Value: "auto_review", Description: "automatic review; applies next turn"},
	},
	Wizard: true,

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
		delegator.CapQuestionPrompt:     delegator.No("the app-server has no question prompts"),

		delegator.CapFoldAttachments:  delegator.No("a message folded into a running turn carries text only"),
		delegator.CapDeliveryTracking: delegator.No("inputs are fire-and-forget: no proof of consumption"),
		delegator.CapThreadNaming:     delegator.Yes(),
		delegator.CapContextWindow:    delegator.Yes(),
		delegator.CapLiveModel:        delegator.No("its model reaches foci only through thread start and turn results"),

		delegator.CapBranch:        delegator.Yes(),
		delegator.CapScopedCleanup: delegator.Yes(),
		delegator.CapIsolatedBatch: delegator.No("its sandbox confines writes, never reads: a session can read the whole disk"),
		delegator.CapReadOnlyBatch: delegator.Yes(),

		delegator.CapHostHooks:                delegator.Yes(),
		delegator.CapUnstartedReadinessProbe:  delegator.Yes(),
		delegator.CapPreToolRules:             delegator.No("no PreToolUse rule engine is wired to the codex hook"),
		delegator.CapStopRules:                delegator.No("no Stop hook"),
		delegator.CapRelogin:                  delegator.No("the re-login driver logs Claude Code in"),
		delegator.CapCommandApprovalAllowlist: delegator.Yes(),
		delegator.CapPlanMode:                 delegator.No("no /plan delivery"),
		delegator.CapUsageQuery:               delegator.Yes(),
	},

	ModelcapsKey:     modelcaps.BackendCodex,
	LedgerKey:        accounting.BackendCodex,
	CacheTTL:         cacheTTL,
	ForkNeedsRunning: true,
	UsageQuery:       QueryUsage,
	// api_key is read but no config path supplies it
	// (docs/BACKENDS.md footnote 48). foci_version is added by the gateway.
	ConfigKeys: []string{"binary", "model", "sandbox", "approvals_reviewer", "api_key", delegator.FociVersionConfigKey},
	StartFields: []string{
		"WorkDir", "SystemPrompt", "Model", "AgentID", "Label", "ResumeSessionID", "SessionKey",
		"BatchOnly", "BatchAccess", "Env", "AutoApproveRules", "Effort", "CompactionPromptFunc",
	},
}

func init() { delegator.Register(spec) }

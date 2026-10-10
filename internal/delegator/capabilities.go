package delegator

// ╔══ BACKEND CAPABILITIES: THE SINGLE SOURCE OF TRUTH (#2154) ══════════════╗
// ║ Everything a delegated backend can or cannot do is declared HERE, in the  ║
// ║ Spec it passes to Register, and asked through Spec.Supports / As. Do NOT: ║
// ║   - type-assert a Delegator for an optional interface (use As[T]);        ║
// ║   - compare a backend name string (add a Spec field or a Capability);     ║
// ║   - import a backend package outside internal/delegator (use .../all);    ║
// ║   - add an optional interface without a Capability row below.             ║
// ║ `make lint` enforces the first three outside internal/delegator           ║
// ║ (scripts/find-backend-capability-bypass; exception: a same-line           ║
// ║ `backend-cap:ignore: <reason>`).                                          ║
// ║ Adding a capability: add a const + a capabilityTable row, then declare it ║
// ║ in every backend's Spec. Adding a backend: Register(Spec{...}) declaring  ║
// ║ every capability. The delegator/all tests fail until the declarations    ║
// ║ are complete and true; at runtime an invalid Spec is logged as an error.  ║
// ║ The capability table in docs/BACKENDS.md is generated from the Specs.     ║
// ║ Config keys and StartOptions fields a backend reads are declared too      ║
// ║ (Spec.ConfigKeys / StartFields, config_schema.go, #2178).                 ║
// ╚═══════════════════════════════════════════════════════════════════════════╝

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Capability is one thing a delegated backend may or may not be able to do.
// Every backend declares every capability in its Spec.Caps (Yes, No or
// NotApplicable): there is no default, so "forgot to declare" can never read
// as "declared false".
type Capability int

const (
	// Behavioural: the backend honours a TurnEvents callback or a host input.
	CapPostToolNudge Capability = iota
	CapPreAnswerNudge
	CapStreaming

	// Runtime control (ControlSender) and its per-request sub-capabilities.
	CapControl
	CapControlModel
	CapControlEffort
	CapControlPermissionMode
	CapThinkingControl
	CapModelResolve
	CapVoiceMode

	// Compaction.
	CapCompactionWait
	CapCompactionStartWait
	CapCompactionSummary

	// Turn and background-work tracking.
	CapActivity
	CapAutonomousRuns
	CapTurnAdoption
	CapStopSubagents
	CapStopCommands
	CapSubagentStatus

	// Prompts the user answers.
	CapPermissionResponse
	CapPermissionRules
	CapQuestions
	CapElicitation
	CapPlanPermission
	CapQuestionPrompt

	// Input and delivery.
	CapFoldAttachments
	CapDeliveryTracking
	CapThreadNaming
	CapContextWindow
	CapLiveModel

	// Sessions.
	CapBranch
	CapScopedCleanup

	// Host integration.
	CapHostHooks
	CapUnstartedReadinessProbe
	CapPreToolRules
	CapStopRules
	CapRelogin
	CapCommandApprovalAllowlist
	CapPlanMode
	CapUsageQuery

	numCapabilities // sentinel: MUST stay last
)

// CapabilityKind says how a capability's declaration is checked.
type CapabilityKind int

const (
	// KindInterface: the capability is an optional interface. The declaration
	// must match the Spec.Prototype's method set, in both directions.
	KindInterface CapabilityKind = iota + 1
	// KindBehaviour: the backend honours something without a method of its own
	// (a TurnEvents callback, a host input). Every Yes must name a test that
	// proves it (internal/delegator/all, the proof registry).
	KindBehaviour
	// KindData: Yes exactly when the Spec carries the matching data field.
	KindData
)

// CapabilityInfo describes one capability: its stable name (docs, logs), a
// one-line doc rendered into docs/BACKENDS.md, how its declaration is checked,
// and, for KindInterface, the interface.
type CapabilityInfo struct {
	Name  string
	Doc   string
	Kind  CapabilityKind
	Iface reflect.Type
	// Requires lists capabilities that a Yes for this one implies.
	Requires []Capability
}

func ifaceOf[T any]() reflect.Type { return reflect.TypeFor[T]() }

// capabilityTable is sized by the sentinel, so a new constant without a row
// is a zero CapabilityInfo, which Validate reports for every backend.
var capabilityTable = [numCapabilities]CapabilityInfo{
	CapPostToolNudge:  {Name: "post_tool_nudge", Kind: KindBehaviour, Doc: "calls TurnEvents.PostToolNudgeFunc after each tool and injects what it returns (every_n_tools, after_error, tool_pattern nudges)"},
	CapPreAnswerNudge: {Name: "pre_answer_nudge", Kind: KindBehaviour, Doc: "calls TurnEvents.PreAnswerNudgeFunc at the final answer and re-dispatches the turn with what it returns (pre_answer nudges)"},
	CapStreaming:      {Name: "streaming", Kind: KindBehaviour, Doc: "emits SessionEvents.OnTextDelta/OnThinkingDelta during a turn (live stream_output)"},

	CapControl:               {Name: "control", Kind: KindInterface, Iface: ifaceOf[ControlSender](), Doc: "accepts runtime control requests (ControlSender)"},
	CapControlModel:          {Name: "control_model", Kind: KindBehaviour, Requires: []Capability{CapControl}, Doc: "applies a SetModelRequest mid-session"},
	CapControlEffort:         {Name: "control_effort", Kind: KindBehaviour, Requires: []Capability{CapControl}, Doc: "applies an effort change (ApplyFlagSettingsRequest effortLevel)"},
	CapControlPermissionMode: {Name: "control_permission_mode", Kind: KindBehaviour, Requires: []Capability{CapControl}, Doc: "applies a SetPermissionModeRequest mid-session"},
	CapThinkingControl:       {Name: "thinking_control", Kind: KindBehaviour, Doc: "honours the session thinking setting (/thinking)"},
	CapModelResolve:          {Name: "model_resolve", Kind: KindInterface, Iface: ifaceOf[ModelResolver](), Doc: "resolves model aliases against its own catalogue (ModelResolver)"},
	CapVoiceMode:             {Name: "voice_mode", Kind: KindInterface, Iface: ifaceOf[VoiceModer](), Doc: "runs voice-originated turns at low effort (VoiceModer)"},

	CapCompactionWait:      {Name: "compaction_wait", Kind: KindInterface, Iface: ifaceOf[CompactionWaiter](), Doc: "signals when a requested compaction finished (CompactionWaiter)"},
	CapCompactionStartWait: {Name: "compaction_start_wait", Kind: KindInterface, Iface: ifaceOf[CompactionStartWaiter](), Doc: "signals when a requested compaction started (CompactionStartWaiter)"},
	CapCompactionSummary:   {Name: "compaction_summary", Kind: KindInterface, Iface: ifaceOf[CompactionSummarizer](), Doc: "recovers its compaction summary text (CompactionSummarizer)"},

	CapActivity:       {Name: "activity", Kind: KindInterface, Iface: ifaceOf[ActivityChecker](), Doc: "reports its last stream activity, for activity-based timeouts (ActivityChecker)"},
	CapAutonomousRuns: {Name: "autonomous_runs", Kind: KindInterface, Iface: ifaceOf[AutonomousRunAwaiter](), Doc: "tracks background work and autonomous runs (AutonomousRunAwaiter)"},
	CapTurnAdoption:   {Name: "turn_adoption", Kind: KindInterface, Iface: ifaceOf[TurnAdopter](), Doc: "starts runs on its own and lets foci adopt them (TurnAdopter)"},
	CapStopSubagents:  {Name: "stop_subagents", Kind: KindInterface, Iface: ifaceOf[SubagentStopper](), Doc: "stops its running subagents (SubagentStopper)"},
	CapStopCommands:   {Name: "stop_commands", Kind: KindInterface, Iface: ifaceOf[CommandStopper](), Doc: "stops its background shell commands (CommandStopper)"},
	CapSubagentStatus: {Name: "subagent_status", Kind: KindInterface, Iface: ifaceOf[SubagentReporter](), Doc: "reports subagent status and the running background work (SubagentReporter)"},

	CapPermissionResponse: {Name: "permission_response", Kind: KindInterface, Iface: ifaceOf[PermissionResponder](), Doc: "delivers the user's answer to a permission prompt (PermissionResponder)"},
	CapPermissionRules:    {Name: "permission_rules", Kind: KindBehaviour, Requires: []Capability{CapPermissionResponse}, Doc: "turns PermissionDecision.RulePrefix into a persistent \"allow always\" rule"},
	CapQuestions:          {Name: "questions", Kind: KindInterface, Iface: ifaceOf[QuestionResponder](), Doc: "routes answers to the agent's own questions (QuestionResponder)"},
	CapElicitation:        {Name: "elicitation", Kind: KindInterface, Iface: ifaceOf[ElicitationResponder](), Doc: "answers MCP elicitation requests (ElicitationResponder)"},
	CapPlanPermission:     {Name: "plan_permission", Kind: KindInterface, Iface: ifaceOf[PlanResponder](), Doc: "turns a typed reply into plan-revision feedback (PlanResponder)"},
	CapQuestionPrompt:     {Name: "question_prompt", Kind: KindInterface, Iface: ifaceOf[QuestionPromptSetter](), Doc: "presents its own questions (AskUserQuestion, elicitation, the question tool) through a separate question prompt function (QuestionPromptSetter)"},

	CapFoldAttachments:  {Name: "fold_attachments", Kind: KindInterface, Iface: ifaceOf[FoldAttachmentCarrier](), Doc: "delivers attachments folded into a running turn (FoldAttachmentCarrier)"},
	CapDeliveryTracking: {Name: "delivery_tracking", Kind: KindInterface, Iface: ifaceOf[DeliveryTracker](), Doc: "proves each input reached the model and hands back the rest (DeliveryTracker + Spec.TranscriptChecker)"},
	CapThreadNaming:     {Name: "thread_naming", Kind: KindInterface, Iface: ifaceOf[ThreadNameConsumer](), Doc: "names its own sessions (ThreadNameConsumer)"},
	CapContextWindow:    {Name: "context_window", Kind: KindInterface, Iface: ifaceOf[ContextWindowQuerier](), Doc: "reports the model's context window and usage (ContextWindowQuerier)"},
	CapLiveModel:        {Name: "live_model", Kind: KindInterface, Iface: ifaceOf[LiveModelReporter](), Doc: "reports the model its live process last named, without a turn result (LiveModelReporter)"},

	CapBranch:        {Name: "branch", Kind: KindInterface, Iface: ifaceOf[BackendBrancher](), Doc: "forks and deletes its own sessions (BackendBrancher)"},
	CapScopedCleanup: {Name: "scoped_cleanup", Kind: KindInterface, Iface: ifaceOf[RunningBackendCleaner](), Requires: []Capability{CapBranch}, Doc: "needs a live server to delete sessions and opens it once per sweep (RunningBackendCleaner)"},

	CapHostHooks:                {Name: "host_hooks", Kind: KindInterface, Iface: ifaceOf[HostHooksAcceptor](), Doc: "takes the gateway's HostHooks: auth-failure and rate-limit reports, pretool and stop rules (HostHooksAcceptor)"},
	CapUnstartedReadinessProbe:  {Name: "unstarted_readiness_probe", Kind: KindBehaviour, Doc: "CheckReady works on a constructed but unstarted backend (the startup probe)"},
	CapPreToolRules:             {Name: "pretool_rules", Kind: KindBehaviour, Requires: []Capability{CapHostHooks}, Doc: "enforces PreToolUse deny rules (pretool_rules)"},
	CapStopRules:                {Name: "stop_rules", Kind: KindBehaviour, Requires: []Capability{CapHostHooks}, Doc: "enforces Stop-hook rules (stop_rules)"},
	CapRelogin:                  {Name: "relogin", Kind: KindBehaviour, Requires: []Capability{CapHostHooks}, Doc: "reports auth failures that foci's Claude Code re-login driver can fix (/login)"},
	CapCommandApprovalAllowlist: {Name: "command_approval_allowlist", Kind: KindBehaviour, Doc: "auto-approves prompts matching foci's [permissions] allowlist (StartOptions.AutoApproveRules)"},
	CapPlanMode:                 {Name: "plan_mode", Kind: KindData, Doc: "has a /plan delivery (Spec.PlanDelivery)"},
	CapUsageQuery:               {Name: "usage_query", Kind: KindData, Doc: "reports plan usage for /mana (Spec.UsageQuery)"},
}

// AllCapabilities returns every capability in declaration order.
func AllCapabilities() []Capability {
	caps := make([]Capability, numCapabilities)
	for i := range caps {
		caps[i] = Capability(i)
	}
	return caps
}

// Info returns the capability's table row.
func (c Capability) Info() CapabilityInfo {
	if c < 0 || c >= numCapabilities {
		return CapabilityInfo{}
	}
	return capabilityTable[c]
}

// String returns the capability's stable name.
func (c Capability) String() string {
	if n := c.Info().Name; n != "" {
		return n
	}
	return fmt.Sprintf("capability(%d)", int(c))
}

// SupportState is a declaration's verdict.
type SupportState int

const (
	Undeclared SupportState = iota
	Supported
	Unsupported
	NotApplicableState
)

// Support is one capability declaration. Build it with Yes, No or
// NotApplicable; the zero value is "undeclared", which Validate rejects.
type Support struct {
	state SupportState
	// Reason says why a No or NotApplicable holds. Required for both: it is
	// rendered into docs/BACKENDS.md.
	Reason string
}

// Yes declares the capability supported.
func Yes() Support { return Support{state: Supported} }

// No declares the capability unsupported, with the reason.
func No(reason string) Support { return Support{state: Unsupported, Reason: reason} }

// NotApplicable declares the capability meaningless for this backend, with
// the reason. Supports reports false, as for No.
func NotApplicable(reason string) Support { return Support{state: NotApplicableState, Reason: reason} }

// State returns the declaration's verdict.
func (s Support) State() SupportState { return s.state }

// Spec is everything foci knows about one delegated backend: its identity, its
// constructor, a declaration for every Capability, and the static data that
// used to sit behind getter interfaces and backend-name switches. A backend
// exists only through Register(Spec).
type Spec struct {
	Name        string // the [agents].backend config value
	DisplayName string // user-facing name, e.g. "Claude Code"
	New         Constructor
	// Prototype is a typed nil of the backend's concrete type, e.g.
	// (*Backend)(nil). It is never called: Validate reads its method set to
	// check each KindInterface declaration.
	Prototype Delegator
	// Wizard offers the backend in the setup wizard and /agents new.
	Wizard bool

	// Caps MUST declare every Capability.
	Caps map[Capability]Support

	DefaultModel       string        // launch model when neither the session nor config picks one
	ModelcapsKey       string        // modelcaps record key ("family") for this backend
	LedgerKey          string        // cost-ledger backend its turns book under (required)
	ClosesTurnActivity bool          // closes each turn's ledger activity itself (CC: background subagents outlive the turn)
	CacheTTL           time.Duration // prompt-cache TTL; 0 = unknown, config fallback
	BatchDefaultModel  string        // batch model when the request names none; "" = the agent's
	BatchCheapModel    string        // batch model for Cheap requests; "" = BatchDefaultModel
	ForkNeedsRunning   bool          // fork is an RPC to a live server: start the parent first
	// ConfigFamily names the global config section folded into the agent's
	// backend_config: ConfigFamilyClaudeCode, ConfigFamilyOpencode or ""
	// (none).
	ConfigFamily string
	// ConfigKeys are the backend_config map keys the backend package reads
	// (config_schema.go). Keys the gateway reads for it (model, idle_timeout,
	// ...) are not listed here. `make lint` checks the list against the source.
	ConfigKeys []string
	// StartFields are the StartOptions fields the backend package reads
	// (config_schema.go). `make lint` checks the list against the source.
	StartFields []string

	TranscriptChecker TranscriptChecker    // required iff CapDeliveryTracking
	ResumeRetention   func() time.Duration // the backend's own transcript retention; nil = unknown
	PlanDelivery      PlanDelivery         // required iff CapPlanMode
	UsageQuery        UsageQuery           // required iff CapUsageQuery

	// Process-global lifecycle hooks the gateway runs once for every backend.
	OnGatewayStart    func()
	OnGatewayShutdown func() int
}

// Config families (Spec.ConfigFamily): the global config section the gateway
// folds into an agent's backend_config.
const (
	ConfigFamilyClaudeCode = "claude-code" // [cc_backend]
	ConfigFamilyOpencode   = "opencode"    // [opencode_backend]
)

// Support returns the declaration for c (undeclared if missing).
func (s Spec) Support(c Capability) Support { return s.Caps[c] }

// Supports reports whether the backend declares c Yes.
func (s Spec) Supports(c Capability) bool { return s.Caps[c].state == Supported }

// Validate reports every way the Spec is incomplete or contradicts itself or
// its Prototype. The delegator/all tests fail on any error; Register logs them.
func (s Spec) Validate() error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("backend %q: "+format+" (see internal/delegator/capabilities.go)", append([]any{s.Name}, a...)...))
	}
	if s.Name == "" {
		bad("empty Name")
	}
	if s.DisplayName == "" {
		bad("empty DisplayName")
	}
	if s.New == nil {
		bad("nil New")
	}
	if s.Prototype == nil {
		bad("nil Prototype: set it to a typed nil, e.g. (*Backend)(nil)")
	}
	if s.LedgerKey == "" {
		bad("empty LedgerKey: every delegated backend books its calls")
	}
	if s.ModelcapsKey == "" {
		bad("empty ModelcapsKey")
	}
	for c := range s.Caps {
		if c < 0 || c >= numCapabilities {
			bad("declares unknown capability %d", int(c))
		}
	}
	var proto reflect.Type
	if s.Prototype != nil {
		proto = reflect.TypeOf(s.Prototype)
	}
	for _, c := range AllCapabilities() {
		info := c.Info()
		if info.Name == "" || info.Kind == 0 {
			bad("capability %d has no capabilityTable row", int(c))
			continue
		}
		sup := s.Caps[c]
		switch sup.state {
		case Undeclared:
			bad("does not declare capability %s: add Yes(), No(reason) or NotApplicable(reason) to its Spec.Caps", c)
			continue
		case Unsupported, NotApplicableState:
			if sup.Reason == "" {
				bad("declares %s unsupported without a reason", c)
			}
		}
		yes := sup.state == Supported
		if info.Kind == KindInterface && proto != nil {
			impl := proto.Implements(info.Iface)
			switch {
			case yes && !impl:
				bad("declares %s but %s does not implement %s", c, proto, info.Iface)
			case !yes && impl:
				bad("%s implements %s but does not declare %s: declare it, or delete the dead methods", proto, info.Iface, c)
			}
		}
		if yes {
			for _, r := range info.Requires {
				if !s.Supports(r) {
					bad("declares %s, which requires %s", c, r)
				}
			}
		}
	}
	coupled := func(c Capability, has bool, field string) {
		if s.Supports(c) != has {
			bad("%s must be declared Yes exactly when %s is set", c, field)
		}
	}
	coupled(CapDeliveryTracking, s.TranscriptChecker != nil, "TranscriptChecker")
	coupled(CapPlanMode, s.PlanDelivery != nil, "PlanDelivery")
	coupled(CapUsageQuery, s.UsageQuery != nil, "UsageQuery")
	if s.ForkNeedsRunning && !s.Supports(CapBranch) {
		bad("ForkNeedsRunning without %s", CapBranch)
	}
	s.validateSchema(bad)
	return errors.Join(errs...)
}

var (
	ifaceIndexOnce sync.Once
	ifaceIndex     map[reflect.Type]Capability
	unregisteredAs sync.Map // reflect.Type → struct{}: logged once each
)

// As is the sanctioned way to reach a capability's methods on a live backend:
// a type assertion that also insists T is the interface of a registered
// Capability. An unregistered T is an optional interface that bypasses the
// declarations; it panics under test and is logged once in production.
func As[T any](be Delegator) (T, bool) {
	t := reflect.TypeFor[T]()
	ifaceIndexOnce.Do(func() {
		ifaceIndex = make(map[reflect.Type]Capability)
		for _, c := range AllCapabilities() {
			if it := c.Info().Iface; it != nil {
				ifaceIndex[it] = c
			}
		}
	})
	if _, ok := ifaceIndex[t]; !ok {
		msg := fmt.Sprintf("delegator.As[%s]: not the interface of any Capability — add a row to internal/delegator/capabilities.go", t)
		if testing.Testing() {
			panic(msg)
		}
		if _, seen := unregisteredAs.LoadOrStore(t, struct{}{}); !seen {
			delegatedLog.Errorf("%s", msg)
		}
	}
	v, ok := be.(T)
	return v, ok
}

package config

import (
	"sync"

	"foci/internal/log"
)

// Call site names — each identifies a specific LLM call site in the codebase.
const (
	// Powerful group (default)
	CallChat              = "chat"
	CallSpawnClone        = "spawn-clone"
	CallBackground        = "background"
	CallCompaction        = "compaction"
	CallMemoryCapture     = "memory-capture"
	CallMemoryConsolidate = "memory-consolidate"

	// Fast group (default)
	CallSpawnRaw       = "spawn-raw"
	CallSpawnCharacter = "spawn-character"

	// Cheap group (default)
	CallSpawnExplore  = "spawn-explore"
	CallSummarizeTool = "summarize-tool"
	CallSummarizeFile = "summarize-file"
	CallPromptDiff    = "prompt-diff"

	// Ungrouped — always use session model
	CallKeepalive   = "keepalive"
	CallCountTokens = "count-tokens"
)

// Built-in group names.
const (
	GroupPowerful = "powerful"
	GroupFast     = "fast"
	GroupCheap    = "cheap"
)

// defaultCallGroups maps each call site to its default group.
// Ungrouped calls are absent from this map.
var defaultCallGroups = map[string]string{
	CallChat:              GroupPowerful,
	CallSpawnClone:        GroupPowerful,
	CallBackground:        GroupPowerful,
	CallCompaction:        GroupPowerful,
	CallMemoryCapture:     GroupPowerful,
	CallMemoryConsolidate: GroupPowerful,

	CallSpawnRaw:       GroupFast,
	CallSpawnCharacter: GroupFast,

	CallSpawnExplore:  GroupCheap,
	CallSummarizeTool: GroupCheap,
	CallSummarizeFile: GroupCheap,
	CallPromptDiff:    GroupCheap,
}

// GroupResolver resolves call sites and group names to concrete models.
// groups/callOverrides/models/apiAgentsPresent are user-defined maps (and
// their merge is per-agent) — a live /config set edit rebuilds them via
// Update, so every field is guarded by mu rather than set once at
// construction (same pattern as compaction.Compactor's threshold fields).
type GroupResolver struct {
	mu sync.RWMutex
	// groups maps group name → model name (key in models map or raw developer/model_id)
	groups map[string]string
	// callOverrides maps call site name → group name (from [groups.calls])
	callOverrides map[string]string
	// models for ResolveModel (carries settings through)
	models map[string]ModelConfig
	// apiAgentsPresent is true when the deployment has at least one API-backed
	// agent. When false, no model group should ever resolve to a concrete model
	// (delegated backends route everything through the backend) — resolveGroup
	// logs a loud error if it does, surfacing the regression in normal use.
	apiAgentsPresent bool
}

// NewGroupResolver creates a GroupResolver from config.
// groups maps group name → model name. calls maps call site → group override.
// apiAgentsPresent should be cfg.HasAPIAgent(): when no API agent is configured,
// any successful group resolution is a bug and is logged loudly.
func NewGroupResolver(gc GroupsConfig, models map[string]ModelConfig, apiAgentsPresent bool) *GroupResolver {
	gr := &GroupResolver{}
	gr.Update(gc, models, apiAgentsPresent)
	return gr
}

// Update replaces the resolver's state in place, for a live /config set edit
// to groups/groups.calls/groups.fallbacks/models — called by the
// liveApplyResolvedAddrs-adjacent applier (cmd/foci-gw/liveapply.go) with the
// same freshly-per-agent-merged GroupsConfig that Resolve() would have
// produced, so every call site holding this *GroupResolver keeps working
// unchanged: the pointer never changes, only what it reads.
func (gr *GroupResolver) Update(gc GroupsConfig, models map[string]ModelConfig, apiAgentsPresent bool) {
	gr.mu.Lock()
	defer gr.mu.Unlock()
	gr.models = models
	gr.callOverrides = gc.Calls
	gr.groups = gc.Groups
	gr.apiAgentsPresent = apiAgentsPresent
}

// GroupNames returns the names of all configured groups.
func (gr *GroupResolver) GroupNames() []string {
	gr.mu.RLock()
	defer gr.mu.RUnlock()
	names := make([]string, 0, len(gr.groups))
	for name := range gr.groups {
		names = append(names, name)
	}
	return names
}

// ResolveCall resolves a call site to a concrete model.
// Returns nil for ungrouped calls.
func (gr *GroupResolver) ResolveCall(callSite string) *ResolvedModel {
	gr.warnIfNoAPIAgent("call=" + callSite)

	// Check if this call site has a group assignment
	groupName, ok := defaultCallGroups[callSite]
	if !ok {
		return nil // ungrouped
	}

	gr.mu.RLock()
	// Check for call-level override
	if gr.callOverrides != nil {
		if override, ok := gr.callOverrides[callSite]; ok {
			groupName = override
		}
	}
	gr.mu.RUnlock()

	return gr.resolveGroup(groupName)
}

// ResolveGroup resolves a group name to a concrete model.
// Returns nil if the group name is unknown — use for user-provided group names.
func (gr *GroupResolver) ResolveGroup(groupName string) *ResolvedModel {
	gr.warnIfNoAPIAgent("group=" + groupName)
	return gr.resolveGroup(groupName)
}

// resolveGroup resolves a group name to a ResolvedModel.
// Returns nil if the group name is not found.
func (gr *GroupResolver) resolveGroup(groupName string) *ResolvedModel {
	gr.mu.RLock()
	model, ok := gr.groups[groupName]
	models := gr.models
	gr.mu.RUnlock()
	if !ok {
		return nil
	}
	resolved, err := ResolveModel(model, "", models)
	if err != nil {
		log.Warnf("config", "group %q is configured but its model failed to resolve: %v", groupName, err)
		return nil
	}
	return resolved
}

// warnIfNoAPIAgent logs a loud error if the resolver is invoked at all in a
// deployment with no API-backed agent. Delegated backends (claude-code, etc.)
// route ALL LLM work through the backend and must never touch the model groups,
// so any resolver call — whether or not it resolves to a model — means a caller
// is doing work it shouldn't (and is likely about to reach for credentials that
// don't exist). Surfacing it loudly lets the offending call site be found and
// guarded in normal use rather than as a confusing downstream
// "no Anthropic credentials" error.
func (gr *GroupResolver) warnIfNoAPIAgent(what string) {
	gr.mu.RLock()
	present := gr.apiAgentsPresent
	gr.mu.RUnlock()
	if !present {
		log.Errorf("config", "BUG: model resolver invoked (%s) but no API-backed agent is configured — claude-code-only deployments must never touch the model groups; this call site should be guarded", what)
	}
}

package config

// Call site names — each identifies a specific LLM call site in the codebase.
const (
	// Powerful group (default)
	CallChat               = "chat"
	CallSpawnClone         = "spawn-clone"
	CallBackground         = "background"
	CallCompaction         = "compaction"
	CallMemoryCapture      = "memory-capture"
	CallMemoryConsolidate  = "memory-consolidate"

	// Fast group (default)
	CallSpawnRaw       = "spawn-raw"
	CallSpawnCharacter = "spawn-character"

	// Cheap group (default)
	CallSpawnExplore = "spawn-explore"
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
type GroupResolver struct {
	// groups maps group name → model name (key in models map or raw developer/model_id)
	groups map[string]string
	// callOverrides maps call site name → group name (from [groups.calls])
	callOverrides map[string]string
	// models for ResolveModel (carries settings through)
	models map[string]ModelConfig
}

// NewGroupResolver creates a GroupResolver from config.
// groups maps group name → model name. calls maps call site → group override.
func NewGroupResolver(gc GroupsConfig, models map[string]ModelConfig) *GroupResolver {
	return &GroupResolver{
		models:        models,
		callOverrides: gc.Calls,
		groups:        gc.Groups,
	}
}

// GroupNames returns the names of all configured groups.
func (gr *GroupResolver) GroupNames() []string {
	names := make([]string, 0, len(gr.groups))
	for name := range gr.groups {
		names = append(names, name)
	}
	return names
}

// ResolveCall resolves a call site to a concrete model.
// Returns nil for ungrouped calls.
func (gr *GroupResolver) ResolveCall(callSite string) *ResolvedModel {
	// Check if this call site has a group assignment
	groupName, ok := defaultCallGroups[callSite]
	if !ok {
		return nil // ungrouped
	}

	// Check for call-level override
	if gr.callOverrides != nil {
		if override, ok := gr.callOverrides[callSite]; ok {
			groupName = override
		}
	}

	return gr.resolveGroup(groupName)
}

// ResolveGroup resolves a group name to a concrete model.
// Returns nil if the group name is unknown — use for user-provided group names.
func (gr *GroupResolver) ResolveGroup(groupName string) *ResolvedModel {
	return gr.resolveGroup(groupName)
}

// resolveGroup resolves a group name to a ResolvedModel.
// Returns nil if the group name is not found.
func (gr *GroupResolver) resolveGroup(groupName string) *ResolvedModel {
	model, ok := gr.groups[groupName]
	if !ok {
		return nil
	}
	resolved, err := ResolveModel(model, "", gr.models)
	if err != nil {
		return nil
	}
	return resolved
}


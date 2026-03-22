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
// Powerful must be set in config (validated by Validate). Fast/Cheap default
// to Powerful when not set.
func NewGroupResolver(groups ResolvedGroups, models map[string]ModelConfig) *GroupResolver {
	powerful := groups.Powerful
	gr := &GroupResolver{
		models:        models,
		callOverrides: groups.Calls,
		groups: map[string]string{
			GroupPowerful: powerful,
		},
	}

	if groups.Fast != "" {
		gr.groups[GroupFast] = groups.Fast
	} else {
		gr.groups[GroupFast] = powerful
	}
	if groups.Cheap != "" {
		gr.groups[GroupCheap] = groups.Cheap
	} else {
		gr.groups[GroupCheap] = powerful
	}

	return gr
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
// Falls back to the powerful group if the group name is unknown.
func (gr *GroupResolver) ResolveGroup(groupName string) *ResolvedModel {
	return gr.resolveGroup(groupName)
}

// resolveGroup resolves a group name to a ResolvedModel.
func (gr *GroupResolver) resolveGroup(groupName string) *ResolvedModel {
	model, ok := gr.groups[groupName]
	if !ok {
		// Unknown group — fall back to powerful
		model = gr.groups[GroupPowerful]
	}
	resolved, err := ResolveModel(model, "", gr.models)
	if err != nil {
		return nil
	}
	return resolved
}

// PowerfulModel returns the model string for the powerful group.
func (gr *GroupResolver) PowerfulModel() string {
	return gr.groups[GroupPowerful]
}

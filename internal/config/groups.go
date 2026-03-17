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
// When groups are not configured (single-model mode), all calls return nil
// indicating the caller should use the session model.
type GroupResolver struct {
	// groups maps group name → model string (developer/model_id format)
	groups map[string]string
	// callOverrides maps call site name → group name (from [models.calls])
	callOverrides map[string]string
	// aliases for ResolveModel
	aliases map[string]string
	// singleModel is true when no groups are configured
	singleModel bool
}

// NewGroupResolver creates a GroupResolver from config.
// When models.Powerful is empty, single-model mode is used (all calls return nil).
// Fast/Cheap default to Powerful when not set.
func NewGroupResolver(models ModelsConfig, aliases map[string]string) *GroupResolver {
	gr := &GroupResolver{
		aliases:       aliases,
		callOverrides: models.Calls,
	}

	if models.Powerful == "" {
		gr.singleModel = true
		return gr
	}

	gr.groups = map[string]string{
		GroupPowerful: models.Powerful,
	}
	if models.Fast != "" {
		gr.groups[GroupFast] = models.Fast
	} else {
		gr.groups[GroupFast] = models.Powerful
	}
	if models.Cheap != "" {
		gr.groups[GroupCheap] = models.Cheap
	} else {
		gr.groups[GroupCheap] = models.Powerful
	}

	return gr
}

// IsSingleModel returns true when no model groups are configured.
// In single-model mode, all calls should use the session model.
func (gr *GroupResolver) IsSingleModel() bool {
	return gr.singleModel
}

// GroupNames returns the names of all configured groups.
func (gr *GroupResolver) GroupNames() []string {
	if gr.singleModel {
		return nil
	}
	names := make([]string, 0, len(gr.groups))
	for name := range gr.groups {
		names = append(names, name)
	}
	return names
}

// ResolveCall resolves a call site to a concrete model.
// Returns nil for ungrouped calls or when in single-model mode.
func (gr *GroupResolver) ResolveCall(callSite string) *ResolvedModel {
	if gr.singleModel {
		return nil
	}

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
// Returns nil in single-model mode.
func (gr *GroupResolver) ResolveGroup(groupName string) *ResolvedModel {
	if gr.singleModel {
		return nil
	}
	return gr.resolveGroup(groupName)
}

// resolveGroup resolves a group name to a ResolvedModel.
func (gr *GroupResolver) resolveGroup(groupName string) *ResolvedModel {
	model, ok := gr.groups[groupName]
	if !ok {
		// Unknown group — fall back to powerful
		model = gr.groups[GroupPowerful]
	}
	resolved, err := ResolveModel(model, "", gr.aliases)
	if err != nil {
		return nil
	}
	return resolved
}

// PowerfulModel returns the model string for the powerful group,
// or empty string in single-model mode.
func (gr *GroupResolver) PowerfulModel() string {
	if gr.singleModel {
		return ""
	}
	return gr.groups[GroupPowerful]
}

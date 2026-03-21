package config

import (
	"sort"
	"testing"
)

// TestPowerfulDefault verifies that when only Powerful is set,
// all groups default to the powerful model.
// Grouped calls resolve to the powerful model; ungrouped calls still return nil.
func TestPowerfulDefault(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-sonnet-4-10-20250514"),
	}, nil)

	if names := gr.GroupNames(); len(names) != 3 {
		t.Fatalf("expected 3 group names, got %v", names)
	}
	if pm := gr.PowerfulModel(); pm != "anthropic/claude-sonnet-4-10-20250514" {
		t.Fatalf("PowerfulModel() = %q, want powerful model", pm)
	}

	// Grouped call sites should resolve to the powerful model
	for _, cs := range []string{CallChat, CallSpawnExplore, CallSummarizeTool} {
		r := gr.ResolveCall(cs)
		if r == nil {
			t.Errorf("ResolveCall(%q) = nil, want powerful model", cs)
			continue
		}
		if r.ModelID != "claude-sonnet-4-10-20250514" {
			t.Errorf("ResolveCall(%q).ModelID = %q, want %q", cs, r.ModelID, "claude-sonnet-4-10-20250514")
		}
	}

	// Ungrouped call sites should still return nil
	for _, cs := range []string{CallKeepalive, CallCountTokens} {
		if r := gr.ResolveCall(cs); r != nil {
			t.Errorf("ResolveCall(%q) = %+v, want nil (ungrouped)", cs, r)
		}
	}

	// ResolveGroup should resolve to powerful model
	r := gr.ResolveGroup(GroupPowerful)
	if r == nil {
		t.Fatal("ResolveGroup(powerful) = nil")
	}
	if r.ModelID != "claude-sonnet-4-10-20250514" {
		t.Errorf("ResolveGroup(powerful).ModelID = %q, want powerful model", r.ModelID)
	}
}

// TestThreeGroupsResolution verifies that when all three groups are defined,
// each call site resolves to the correct model based on its default group.
func TestThreeGroupsResolution(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-opus-4-6"),
		Fast:     Ptr("anthropic/claude-sonnet-4-10-20250514"),
		Cheap:    Ptr("anthropic/claude-haiku-4-5-20251001"),
	}, nil)

	tests := []struct {
		callSite string
		wantID   string
	}{
		// Powerful group
		{CallChat, "claude-opus-4-6"},
		{CallCompaction, "claude-opus-4-6"},
		{CallMemoryCapture, "claude-opus-4-6"},

		// Fast group
		{CallSpawnRaw, "claude-sonnet-4-10-20250514"},
		{CallSpawnCharacter, "claude-sonnet-4-10-20250514"},

		// Cheap group
		{CallSpawnExplore, "claude-haiku-4-5-20251001"},
		{CallSummarizeTool, "claude-haiku-4-5-20251001"},
		{CallSummarizeFile, "claude-haiku-4-5-20251001"},
		{CallPromptDiff, "claude-haiku-4-5-20251001"},
	}

	for _, tt := range tests {
		r := gr.ResolveCall(tt.callSite)
		if r == nil {
			t.Errorf("ResolveCall(%q) = nil, want model %q", tt.callSite, tt.wantID)
			continue
		}
		if r.ModelID != tt.wantID {
			t.Errorf("ResolveCall(%q).ModelID = %q, want %q", tt.callSite, r.ModelID, tt.wantID)
		}
		if r.Developer != "anthropic" {
			t.Errorf("ResolveCall(%q).Developer = %q, want %q", tt.callSite, r.Developer, "anthropic")
		}
		if r.Format != "anthropic" {
			t.Errorf("ResolveCall(%q).Format = %q, want %q", tt.callSite, r.Format, "anthropic")
		}
	}
}

// TestMissingFastCheapDefaultsToPowerful verifies that when Fast and Cheap
// are not set, they default to the Powerful model.
func TestMissingFastCheapDefaultsToPowerful(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-opus-4-6"),
	}, nil)

	// Fast call site should resolve to powerful model
	r := gr.ResolveCall(CallSpawnRaw)
	if r == nil {
		t.Fatal("ResolveCall(CallSpawnRaw) = nil")
	}
	if r.ModelID != "claude-opus-4-6" {
		t.Errorf("Fast defaulted to %q, want %q", r.ModelID, "claude-opus-4-6")
	}

	// Cheap call site should resolve to powerful model
	r = gr.ResolveCall(CallSpawnExplore)
	if r == nil {
		t.Fatal("ResolveCall(CallSpawnExplore) = nil")
	}
	if r.ModelID != "claude-opus-4-6" {
		t.Errorf("Cheap defaulted to %q, want %q", r.ModelID, "claude-opus-4-6")
	}
}

// TestCallOverrides verifies that [models.calls] overrides take precedence
// over the default group assignment for a call site.
func TestCallOverrides(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-opus-4-6"),
		Fast:     Ptr("anthropic/claude-sonnet-4-10-20250514"),
		Cheap:    Ptr("anthropic/claude-haiku-4-5-20251001"),
		Calls: map[string]string{
			CallCompaction: GroupFast, // move compaction from powerful -> fast
		},
	}, nil)

	r := gr.ResolveCall(CallCompaction)
	if r == nil {
		t.Fatal("ResolveCall(CallCompaction) = nil")
	}
	if r.ModelID != "claude-sonnet-4-10-20250514" {
		t.Errorf("overridden compaction model = %q, want %q", r.ModelID, "claude-sonnet-4-10-20250514")
	}

	// Non-overridden call should still use default group
	r = gr.ResolveCall(CallChat)
	if r == nil {
		t.Fatal("ResolveCall(CallChat) = nil")
	}
	if r.ModelID != "claude-opus-4-6" {
		t.Errorf("chat model = %q, want %q", r.ModelID, "claude-opus-4-6")
	}
}

// TestInvalidOverrideGroupFallsToPowerful verifies that if a call override
// references a non-existent group name, it falls back to the powerful group.
func TestInvalidOverrideGroupFallsToPowerful(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-opus-4-6"),
		Cheap:    Ptr("anthropic/claude-haiku-4-5-20251001"),
		Calls: map[string]string{
			CallCompaction: "nonexistent-group",
		},
	}, nil)

	r := gr.ResolveCall(CallCompaction)
	if r == nil {
		t.Fatal("ResolveCall(CallCompaction) = nil")
	}
	if r.ModelID != "claude-opus-4-6" {
		t.Errorf("invalid group fell back to %q, want %q", r.ModelID, "claude-opus-4-6")
	}
}

// TestUngroupedCallsReturnNil verifies that call sites not in the
// defaultCallGroups map always return nil (use session model).
func TestUngroupedCallsReturnNil(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-opus-4-6"),
	}, nil)

	for _, cs := range []string{CallKeepalive, CallCountTokens} {
		if r := gr.ResolveCall(cs); r != nil {
			t.Errorf("ResolveCall(%q) = %+v, want nil (ungrouped)", cs, r)
		}
	}
}

// TestResolveGroupByName verifies that ResolveGroup resolves group names
// directly, falling back to powerful for unknown names.
func TestResolveGroupByName(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-opus-4-6"),
		Fast:     Ptr("google/gemini-2.5-flash"),
		Cheap:    Ptr("anthropic/claude-haiku-4-5-20251001"),
	}, nil)

	tests := []struct {
		group      string
		wantID     string
		wantFormat string
	}{
		{GroupPowerful, "claude-opus-4-6", "anthropic"},
		{GroupFast, "gemini-2.5-flash", "gemini"},
		{GroupCheap, "claude-haiku-4-5-20251001", "anthropic"},
		{"unknown", "claude-opus-4-6", "anthropic"}, // falls back to powerful
	}

	for _, tt := range tests {
		r := gr.ResolveGroup(tt.group)
		if r == nil {
			t.Errorf("ResolveGroup(%q) = nil", tt.group)
			continue
		}
		if r.ModelID != tt.wantID {
			t.Errorf("ResolveGroup(%q).ModelID = %q, want %q", tt.group, r.ModelID, tt.wantID)
		}
		if r.Format != tt.wantFormat {
			t.Errorf("ResolveGroup(%q).Format = %q, want %q", tt.group, r.Format, tt.wantFormat)
		}
	}
}

// TestGroupNamesReturnsAllGroups verifies GroupNames returns all three
// built-in groups when configured.
func TestGroupNamesReturnsAllGroups(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-opus-4-6"),
		Fast:     Ptr("anthropic/claude-sonnet-4-10-20250514"),
		Cheap:    Ptr("anthropic/claude-haiku-4-5-20251001"),
	}, nil)

	names := gr.GroupNames()
	sort.Strings(names)
	want := []string{GroupCheap, GroupFast, GroupPowerful}
	if len(names) != len(want) {
		t.Fatalf("GroupNames() = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("GroupNames()[%d] = %q, want %q", i, n, want[i])
		}
	}
}

// TestPowerfulModel verifies that PowerfulModel returns the raw model string
// for the powerful group.
func TestPowerfulModel(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-opus-4-6"),
	}, nil)

	if pm := gr.PowerfulModel(); pm != "anthropic/claude-opus-4-6" {
		t.Errorf("PowerfulModel() = %q, want %q", pm, "anthropic/claude-opus-4-6")
	}
}

// TestMixedDevelopers verifies that groups can use models from different
// developers and each resolves with the correct format and endpoint.
func TestMixedDevelopers(t *testing.T) {
	gr := NewGroupResolver(GroupsConfig{
		Powerful: Ptr("anthropic/claude-opus-4-6"),
		Fast:     Ptr("google/gemini-2.5-flash"),
		Cheap:    Ptr("openai/gpt-4o-mini"),
	}, nil)

	tests := []struct {
		callSite       string
		wantDeveloper  string
		wantFormat     string
	}{
		{CallChat, "anthropic", "anthropic"},
		{CallSpawnRaw, "google", "gemini"},
		{CallSpawnExplore, "openai", "openai"},
	}

	for _, tt := range tests {
		r := gr.ResolveCall(tt.callSite)
		if r == nil {
			t.Errorf("ResolveCall(%q) = nil", tt.callSite)
			continue
		}
		if r.Developer != tt.wantDeveloper {
			t.Errorf("ResolveCall(%q).Developer = %q, want %q", tt.callSite, r.Developer, tt.wantDeveloper)
		}
		if r.Format != tt.wantFormat {
			t.Errorf("ResolveCall(%q).Format = %q, want %q", tt.callSite, r.Format, tt.wantFormat)
		}
	}
}

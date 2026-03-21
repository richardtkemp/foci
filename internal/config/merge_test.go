package config

import (
	"reflect"
	"testing"
)

// TestFirst_ReturnsFirstNonNil verifies that First returns the first non-nil
// pointer from a variadic list, or nil if all are nil.
func TestFirst_ReturnsFirstNonNil(t *testing.T) {
	a := "agent"
	g := "global"

	tests := []struct {
		name string
		ptrs []*string
		want *string
	}{
		{"all nil", []*string{nil, nil}, nil},
		{"first set", []*string{&a, &g}, &a},
		{"second set", []*string{nil, &g}, &g},
		{"single set", []*string{&a}, &a},
		{"empty", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := First(tt.ptrs...)
			if got != tt.want {
				t.Errorf("First() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSuperveneSlice_BothEmpty proves that two empty slices produce nil.
func TestSuperveneSlice_BothEmpty(t *testing.T) {
	result := SuperveneSlice[string](nil, nil, func(s string) string { return s })
	if result != nil {
		t.Errorf("got %v, want nil", result)
	}
}

// TestSuperveneSlice_AgentEmpty proves that when agent has no entries,
// the global slice is returned unchanged.
func TestSuperveneSlice_AgentEmpty(t *testing.T) {
	global := []string{"alice", "bob"}
	result := SuperveneSlice[string](nil, global, func(s string) string { return s })
	if !reflect.DeepEqual(result, global) {
		t.Errorf("got %v, want %v", result, global)
	}
}

// TestSuperveneSlice_GlobalEmpty proves that when global has no entries,
// the agent slice is returned unchanged.
func TestSuperveneSlice_GlobalEmpty(t *testing.T) {
	agent := []string{"carol"}
	result := SuperveneSlice(agent, nil, func(s string) string { return s })
	if !reflect.DeepEqual(result, agent) {
		t.Errorf("got %v, want %v", result, agent)
	}
}

// TestSuperveneSlice_NoOverlap proves that non-overlapping entries are
// concatenated: global entries first, then agent-only entries appended.
func TestSuperveneSlice_NoOverlap(t *testing.T) {
	global := []string{"alice", "bob"}
	agent := []string{"carol", "dave"}
	result := SuperveneSlice(agent, global, func(s string) string { return s })
	want := []string{"alice", "bob", "carol", "dave"}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("got %v, want %v", result, want)
	}
}

// TestSuperveneSlice_FullOverlap proves that when all agent keys match global
// keys, agent versions replace global versions in global order.
func TestSuperveneSlice_FullOverlap(t *testing.T) {
	type item struct{ Key, Val string }
	global := []item{{"a", "g1"}, {"b", "g2"}}
	agent := []item{{"b", "a2"}, {"a", "a1"}}
	result := SuperveneSlice(agent, global, func(i item) string { return i.Key })
	want := []item{{"a", "a1"}, {"b", "a2"}}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("got %v, want %v", result, want)
	}
}

// TestSuperveneSlice_PartialOverlap proves the core supervene semantics:
// matching global entries are overridden by agent, non-matching global entries
// pass through, and agent-only entries are appended.
func TestSuperveneSlice_PartialOverlap(t *testing.T) {
	type item struct{ Key, Val string }
	global := []item{{"a", "g1"}, {"b", "g2"}, {"c", "g3"}}
	agent := []item{{"b", "a2"}, {"d", "a4"}}
	result := SuperveneSlice(agent, global, func(i item) string { return i.Key })
	want := []item{{"a", "g1"}, {"b", "a2"}, {"c", "g3"}, {"d", "a4"}}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("got %v, want %v", result, want)
	}
}

// TestSuperveneSlice_MessageTransforms proves supervene works for the
// MessageTransform use case: agent overrides matching Find patterns,
// global rules with different patterns fall through.
func TestSuperveneSlice_MessageTransforms(t *testing.T) {
	global := []MessageTransform{
		{Find: `a`, Replace: "b"},
		{Find: `1`, Replace: "2"},
	}
	agent := []MessageTransform{
		{Find: `a`, Replace: "z"},
	}
	result := SuperveneSlice(agent, global, func(mt MessageTransform) string { return mt.Find })
	want := []MessageTransform{
		{Find: `a`, Replace: "z"},
		{Find: `1`, Replace: "2"},
	}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("got %v, want %v", result, want)
	}
}

// TestSuperveneSlice_BlockedPaths proves supervene works for the
// BlockedPath use case: agent can override rebuke for a path while
// global paths with different prefixes still apply.
func TestSuperveneSlice_BlockedPaths(t *testing.T) {
	global := []BlockedPath{
		{Path: "/etc", Rebuke: "hands off /etc"},
		{Path: "/var", Rebuke: "no writes to /var"},
	}
	agent := []BlockedPath{
		{Path: "/etc", Rebuke: "agent says no to /etc"},
		{Path: "/tmp/secrets", Rebuke: "agent-only block"},
	}
	result := SuperveneSlice(agent, global, func(bp BlockedPath) string { return bp.Path })
	want := []BlockedPath{
		{Path: "/etc", Rebuke: "agent says no to /etc"},
		{Path: "/var", Rebuke: "no writes to /var"},
		{Path: "/tmp/secrets", Rebuke: "agent-only block"},
	}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("got %v, want %v", result, want)
	}
}

// TestSuperveneSlice_StringDedup proves that for string slices (AllowedUsers),
// duplicate strings from agent are effectively deduped with global.
func TestSuperveneSlice_StringDedup(t *testing.T) {
	global := []string{"111", "222", "333"}
	agent := []string{"222", "444"}
	result := SuperveneSlice(agent, global, func(s string) string { return s })
	want := []string{"111", "222", "333", "444"}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("got %v, want %v", result, want)
	}
}

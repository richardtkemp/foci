package config

import (
	"reflect"
	"testing"
)

// descend = the override-tree "spine": container structs we walk through to
// find cascade groups. They hold their own value-typed config and are never
// passed to Merge whole — the cascade group is extracted first (e.g.
// acfg.Tools.ToolConfig), so the containers themselves needn't be all-nillable.
var descend = map[reflect.Type]bool{
	reflect.TypeOf(AgentConfig{}):           true,
	reflect.TypeOf(PlatformConfig{}):        true,
	reflect.TypeOf(AgentToolsOverride{}):    true,
	reflect.TypeOf(AgentSessionsOverride{}): true,
}

// notCascade = value-structs reached from the spine that are NOT resolved via
// Merge: Groups/Permissions aren't Merge'd; MessageTransform/BlockedPath are
// plain slice-element value structs.
var notCascade = map[reflect.Type]bool{
	reflect.TypeOf(GroupsConfig{}):      true,
	reflect.TypeOf(PermissionsConfig{}): true,
	reflect.TypeOf(MessageTransform{}):  true,
	reflect.TypeOf(BlockedPath{}):       true,
}

// TestCascadeStructFieldsAreNillable walks the per-agent / per-platform override
// tree and asserts every cascade group's fields are pointer/slice/map.
//
// Merge[T] (merge.go) resolves the config cascade by calling reflect.Value.IsNil
// on each field — and IsNil PANICS on a value-typed field (int/string/bool/
// struct). So every struct passed to Merge must be entirely nillable. This test
// derives those structs structurally (walk the spine, treat each reached struct
// as a cascade group unless it's a known container or non-cascade type), so a
// newly-added override field is checked automatically.
//
// A failure means one of:
//   - a value-typed field was added to a cascade struct → make it a pointer and
//     add an accessor for its code default (per the cascade pattern); or
//   - a new non-cascade struct was added to the override spine → classify it in
//     `descend` (a container to walk through) or `notCascade` (skip).
func TestCascadeStructFieldsAreNillable(t *testing.T) {
	checked := map[reflect.Type]bool{}
	var visit func(rt reflect.Type)
	visit = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			ft := rt.Field(i).Type
			if ft.Kind() == reflect.Slice {
				ft = ft.Elem() // []PlatformConfig, []MessageTransform, …
			}
			if ft.Kind() != reflect.Struct {
				continue // pointers, maps, scalars are not cascade groups
			}
			switch {
			case descend[ft]:
				visit(ft)
			case notCascade[ft]:
				// not resolved via Merge — skip
			default:
				checkNillable(t, ft, checked)
			}
		}
	}
	visit(reflect.TypeOf(AgentConfig{}))
	visit(reflect.TypeOf(PlatformConfig{}))
}

// checkNillable asserts every field of a cascade group is nillable (so Merge's
// IsNil reflection is valid). Idempotent via the checked set — a group reached
// by multiple spine paths (e.g. NotifyConfig on both AgentConfig and
// PlatformConfig) is verified once.
func checkNillable(t *testing.T, rt reflect.Type, checked map[reflect.Type]bool) {
	if checked[rt] {
		return
	}
	checked[rt] = true
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		switch f.Type.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			// nillable — Merge can IsNil() it
		default:
			t.Errorf("%s.%s is %s; config-cascade struct fields must be pointer/slice/map (Merge[T] panics on IsNil otherwise)",
				rt.Name(), f.Name, f.Type.Kind())
		}
	}
}

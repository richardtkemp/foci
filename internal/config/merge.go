package config

import "reflect"

// Merge resolves a struct by picking the first non-nil value for each field
// across the provided configs (most-specific first).
// Every field of T must be a nillable type (pointer, slice, or map).
//
// This is the generic building block for the 5-level config cascade:
//
//	per-platform per-agent → per-agent → per-platform global → global config → code default
//
// Code defaults are not handled here — use accessor methods on the returned
// struct (e.g. NotifyConfig.StartupNotifyEnabled()) for that final tier.
// DerefStr returns the string a *string points to, or "" if nil.
func DerefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// DerefInt returns the int a *int points to, or 0 if nil.
func DerefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// DerefBool returns the bool a *bool points to, or false if nil.
func DerefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// DerefInt64 returns the int64 a *int64 points to, or 0 if nil.
func DerefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// DerefFloat returns the float64 a *float64 points to, or 0 if nil.
func DerefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// Ptr returns a pointer to v. Useful for constructing config literals in tests.
func Ptr[T any](v T) *T { return &v }

// First returns the first non-nil pointer from the arguments.
// Useful for cascading individual pointer fields across config levels.
func First[T any](ptrs ...*T) *T {
	for _, p := range ptrs {
		if p != nil {
			return p
		}
	}
	return nil
}

// SuperveneSlice merges two slices with supervening semantics: the result
// starts from global, with agent entries overriding matching global entries
// by key. Agent entries whose keys don't appear in global are appended.
// The keyFn extracts the matching key from each element.
func SuperveneSlice[T any](agent, global []T, keyFn func(T) string) []T {
	if len(agent) == 0 {
		return global
	}
	if len(global) == 0 {
		return agent
	}
	agentByKey := make(map[string]T, len(agent))
	for _, a := range agent {
		agentByKey[keyFn(a)] = a
	}
	consumed := make(map[string]bool, len(agent))
	result := make([]T, 0, len(global)+len(agent))
	for _, g := range global {
		k := keyFn(g)
		if override, ok := agentByKey[k]; ok {
			result = append(result, override)
			consumed[k] = true
		} else {
			result = append(result, g)
		}
	}
	for _, a := range agent {
		if !consumed[keyFn(a)] {
			result = append(result, a)
		}
	}
	return result
}

func Merge[T any](configs ...T) T {
	var result T
	rv := reflect.ValueOf(&result).Elem()
	for _, c := range configs {
		cv := reflect.ValueOf(c)
		for i := 0; i < rv.NumField(); i++ {
			rf := rv.Field(i)
			cf := cv.Field(i)
			if rf.IsNil() && !cf.IsNil() {
				rf.Set(cf)
			}
		}
	}
	return result
}

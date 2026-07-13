package main

import (
	"testing"

	"foci/internal/config"
)

// TestLiveApplyCoversHotFields keeps the applier address lists and the `hot`
// struct tags (internal/config/types.go) in lockstep: a hot-tagged registry
// row without an applier would claim live-appliability it doesn't have, and
// an applier for a restart-tagged row would apply silently while the UI says
// "restart required".
func TestLiveApplyCoversHotFields(t *testing.T) {
	covered := map[string]bool{}
	for _, addrs := range [][]string{liveApplyLoggingAddrs, liveApplyDebugAddrs, liveApplyPeriodicAddrs} {
		for _, a := range addrs {
			if covered[a] {
				t.Errorf("duplicate applier address %s", a)
			}
			covered[a] = true
		}
	}

	hot := map[string]bool{}
	for _, f := range config.AllFields() {
		if !f.NeedsRestart {
			hot[f.Section+"."+f.Key] = true
		}
	}

	for a := range covered {
		if !hot[a] {
			t.Errorf("applier covers %s but its registry row is not hot-tagged", a)
		}
	}
	for h := range hot {
		if !covered[h] {
			t.Errorf("registry row %s is hot-tagged but has no applier", h)
		}
	}
}

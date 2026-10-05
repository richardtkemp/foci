package main

import (
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/delegator"
)

// TestBackendSpecs_AgreeWithGatewaySwitches holds each backend's Spec and the
// gateway's backend-name switches it will replace (#2154 Phase 2) to the same
// answers until the switches are deleted.
func TestBackendSpecs_AgreeWithGatewaySwitches(t *testing.T) {
	specs := delegator.Specs()
	if len(specs) == 0 {
		t.Fatal("no backend registered: the gateway must import internal/delegator/all")
	}
	for _, s := range specs {
		if got := backendDefaultModel(s.Name); got != s.DefaultModel {
			t.Errorf("%s: backendDefaultModel = %q, Spec.DefaultModel = %q", s.Name, got, s.DefaultModel)
		}
		gwRetention, specRetention := resumeRetentionFor(s.Name), time.Duration(0)
		if s.ResumeRetention != nil {
			specRetention = s.ResumeRetention()
		}
		if gwRetention != specRetention {
			t.Errorf("%s: resumeRetentionFor = %v, Spec.ResumeRetention = %v", s.Name, gwRetention, specRetention)
		}
		if got, want := transcriptCheckerFor(s.Name) != nil, s.TranscriptChecker != nil; got != want {
			t.Errorf("%s: transcriptCheckerFor set = %v, Spec.TranscriptChecker set = %v", s.Name, got, want)
		}
		postTool, preAnswer := nudgeCapabilities(config.AgentConfig{ID: "a", Backend: s.Name})
		if postTool != s.Supports(delegator.CapPostToolNudge) || preAnswer != s.Supports(delegator.CapPreAnswerNudge) {
			t.Errorf("%s: nudgeCapabilities = (%v, %v), Spec says (%v, %v)", s.Name, postTool, preAnswer,
				s.Supports(delegator.CapPostToolNudge), s.Supports(delegator.CapPreAnswerNudge))
		}
		// checkDelegatedReadiness skips the probe by name for opencode only.
		if probed := s.Name != "opencode"; probed != s.Supports(delegator.CapUnstartedReadinessProbe) {
			t.Errorf("%s: readiness probe run = %v, Spec unstarted_readiness_probe = %v", s.Name, probed, s.Supports(delegator.CapUnstartedReadinessProbe))
		}
		// configureDelegated folds [cc_backend] for claude-code and
		// [opencode_backend] for opencode, by name.
		fold := map[string]string{"claude-code": "claude-code", "opencode": "opencode"}[s.Name]
		if fold != s.ConfigFamily {
			t.Errorf("%s: configureDelegated folds %q, Spec.ConfigFamily = %q", s.Name, fold, s.ConfigFamily)
		}
	}
}

package delegator

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// specFake is a Delegator with no optional interface; specFakeControl adds
// ControlSender. Neither is ever called: Validate reads method sets only.
type specFake struct{ Delegator }

type specFakeControl struct{ Delegator }

func (*specFakeControl) SendControl(context.Context, ControlRequest) error { return nil }

// validSpec declares every capability No, which is consistent with a
// prototype that implements no optional interface.
func validSpec() Spec {
	caps := make(map[Capability]Support, numCapabilities)
	for _, c := range AllCapabilities() {
		caps[c] = No("test fake")
	}
	return Spec{
		Name:         "fake",
		DisplayName:  "Fake",
		New:          func(map[string]any) (Delegator, error) { return &specFake{}, nil },
		Prototype:    (*specFake)(nil),
		Caps:         caps,
		ModelcapsKey: "fake",
		LedgerKey:    "fake",
	}
}

func TestCapabilityTableComplete(t *testing.T) {
	names := map[string]Capability{}
	for _, c := range AllCapabilities() {
		info := c.Info()
		if info.Name == "" || info.Doc == "" || info.Kind == 0 {
			t.Errorf("capability %d has an incomplete capabilityTable row: %+v", int(c), info)
			continue
		}
		if prev, dup := names[info.Name]; dup {
			t.Errorf("capabilities %d and %d share the name %q", int(prev), int(c), info.Name)
		}
		names[info.Name] = c
		switch {
		case info.Kind == KindInterface && (info.Iface == nil || info.Iface.Kind() != reflect.Interface):
			t.Errorf("%s: KindInterface needs an interface Iface, got %v", c, info.Iface)
		case info.Kind != KindInterface && info.Iface != nil:
			t.Errorf("%s: only KindInterface rows carry an Iface", c)
		}
	}
}

func TestSpecValidate_ValidSpecPasses(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// Each arm breaks one rule and must fail for that rule's reason.
func TestSpecValidate_FailArms(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"undeclared capability", func(s *Spec) { delete(s.Caps, CapStreaming) }, "does not declare capability streaming"},
		{"no reason", func(s *Spec) { s.Caps[CapStreaming] = No("") }, "streaming unsupported without a reason"},
		{"n/a no reason", func(s *Spec) { s.Caps[CapStreaming] = NotApplicable("") }, "streaming unsupported without a reason"},
		{"unknown capability", func(s *Spec) { s.Caps[numCapabilities] = Yes() }, "unknown capability"},
		{"claims an unimplemented interface", func(s *Spec) { s.Caps[CapControl] = Yes() }, "declares control but"},
		{"hides an implemented interface", func(s *Spec) { s.Prototype = (*specFakeControl)(nil) }, "does not declare control"},
		{"requires", func(s *Spec) {
			s.Prototype = (*specFakeControl)(nil)
			s.Caps[CapControl] = No("x")
			s.Caps[CapControlModel] = Yes()
		}, "control_model, which requires control"},
		{"plan mode without delivery", func(s *Spec) { s.Caps[CapPlanMode] = Yes() }, "plan_mode must be declared Yes exactly when PlanDelivery"},
		{"delivery without plan mode", func(s *Spec) {
			s.PlanDelivery = func(context.Context, PlanDeps, string) (string, error) { return "", nil }
		}, "plan_mode must be declared Yes exactly when PlanDelivery"},
		{"usage without query", func(s *Spec) { s.Caps[CapUsageQuery] = Yes() }, "usage_query must be declared Yes exactly when UsageQuery"},
		{"transcript checker without tracking", func(s *Spec) {
			s.TranscriptChecker = func(string, string, string) (bool, error) { return false, nil }
		}, "delivery_tracking must be declared Yes exactly when TranscriptChecker"},
		{"fork needs running without branch", func(s *Spec) { s.ForkNeedsRunning = true }, "ForkNeedsRunning without branch"},
		{"no name", func(s *Spec) { s.Name = "" }, "empty Name"},
		{"no display name", func(s *Spec) { s.DisplayName = "" }, "empty DisplayName"},
		{"no constructor", func(s *Spec) { s.New = nil }, "nil New"},
		{"no prototype", func(s *Spec) { s.Prototype = nil }, "nil Prototype"},
		{"no ledger key", func(s *Spec) { s.LedgerKey = "" }, "empty LedgerKey"},
		{"no modelcaps key", func(s *Spec) { s.ModelcapsKey = "" }, "empty ModelcapsKey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			tt.mutate(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "internal/delegator/capabilities.go") {
				t.Errorf("error does not point at capabilities.go: %v", err)
			}
		})
	}
}

func TestSpecSupports(t *testing.T) {
	s := validSpec()
	s.Prototype = (*specFakeControl)(nil)
	s.Caps[CapControl] = Yes()
	s.Caps[CapStreaming] = NotApplicable("n/a")
	if !s.Supports(CapControl) {
		t.Error("Supports(control) = false for a Yes")
	}
	if s.Supports(CapStreaming) || s.Supports(CapPreAnswerNudge) {
		t.Error("Supports is true for NotApplicable or No")
	}
}

// TestRegister_InvalidAndDuplicate pins #2154 Q1: an invalid Spec is still
// registered (the gateway logs, the delegator/all tests fail), and a second
// registration under a taken name is ignored.
func TestRegister_InvalidAndDuplicate(t *testing.T) {
	t.Cleanup(func() {
		registryMu.Lock()
		delete(specs, "fake-register")
		registryMu.Unlock()
	})
	first := validSpec()
	first.Name = "fake-register"
	delete(first.Caps, CapStreaming) // invalid
	Register(first)
	got, ok := SpecFor("fake-register")
	if !ok || got.DisplayName != "Fake" {
		t.Fatalf("invalid Spec not registered: ok=%v %+v", ok, got)
	}
	second := validSpec()
	second.Name = "fake-register"
	second.DisplayName = "Second"
	Register(second)
	if got, _ := SpecFor("fake-register"); got.DisplayName != "Fake" {
		t.Errorf("duplicate registration replaced the first: DisplayName = %q", got.DisplayName)
	}
	if got := HumanReadableBackendName("fake-register"); got != "Fake" {
		t.Errorf("HumanReadableBackendName = %q, want the Spec's DisplayName", got)
	}
}

func TestHumanReadableBackendName_Fallbacks(t *testing.T) {
	if got := HumanReadableBackendName(""); got != "the delegated backend" {
		t.Errorf(`HumanReadableBackendName("") = %q`, got)
	}
	if got := HumanReadableBackendName("some-future-backend"); got != "some-future-backend" {
		t.Errorf("unregistered name = %q, want it back verbatim", got)
	}
}

type notACapability interface{ NotACapability() }

func TestAs(t *testing.T) {
	var be Delegator = &specFakeControl{}
	if _, ok := As[ControlSender](be); !ok {
		t.Error("As[ControlSender] = false on a ControlSender")
	}
	if _, ok := As[VoiceModer](be); ok {
		t.Error("As[VoiceModer] = true on a backend without it")
	}
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(r.(string), "capabilities.go") {
			t.Errorf("As on an unregistered interface: recover() = %v, want a panic naming capabilities.go", r)
		}
	}()
	As[notACapability](be)
}

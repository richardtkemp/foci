package app

import (
	"reflect"
	"strings"
	"testing"

	"foci/internal/fap"
)

// valueClient is a live socket whose hello advertised askgw prompts AND the
// value step (#3090).
func valueClient(h *Hub, device string) *wsClient {
	c := deviceClient(h, device)
	c.features = featureSet([]string{featureAskgwPrompts, featureAskgwValueInput})
	return c
}

var secretValueInput = &fap.ValueInput{Choice: "opt:0", Label: "hetzner.api_key", CancelData: "cancel-value"}

// A value prompt reaches only devices that render the value step; an older
// askgw client gets the set without it, so its dialog never answers without
// the value. A plain askgw prompt still reaches both.
func TestPresentAppPrompt_ValuePromptOnlyToValueCapable(t *testing.T) {
	h := newTestHub()
	newer, older := valueClient(h, "phone"), askgwClient(h, "mac")

	if err := h.presentAppPrompt("s1", "Set secret", "x", "", askgwButtons, secretValueInput, func(string, string) {}); err != nil {
		t.Fatalf("present: %v", err)
	}
	if got := askgwSnapshots(t, newer); !reflect.DeepEqual(got, [][]string{{"s1"}}) {
		t.Errorf("value-capable snapshots = %v, want [[s1]]", got)
	}
	if got := askgwSnapshots(t, older); !reflect.DeepEqual(got, [][]string{{}}) {
		t.Errorf("older client snapshots = %v, want [[]] (prompt withheld)", got)
	}

	h.presentAskgw("a1", "Sudo", "y", "", askgwButtons, func(string) {})
	if got := askgwSnapshots(t, newer); !reflect.DeepEqual(got, [][]string{{"s1", "a1"}}) {
		t.Errorf("value-capable snapshots = %v, want [[s1 a1]]", got)
	}
	if got := askgwSnapshots(t, older); !reflect.DeepEqual(got, [][]string{{"a1"}}) {
		t.Errorf("older client snapshots = %v, want [[a1]]", got)
	}
}

// The value-input frame carries the step, so the client can render it.
func TestPresentAppPrompt_FrameCarriesValueInput(t *testing.T) {
	h := newTestHub()
	c := valueClient(h, "phone")
	if err := h.presentAppPrompt("s1", "Set secret", "x", "", askgwButtons, secretValueInput, func(string, string) {}); err != nil {
		t.Fatalf("present: %v", err)
	}
	frames := drain(t, c)
	p := frames[0].d["prompts"].([]any)[0].(map[string]any)
	vi, ok := p["valueInput"].(map[string]any)
	if !ok || vi["choice"] != "opt:0" || vi["label"] != "hetzner.api_key" || vi["cancelData"] != "cancel-value" {
		t.Fatalf("valueInput = %v", p["valueInput"])
	}
}

// With no device able to show it, an app prompt is refused with a reason and
// nothing is registered: there is no chat fallback (Dick 2026-11-13: app only).
func TestPresentAppPrompt_NoCapableDeviceRefuses(t *testing.T) {
	h := newTestHub()
	askgwClient(h, "mac") // can show plain prompts, not the value step
	err := h.presentAppPrompt("s1", "Set secret", "x", "", askgwButtons, secretValueInput, func(string, string) {})
	if err == nil || !strings.Contains(err.Error(), featureAskgwValueInput) {
		t.Fatalf("err = %v, want a refusal naming %s", err, featureAskgwValueInput)
	}
	if got := h.askgw.snapshot(true).Prompts; len(got) != 0 {
		t.Errorf("refused prompt registered: %v", got)
	}

	// A plain app prompt (no value step) needs only askgw support.
	if err := h.presentAppPrompt("s2", "Remove secret", "x", "", askgwButtons, nil, func(string, string) {}); err != nil {
		t.Errorf("plain app prompt refused: %v", err)
	}
}

// The value travels with the value choice only; any other shape leaves the
// prompt pending (an owner could not resolve it).
func TestAskgwResponse_ValueRules(t *testing.T) {
	cases := []struct {
		name        string
		data, value string
		taken       bool
	}{
		{"value choice with value", "opt:0", "s3cret", true},
		{"value choice without value", "opt:0", "", false},
		{"cancel without value", "cancel-value", "", true},
		{"cancel with value", "cancel-value", "s3cret", false},
		{"plain choice", "cancel", "", true},
		{"plain choice with value", "cancel", "s3cret", false},
		{"unknown choice", "nope", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub()
			c := valueClient(h, "phone")
			var gotData, gotValue string
			calls := 0
			if err := h.presentAppPrompt("s1", "Set secret", "x", "", askgwButtons, secretValueInput, func(d, v string) {
				calls++
				gotData, gotValue = d, v
			}); err != nil {
				t.Fatalf("present: %v", err)
			}
			drain(t, c)
			h.handleAskgwResponse(c, fap.AskgwResponse{PromptID: "s1", Data: tc.data, Value: tc.value})
			if tc.taken {
				if calls != 1 || gotData != tc.data || gotValue != tc.value {
					t.Fatalf("callback calls=%d data=%q value given=%t, want 1 %q %t", calls, gotData, gotValue != "", tc.data, tc.value != "")
				}
				if n := len(h.askgw.snapshot(true).Prompts); n != 0 {
					t.Errorf("prompt still pending after a valid answer")
				}
				return
			}
			if calls != 0 {
				t.Fatalf("invalid answer fired the callback")
			}
			if n := len(h.askgw.snapshot(true).Prompts); n != 1 {
				t.Errorf("invalid answer consumed the prompt")
			}
		})
	}
}

// A plain askgw prompt (no value step) refuses an answer carrying a value.
func TestAskgwResponse_PlainPromptRefusesValue(t *testing.T) {
	h := newTestHub()
	c := askgwClient(h, "mac")
	fired := false
	h.presentAskgw("a1", "Sudo", "x", "", askgwButtons, func(string) { fired = true })
	drain(t, c)
	h.handleAskgwResponse(c, fap.AskgwResponse{PromptID: "a1", Data: "opt:0", Value: "x"})
	if fired {
		t.Fatal("a plain prompt took an answer with a value")
	}
}

// A device whose latest hello advertised the value step counts for a push
// wake; one that did not is never woken for a value prompt.
func TestPushTokens_AskgwValueTargets(t *testing.T) {
	p := newPushTokens()
	p.set("new", "tok-new")
	p.set("old", "tok-old")
	p.setAskgw("new", true)
	p.setAskgw("old", true)
	p.setAskgwValue("new", true)
	p.setAskgwValue("old", false)
	got := p.askgwValueTargetsExcluding(nil)
	if !reflect.DeepEqual(got, map[string]string{"new": "tok-new"}) {
		t.Fatalf("value targets = %v, want only new", got)
	}
}

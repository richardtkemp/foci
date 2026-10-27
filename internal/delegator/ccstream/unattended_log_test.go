package ccstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"foci/internal/delegator"
	"foci/internal/log"
)

// captureLog routes the process-global logger to a buffer for one test.
// Callers must not be parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(nil) })
	return &out
}

// warnLines returns the WARN lines containing substr.
func warnLines(out, substr string) []string {
	var got []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "WARN") && strings.Contains(l, substr) {
			got = append(got, l)
		}
	}
	return got
}

// The wire shape of a classifier denial, from the 2.1.311 probe.
const deniedRaw = `{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_018L6MYBxNzLewe2HFxjfqgV","decision_reason_type":"classifier","decision_reason":"[Data Exfiltration]","message":"Permission for this action was denied by the Claude Code auto mode classifier.","uuid":"d434a5c9","session_id":"ab3c871d"}`

// TestOnSystem_PermissionDenied_WarnsWhenUnattended: a tool call CC denied on
// its own is logged as an unattended denial (session, trigger, tool, reason
// type and reason) in a skip/batch session and in an unattended turn, and not
// in an attended session, which sees the denial in its chat.
// Not parallel: log.SetOutput is process-global.
func TestOnSystem_PermissionDenied_WarnsWhenUnattended(t *testing.T) {
	for _, tc := range []struct {
		name      string
		skip      bool
		turn      *delegator.TurnEvents
		wantLabel string // "" = no WARN expected
	}{
		{"batch session", true, nil, delegator.UnattendedSkipLabel},
		{"unattended turn", false, &delegator.TurnEvents{Trigger: "reflection", Unattended: true}, "reflection"},
		{"attended turn", false, &delegator.TurnEvents{Trigger: "app"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureLog(t)
			b := &Backend{skipPermissions: tc.skip, turnEvents: tc.turn}
			b.startOpts.SessionKey = "cam/b1"
			b.OnSystem("permission_denied", json.RawMessage(deniedRaw))
			got := warnLines(out.String(), "not by foci's rules or a user")
			if tc.wantLabel == "" {
				if len(got) != 0 {
					t.Fatalf("attended session logged an unattended denial: %q", got)
				}
				return
			}
			want := "unattended: session cam/b1 (trigger=" + tc.wantLabel + ") tried to use Bash; blocked by Claude Code's auto mode classifier, not by foci's rules or a user (classifier: [Data Exfiltration])"
			if len(got) != 1 || !strings.Contains(got[0], want) {
				t.Fatalf("WARN lines = %q, want one containing %q", got, want)
			}
		})
	}
}

// TestOnSystem_InitPermissionModeMismatch_Warns: CC silently starts in another
// mode when the model lacks the requested one (claude-haiku-4-5 asked for
// auto starts in "default"). foci WARNs on a mismatch, and stays quiet when
// the mode matches or when foci requested none.
// Not parallel: log.SetOutput is process-global.
func TestOnSystem_InitPermissionModeMismatch_Warns(t *testing.T) {
	for _, tc := range []struct {
		name, requested, reported string
		wantWarn                  bool
	}{
		{"auto fell back to default", "auto", "default", true},
		{"auto honoured", "auto", "auto", false},
		{"no mode requested", "", "default", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureLog(t)
			b := &Backend{requestedPermMode: tc.requested, readyCh: make(chan struct{})}
			b.startOpts.SessionKey = "cam/batch-1"
			raw := `{"type":"system","subtype":"init","claude_code_version":"2.1.311","model":"claude-haiku-4-5","permissionMode":"` + tc.reported + `","session_id":"s1"}`
			b.OnSystem("init", json.RawMessage(raw))
			got := warnLines(out.String(), "asked Claude Code for permission mode")
			if !tc.wantWarn {
				if len(got) != 0 {
					t.Fatalf("unexpected WARN: %q", got)
				}
				return
			}
			want := `session cam/batch-1: asked Claude Code for permission mode "auto", but it started in "default" (model claude-haiku-4-5)`
			if len(got) != 1 || !strings.Contains(got[0], want) {
				t.Fatalf("WARN lines = %q, want one containing %q", got, want)
			}
		})
	}
}

// TestRequestedPermissionMode: the mode foci expects CC to report is read
// from the launch args it built; --dangerously-skip-permissions sets none.
func TestRequestedPermissionMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cfgSkip, batch bool
		want           string
	}{
		{false, false, ""},
		{false, true, "auto"},
		{true, false, ""},
		{true, true, ""},
	} {
		if got := requestedPermissionMode(unattendedPermissionArgs(tc.cfgSkip, tc.batch)); got != tc.want {
			t.Errorf("cfgSkip=%v batch=%v: requested mode %q, want %q", tc.cfgSkip, tc.batch, got, tc.want)
		}
	}
}

// TestCCDeniedLog_NamesTheDenier: the WARN line says which part of Claude Code
// refused (auto mode classifier, built-in safety check, or CC generically for
// an unknown type) and that foci's rules and the user did not, so a reader can
// tell a CC denial from a foci rule denial.
func TestCCDeniedLog_NamesTheDenier(t *testing.T) {
	for _, tc := range []struct{ reasonType, want string }{
		{"classifier", "blocked by Claude Code's auto mode classifier, not by foci's rules or a user"},
		{"safetyCheck", "blocked by Claude Code's built-in safety check, not by foci's rules or a user"},
		{"somethingNew", "blocked by Claude Code itself, not by foci's rules or a user"},
	} {
		got := ccDeniedLog("cam/b1", "reflection", PermissionDeniedMessage{ToolName: "Bash", DecisionReasonType: tc.reasonType, DecisionReason: "r"})
		if !strings.Contains(got, tc.want) {
			t.Errorf("reasonType %q: got %q, want it to contain %q", tc.reasonType, got, tc.want)
		}
	}
}

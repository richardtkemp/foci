package command

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"foci/internal/agent"
	"foci/internal/delegator"
	_ "foci/internal/delegator/all"
	"foci/internal/tools"
)

// TestModeCommand_BackendChoices proves menu, parsing and help all follow the
// backend declaration, and that an unconfigured state does not claim normal.
func TestModeCommand_BackendChoices(t *testing.T) {
	ctx := tools.WithSessionKey(context.Background(), "mode-test")
	for _, tc := range []struct {
		backend   string
		names     []string
		autoValue string
	}{
		{"claude-code", []string{"normal", "accept", "plan", "auto"}, "auto"},
		{"codex", []string{"normal", "auto"}, "auto_review"},
		{"opencode", []string{"normal", "accept", "plan", "allow"}, ""},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			spec, ok := delegator.SpecFor(tc.backend)
			if !ok {
				t.Fatal("missing backend Spec")
			}
			cc := modelCC(&agent.Agent{Backend: tc.backend, DelegatedManager: &agent.DelegatedManager{Spec: spec}})
			cmd := ModeCommand()
			if !cmd.Visible(ctx, Request{}, cc) {
				t.Fatal("declared /mode must be visible")
			}
			var names []string
			for _, option := range cmd.KeyboardOptions(ctx, cc) {
				names = append(names, option.Label)
			}
			if !reflect.DeepEqual(names, tc.names) {
				t.Fatalf("buttons=%v want %v", names, tc.names)
			}
			response, err := cmd.Execute(ctx, Request{}, cc)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(response.Text, "backend default (not overridden)") {
				t.Fatalf("status=%q", response.Text)
			}
			for _, name := range tc.names {
				if !strings.Contains(response.Text, name) {
					t.Fatalf("missing %q in help %q", name, response.Text)
				}
			}
			mode, ok := delegator.ResolvePermissionMode(spec.PermissionModes, "auto")
			if tc.autoValue == "" {
				if ok {
					t.Fatal("opencode must not advertise classifier auto-review it does not implement")
				}
			} else if !ok || mode.Value != tc.autoValue {
				t.Fatalf("auto resolves to %+v, %v", mode, ok)
			}
			if tc.backend == "codex" {
				mode, ok = delegator.ResolvePermissionMode(spec.PermissionModes, "auto_review")
				if !ok || mode.Value != "auto_review" {
					t.Fatal("Codex native reviewer alias must work")
				}
				response, err = cmd.Execute(ctx, Request{Args: "plan"}, cc)
				if err != nil || !strings.Contains(response.Text, "Invalid permission mode") || strings.Contains(response.Text, "3)") {
					t.Fatalf("Codex rejection/help=%q, %v", response.Text, err)
				}
			}
		})
	}
}

// TestModeCommand_Unavailable covers API and undeclared backends, including
// keyboard rendering without an agent, rather than leaking CC-only buttons.
func TestModeCommand_Unavailable(t *testing.T) {
	cmd := ModeCommand()
	for _, cc := range []CommandContext{{}, modelCC(&agent.Agent{}), modelCC(&agent.Agent{DelegatedManager: &agent.DelegatedManager{}})} {
		if cmd.Visible(context.Background(), Request{}, cc) {
			t.Fatal("unsupported command visible")
		}
		if len(cmd.KeyboardOptions(context.Background(), cc)) != 0 {
			t.Fatal("unsupported command has buttons")
		}
		response, err := cmd.Execute(context.Background(), Request{Args: "auto"}, cc)
		if err != nil || !strings.Contains(response.Text, "not supported") {
			t.Fatalf("response=%q err=%v", response.Text, err)
		}
	}
}

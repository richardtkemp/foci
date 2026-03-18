package command

import (
	"context"
	"strings"
	"testing"
)

// subCmdCC returns a minimal CommandContext for subcommand tests.
func subCmdCC() CommandContext {
	return CommandContext{}
}

// TestSubcommandDispatchRoutes verifies that dispatch routes to the correct
// subcommand and strips the subcommand name from req.Args.
func TestSubcommandDispatchRoutes(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&Command{
		Name: "test",
		Subcommands: []Subcommand{
			{
				Name: "alpha",
				Execute: func(_ context.Context, req Request, _ CommandContext) (Response, error) {
					return Response{Text: "alpha:" + req.Args}, nil
				},
			},
			{
				Name: "beta",
				Execute: func(_ context.Context, req Request, _ CommandContext) (Response, error) {
					return Response{Text: "beta:" + req.Args}, nil
				},
			},
		},
	})

	resp, ok, err := reg.Dispatch(context.Background(), Request{Name: "test", Args: "alpha extra args"}, subCmdCC())
	if err != nil || !ok {
		t.Fatalf("dispatch failed: ok=%v err=%v", ok, err)
	}
	if resp.Text != "alpha:extra args" {
		t.Errorf("expected alpha:extra args, got %q", resp.Text)
	}

	resp, _, _ = reg.Dispatch(context.Background(), Request{Name: "test", Args: "beta"}, subCmdCC())
	if resp.Text != "beta:" {
		t.Errorf("expected beta:, got %q", resp.Text)
	}
}

// TestSubcommandAutoKeyboard verifies that auto-generated keyboard matches
// non-hidden, visible subcommands in declaration order.
func TestSubcommandAutoKeyboard(t *testing.T) {
	cmd := &Command{
		Name: "test",
		Subcommands: []Subcommand{
			{Name: "one", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil }},
			{Name: "two", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil }},
			{Name: "three", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil }},
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	opts := cmd.KeyboardOptions(context.Background(), subCmdCC())
	if len(opts) != 3 {
		t.Fatalf("expected 3 keyboard options, got %d", len(opts))
	}
	if opts[0].Label != "one" || opts[1].Label != "two" || opts[2].Label != "three" {
		t.Errorf("unexpected labels: %v, %v, %v", opts[0].Label, opts[1].Label, opts[2].Label)
	}
	for _, o := range opts {
		if o.Data != o.Label {
			t.Errorf("expected Data=Label for %q, got Data=%q", o.Label, o.Data)
		}
	}
}

// TestSubcommandHiddenExcludedFromKeyboard verifies that a hidden subcommand
// dispatches correctly but is excluded from the auto-generated keyboard.
func TestSubcommandHiddenExcludedFromKeyboard(t *testing.T) {
	cmd := &Command{
		Name: "test",
		Subcommands: []Subcommand{
			{Name: "visible", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{Text: "v"}, nil }},
			{Name: "hidden", Hidden: true, Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{Text: "h"}, nil }},
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	// Keyboard should only have "visible"
	opts := cmd.KeyboardOptions(context.Background(), subCmdCC())
	if len(opts) != 1 || opts[0].Label != "visible" {
		t.Errorf("expected only visible in keyboard, got %v", opts)
	}

	// But dispatch to hidden should still work
	resp, _, _ := reg.Dispatch(context.Background(), Request{Name: "test", Args: "hidden"}, subCmdCC())
	if resp.Text != "h" {
		t.Errorf("expected hidden dispatch to work, got %q", resp.Text)
	}
}

// TestSubcommandVisibleFunc verifies that the Visible func controls whether
// a subcommand appears in the keyboard dynamically.
func TestSubcommandVisibleFunc(t *testing.T) {
	show := true
	cmd := &Command{
		Name: "test",
		Subcommands: []Subcommand{
			{Name: "always", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil }},
			{
				Name:    "conditional",
				Visible: func(_ context.Context, _ CommandContext) bool { return show },
				Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil },
			},
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	opts := cmd.KeyboardOptions(context.Background(), subCmdCC())
	if len(opts) != 2 {
		t.Fatalf("expected 2 options when visible=true, got %d", len(opts))
	}

	show = false
	opts = cmd.KeyboardOptions(context.Background(), subCmdCC())
	if len(opts) != 1 || opts[0].Label != "always" {
		t.Errorf("expected only 'always' when visible=false, got %v", opts)
	}
}

// TestSubcommandAliases verifies that aliases dispatch correctly but aren't
// shown in the keyboard.
func TestSubcommandAliases(t *testing.T) {
	cmd := &Command{
		Name: "test",
		Subcommands: []Subcommand{
			{
				Name:    "list",
				Aliases: []string{"ls", "l"},
				Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
					return Response{Text: "listed"}, nil
				},
			},
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	// Aliases should dispatch
	for _, alias := range []string{"list", "ls", "l"} {
		resp, _, _ := reg.Dispatch(context.Background(), Request{Name: "test", Args: alias}, subCmdCC())
		if resp.Text != "listed" {
			t.Errorf("alias %q: expected 'listed', got %q", alias, resp.Text)
		}
	}

	// Keyboard should only show "list", not aliases
	opts := cmd.KeyboardOptions(context.Background(), subCmdCC())
	if len(opts) != 1 || opts[0].Label != "list" {
		t.Errorf("expected only 'list' in keyboard, got %v", opts)
	}
}

// TestSubcommandUnknownAndEmpty verifies that unknown subcommands and empty
// args both return the auto-generated usage text.
func TestSubcommandUnknownAndEmpty(t *testing.T) {
	cmd := &Command{
		Name: "test",
		Subcommands: []Subcommand{
			{Name: "one", Description: "First subcommand", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil }},
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	// Empty args
	resp, _, _ := reg.Dispatch(context.Background(), Request{Name: "test", Args: ""}, subCmdCC())
	if !strings.Contains(resp.Text, "Usage: /test") {
		t.Errorf("empty args should show usage, got %q", resp.Text)
	}
	if !strings.Contains(resp.Text, "one") {
		t.Errorf("usage should mention subcommand 'one', got %q", resp.Text)
	}

	// Unknown subcommand
	resp, _, _ = reg.Dispatch(context.Background(), Request{Name: "test", Args: "bogus"}, subCmdCC())
	if !strings.Contains(resp.Text, "Usage: /test") {
		t.Errorf("unknown subcommand should show usage, got %q", resp.Text)
	}
}

// TestSubcommandLabelOverride verifies that Label is used in the keyboard
// instead of Name when set.
func TestSubcommandLabelOverride(t *testing.T) {
	cmd := &Command{
		Name: "test",
		Subcommands: []Subcommand{
			{
				Name:  "run",
				Label: "execute now",
				Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
					return Response{Text: "ran"}, nil
				},
			},
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	opts := cmd.KeyboardOptions(context.Background(), subCmdCC())
	if len(opts) != 1 {
		t.Fatalf("expected 1 keyboard option, got %d", len(opts))
	}
	if opts[0].Label != "execute now" {
		t.Errorf("expected label 'execute now', got %q", opts[0].Label)
	}
	// Data should still use Name for dispatch
	if opts[0].Data != "run" {
		t.Errorf("expected data 'run', got %q", opts[0].Data)
	}
}

// TestSubcommandExplicitKeyboardTakesPrecedence verifies that an explicitly
// set KeyboardOptions func is not overwritten by auto-generation.
func TestSubcommandExplicitKeyboardTakesPrecedence(t *testing.T) {
	cmd := &Command{
		Name: "test",
		Subcommands: []Subcommand{
			{Name: "alpha", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil }},
			{Name: "beta", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{}, nil }},
		},
		KeyboardOptions: func(_ context.Context, _ CommandContext) []KeyboardOption {
			return []KeyboardOption{{Label: "custom", Data: "custom"}}
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	opts := cmd.KeyboardOptions(context.Background(), subCmdCC())
	if len(opts) != 1 || opts[0].Label != "custom" {
		t.Errorf("explicit KeyboardOptions should take precedence, got %v", opts)
	}

	// Dispatch should still work via subcommands
	resp, _, _ := reg.Dispatch(context.Background(), Request{Name: "test", Args: "alpha"}, subCmdCC())
	if resp.Text != "" {
		t.Errorf("expected empty response from alpha, got %q", resp.Text)
	}
}

// TestSubcommandDefaultExecute verifies that DefaultExecute is called when
// no subcommand matches — both for unknown args and empty args.
func TestSubcommandDefaultExecute(t *testing.T) {
	cmd := &Command{
		Name: "test",
		Subcommands: []Subcommand{
			{Name: "named", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{Text: "named"}, nil }},
		},
		DefaultExecute: func(_ context.Context, req Request, _ CommandContext) (Response, error) {
			return Response{Text: "default:" + req.Args}, nil
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	// Named subcommand still works
	resp, _, _ := reg.Dispatch(context.Background(), Request{Name: "test", Args: "named"}, subCmdCC())
	if resp.Text != "named" {
		t.Errorf("expected 'named', got %q", resp.Text)
	}

	// Unknown falls to DefaultExecute with full original args
	resp, _, _ = reg.Dispatch(context.Background(), Request{Name: "test", Args: "42"}, subCmdCC())
	if resp.Text != "default:42" {
		t.Errorf("expected 'default:42', got %q", resp.Text)
	}

	// Empty args also falls to DefaultExecute
	resp, _, _ = reg.Dispatch(context.Background(), Request{Name: "test", Args: ""}, subCmdCC())
	if resp.Text != "default:" {
		t.Errorf("expected 'default:', got %q", resp.Text)
	}
}

// TestSubcommandChainKeyboardAlongside verifies that ChainKeyboard continues
// working alongside subcommand dispatch.
func TestSubcommandChainKeyboardAlongside(t *testing.T) {
	cmd := &Command{
		Name: "test",
		Subcommands: []Subcommand{
			{Name: "sub", Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) { return Response{Text: "sub"}, nil }},
		},
		ChainKeyboard: func(_ context.Context, subcommand string, _ CommandContext) []KeyboardOption {
			if subcommand == "sub" {
				return []KeyboardOption{{Label: "chained", Data: "sub chained"}}
			}
			return nil
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	// Chain keyboard should work
	_, opts, ok := reg.LookupChainKeyboard(context.Background(), "/test sub", subCmdCC())
	if !ok || len(opts) != 1 || opts[0].Label != "chained" {
		t.Errorf("expected chain keyboard, got ok=%v opts=%v", ok, opts)
	}
}

// TestSubcommandUsageFormat verifies the auto-generated usage text includes
// all subcommand names and descriptions in the expected format.
func TestSubcommandUsageFormat(t *testing.T) {
	cmd := &Command{
		Name: "mycmd",
		Subcommands: []Subcommand{
			{Name: "list", Description: "List all items"},
			{Name: "add", Description: "Add a new item"},
			{Name: "remove", Description: "Remove an item"},
		},
	}
	reg := NewRegistry()
	reg.Register(cmd)

	resp, _, _ := reg.Dispatch(context.Background(), Request{Name: "mycmd", Args: ""}, subCmdCC())
	if !strings.Contains(resp.Text, "Usage: /mycmd [list|add|remove]") {
		t.Errorf("unexpected usage header: %q", resp.Text)
	}
	if !strings.Contains(resp.Text, "list") || !strings.Contains(resp.Text, "List all items") {
		t.Errorf("missing list description in usage: %q", resp.Text)
	}
	if !strings.Contains(resp.Text, "remove") || !strings.Contains(resp.Text, "Remove an item") {
		t.Errorf("missing remove description in usage: %q", resp.Text)
	}
}

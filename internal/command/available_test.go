package command

import (
	"context"
	"testing"
)

// availabilityRegistry registers one command per way a command can be
// unavailable, plus one that is always available.
func availabilityRegistry() *Registry {
	r := NewRegistry()
	r.Register(&Command{Name: "ok"})
	r.Register(&Command{Name: "hidden", Hidden: true})
	r.Register(&Command{Name: "backendonly", Requires: RequiresBackend})
	r.Register(&Command{Name: "gated", Visible: func(context.Context, Request, CommandContext) bool { return false }})
	r.Register(&Command{Name: "apphidden", ExcludeApp: true})
	return r
}

func infoNames(infos []CommandInfo) []string {
	out := make([]string, len(infos))
	for i, c := range infos {
		out[i] = c.Name
	}
	return out
}

// TestVisibleList_OnlyAvailable pins #898: the app palette lists only commands
// the caller can actually run. A command whose transport requirement is unmet
// (RequiresBackend on an agent with no delegated backend) used to be listed,
// then answered "requires a Claude Code backend" when picked.
func TestVisibleList_OnlyAvailable(t *testing.T) {
	got := infoNames(availabilityRegistry().VisibleList(context.Background(), Request{}, CommandContext{}))
	if len(got) != 1 || got[0] != "ok" {
		t.Errorf("VisibleList = %v, want [ok]", got)
	}
}

// TestAvailableList_KeepsExcludeApp checks the non-app listing (the Telegram
// menu): same availability rules, but ExcludeApp commands stay, since they are
// the chat platforms' substitutes for app-native controls.
func TestAvailableList_KeepsExcludeApp(t *testing.T) {
	got := infoNames(availabilityRegistry().AvailableList(context.Background(), Request{}, CommandContext{}))
	if len(got) != 2 || got[0] != "apphidden" || got[1] != "ok" {
		t.Errorf("AvailableList = %v, want [apphidden ok]", got)
	}
}

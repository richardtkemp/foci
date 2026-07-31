package app

import (
	"context"
	"testing"

	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/platform"
)

// paletteHub builds a hub whose agent has a one-command palette, plus a
// conversation bound to a fresh socket.
func paletteHub(t *testing.T, agentID, convID string, chatID int64) (*Hub, *convBinding, *wsClient) {
	t.Helper()
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: newTestIndex(t)}
	reg := command.NewRegistry()
	reg.Register(&command.Command{
		Name: "ping",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{}, nil
		},
	})
	h.agents[agentID] = &appConn{hub: h, agentID: agentID, commands: reg}
	h.agentOrder = append(h.agentOrder, agentID)

	c := fakeClientFor(h)
	b := &convBinding{
		convID:     convID,
		sessionKey: agentID + "/c" + convID,
		agentID:    agentID,
		chatID:     chatID,
		clients:    map[*wsClient]struct{}{c: {}},
	}
	h.convs[convID] = b
	h.clients[c] = struct{}{}
	return h, b, c
}

func countCommandFrames(t *testing.T, c *wsClient) int {
	t.Helper()
	n := 0
	for _, f := range drain(t, c) {
		if f.t == fap.TypeCommands {
			n++
		}
	}
	return n
}

// TestPushCommands_SendsOnlyOnChange is the core of the palette storm: the
// command palette is derived from the registry and the session's capability
// gating, neither of which moves when a socket reconnects — so re-pushing it
// unconditionally sent the same ~3.4KB frame over and over.
//
// Measured on the live install before the fix: 256,699 palette frames totalling
// 847MB, which was 87% of every byte foci had ever sent to the app, and 91% of
// them byte-identical within any given hour.
func TestPushCommands_SendsOnlyOnChange(t *testing.T) {
	h, b, c := paletteHub(t, "ag", "c1", 42)

	h.pushCommands(b)
	if got := countCommandFrames(t, c); got != 1 {
		t.Fatalf("first push sent %d command frames, want 1", got)
	}

	// Nothing changed between these — the palette must not be resent.
	h.pushCommands(b)
	h.pushCommands(b)
	if got := countCommandFrames(t, c); got != 0 {
		t.Errorf("unchanged palette resent %d time(s), want 0", got)
	}

	// A real change must still get through, or the dedup would silently freeze
	// the client's palette.
	h.agents["ag"].commands.Register(&command.Command{
		Name: "pong",
		Execute: func(_ context.Context, _ command.Request, _ command.CommandContext) (command.Response, error) {
			return command.Response{}, nil
		},
	})
	h.pushCommands(b)
	if got := countCommandFrames(t, c); got != 1 {
		t.Errorf("changed palette sent %d frames, want 1 — dedup must not freeze the palette", got)
	}
}

// TestPushRoster_DoesNotBroadcastPalettesToOtherConversations pins the
// per-client scoping. pushRoster serves ONE socket (its doc comment draws
// exactly this distinction against pushRosterAll) but used to call
// pushCommandsAll, so a single device's wifi blip rebuilt and rebroadcast the
// palette for every live conversation — 152 of them on the live install, ~520KB
// per reconnect, ~361 reconnects a day.
func TestPushRoster_DoesNotBroadcastPalettesToOtherConversations(t *testing.T) {
	h, _, mine := paletteHub(t, "ag", "c1", 42)

	// A second conversation belonging to a DIFFERENT device.
	theirs := fakeClientFor(h)
	other := &convBinding{
		convID:     "c2",
		sessionKey: "ag/c2",
		agentID:    "ag",
		chatID:     43,
		clients:    map[*wsClient]struct{}{theirs: {}},
	}
	h.convs["c2"] = other
	h.clients[theirs] = struct{}{}

	h.pushRoster(mine)

	if got := countCommandFrames(t, mine); got != 1 {
		t.Errorf("reconnecting socket got %d command frames for its own conversation, want 1", got)
	}
	if got := countCommandFrames(t, theirs); got != 0 {
		t.Errorf("unrelated conversation got %d command frames from another device's reconnect, want 0", got)
	}
}

// TestPushCommands_SkipsArchivedConversation: an archived conversation is hidden
// from the roster, so its palette can never be rendered — but the frame was
// still built, sent, and DURABLY STORED in app_frames. Long-dead branch sessions
// therefore cost storage forever.
func TestPushCommands_SkipsArchivedConversation(t *testing.T) {
	h, b, c := paletteHub(t, "ag", "c1", 42)
	if err := h.deps.SessionIndex.SetArchivedChat("ag", "app", 42, true); err != nil {
		t.Fatalf("SetArchivedChat: %v", err)
	}

	h.pushCommands(b)
	if got := countCommandFrames(t, c); got != 0 {
		t.Errorf("archived conversation got %d command frames, want 0", got)
	}

	// Unarchiving must restore delivery — the skip is a filter, not a latch.
	if err := h.deps.SessionIndex.SetArchivedChat("ag", "app", 42, false); err != nil {
		t.Fatalf("SetArchivedChat: %v", err)
	}
	h.pushCommands(b)
	if got := countCommandFrames(t, c); got != 1 {
		t.Errorf("unarchived conversation got %d command frames, want 1", got)
	}
}

package app

import (
	"sort"
	"testing"

	"foci/internal/app/fap"
	"foci/internal/platform"
)

// rosterConvs returns the conversations carried by the LAST hello frame a socket
// received, keyed by ID, and whether it received one at all. Mirrors
// lastOpenSync in open_sync_test.go: the roster is server-authoritative and
// idempotent, so only the most recent frame matters.
func rosterConvs(t *testing.T, c *wsClient) (map[string]map[string]any, bool) {
	t.Helper()
	var out map[string]map[string]any
	seen := false
	for _, f := range drain(t, c) {
		if f.t != fap.TypeHello {
			continue
		}
		seen = true
		out = map[string]map[string]any{}
		agents, ok := f.d["agents"].([]any)
		if !ok {
			continue
		}
		for _, a := range agents {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			convs, ok := am["conversations"].([]any)
			if !ok {
				continue
			}
			for _, cv := range convs {
				cm, ok := cv.(map[string]any)
				if !ok {
					continue
				}
				if id, ok := cm["id"].(string); ok {
					out[id] = cm
				}
			}
		}
	}
	return out, seen
}

// rosterTestHub builds a hub with one agent connection and two live sockets
// registered in h.clients, standing in for the user's two devices. The appConn
// is installed directly rather than via setupAgent, which returns nil (and
// registers nothing) without a real *agent.Agent handler — leaving PrimaryBot
// nil and handleConversationOpen bailing early with "no_agent".
func rosterTestHub(t *testing.T, agentID string) (*Hub, *wsClient, *wsClient) {
	t.Helper()
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: newTestIndex(t)}
	h.agents[agentID] = &appConn{hub: h, agentID: agentID}
	h.agentOrder = append(h.agentOrder, agentID)

	sender := fakeClient()
	other := fakeClient()
	h.clients[sender] = struct{}{}
	h.clients[other] = struct{}{}
	return h, sender, other
}

// TestConversationOpen_RosterReachesOtherDevices is the #1558 regression: a
// conversation created on ONE device must be advertised to the user's other
// live sockets immediately, not on their next reconnect.
//
// The bug: handleConversationOpen ended with pushRoster(client) — the
// originating socket only — so a second device learned nothing. The one
// cross-device signal it did get, ConversationOpenSync, carries bare IDs with
// no title/agent/sessionKey, so the peer could not render the chat even when it
// mirrored the open-set. Symptom: a new chat created on desktop was invisible
// on Android until the app was restarted.
//
// Server-MINTED conversations (mintFacetConversation, deliverBinding) already
// used pushRosterAll, which is why the symptom looked intermittent.
func TestConversationOpen_RosterReachesOtherDevices(t *testing.T) {
	const agentID = "arnix"
	h, sender, other := rosterTestHub(t, agentID)

	h.handleConversationOpen(sender, fap.ConversationOpen{AgentID: agentID, ConversationID: "conv-new"})

	got, seen := rosterConvs(t, other)
	if !seen {
		t.Fatal("other device received no hello frame — a conversation created on one device is invisible on the rest until reconnect (#1558)")
	}
	if _, ok := got["conv-new"]; !ok {
		t.Errorf("other device's roster = %v, want it to contain conv-new", keysOf(got))
	}

	// The creating socket still gets its own ack from the same broadcast: it is
	// a member of h.clients, so it needs no separate push.
	if got, seen := rosterConvs(t, sender); !seen || got["conv-new"] == nil {
		t.Errorf("creating socket's roster = %v (seen=%v), want it to contain conv-new", keysOf(got), seen)
	}
}

// keysOf renders a roster map compactly for failure messages — the full
// ConversationInfo maps are far too noisy to print.
func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMessageMintedConversation_RosterReachesOtherDevices covers the SECOND
// entry point for #1558: a conversation created by a message rather than by a
// conversation.open frame.
//
// This is reachable, not theoretical. The app's ConversationOpen is
// fire-and-forget (MessageSender.openConversation discards sendWire's bool and,
// alone among its send paths, never enqueues to the outbox), so a chat created
// while the socket is down is never announced — the client hello's ResumePoint
// carries no agentId, so the server cannot mint from it, and the roster orphan
// sweep deliberately skips blank-sessionKey rows. The user's first message is
// then what mints the binding, since ClientMessage carries conversationId AND
// agentId and IS durably queued. ensureBinding pushes no roster of its own, so
// without an explicit broadcast the conversation exists server-side while
// appearing on nobody's roster.
func TestMessageMintedConversation_RosterReachesOtherDevices(t *testing.T) {
	const agentID = "arnix"
	h, sender, other := rosterTestHub(t, agentID)
	registerFakeAgent(h, agentID)
	drain(t, sender)
	drain(t, other)

	// No conversation.open first — straight to a message on an id the server has
	// never seen, exactly as an offline-created chat behaves once it reconnects.
	h.dispatchInbound(sender, []byte(`{"t":"message","id":"m1","seq":1,"d":{"conversationId":"conv-orphan","agentId":"`+agentID+`","text":"hi"}}`))

	if _, ok := h.convs["conv-orphan"]; !ok {
		t.Fatal("message did not mint the binding — test premise is wrong")
	}
	got, seen := rosterConvs(t, other)
	if !seen {
		t.Fatal("other device received no hello after a message minted a conversation — it stays invisible there until reconnect (#1558)")
	}
	if _, ok := got["conv-orphan"]; !ok {
		t.Errorf("other device's roster = %v, want it to contain conv-orphan", keysOf(got))
	}
}

// TestConversationRename_RosterReachesOtherDevices proves the rename half of
// #1558: renaming a chat on one device updates the title everywhere at once.
func TestConversationRename_RosterReachesOtherDevices(t *testing.T) {
	const agentID = "arnix"
	h, sender, other := rosterTestHub(t, agentID)
	b := h.ensureBinding(sender, agentID, "conv-1")
	drain(t, sender)
	drain(t, other)

	h.handleConversationRename(sender, fap.ConversationRename{ConversationID: b.convID, Title: "Renamed"})

	got, seen := rosterConvs(t, other)
	if !seen {
		t.Fatal("other device received no hello after a rename — the new title only lands on its next reconnect (#1558)")
	}
	if title, _ := got[b.convID]["title"].(string); title != "Renamed" {
		t.Errorf("other device's roster title = %q, want %q", title, "Renamed")
	}
}

// TestConversationArchive_RosterReachesOtherDevices proves the archive half of
// #1558: archiving on one device hides the chat everywhere at once. The
// conversation STAYS in the roster carrying archived=true — the flag is
// server-authoritative and the client reconciles against it (see
// ConversationInfo.Archived) — so the assertion is on the flag, not on absence.
func TestConversationArchive_RosterReachesOtherDevices(t *testing.T) {
	const agentID = "arnix"
	h, sender, other := rosterTestHub(t, agentID)
	b := h.ensureBinding(sender, agentID, "conv-1")
	drain(t, sender)
	drain(t, other)

	h.handleConversationArchive(sender, fap.ConversationArchive{ConversationID: b.convID, Archived: true})

	got, seen := rosterConvs(t, other)
	if !seen {
		t.Fatal("other device received no hello after an archive — the chat stays visible there until reconnect (#1558)")
	}
	if archived, _ := got[b.convID]["archived"].(bool); !archived {
		t.Errorf("other device's roster has conv-1 archived=%v, want true", archived)
	}
}

// TestConversationSetDefault_RosterReachesOtherDevices proves the set-default
// half of #1558. This one is more than cosmetic: the default chat is what
// session-blind delivery (keepalive, cron) routes to, so a device holding a
// stale default renders the golden pin on the wrong conversation.
func TestConversationSetDefault_RosterReachesOtherDevices(t *testing.T) {
	const agentID = "arnix"
	h, sender, other := rosterTestHub(t, agentID)
	b := h.ensureBinding(sender, agentID, "conv-1")
	drain(t, sender)
	drain(t, other)

	h.handleConversationSetDefault(fap.ConversationSetDefault{ConversationID: b.convID, IsDefault: true})

	got, seen := rosterConvs(t, other)
	if !seen {
		t.Fatal("other device received no hello after setDefault — it keeps routing/pinning the old default until reconnect (#1558)")
	}
	if isDefault, _ := got[b.convID]["isDefault"].(bool); !isDefault {
		t.Errorf("other device's roster has conv-1 isDefault=%v, want true", isDefault)
	}
}

// TestConversationArchive_RefusedDefaultDoesNotBroadcast pins the deliberate
// exception to the broadcast rule: when the server REFUSES to archive the
// default chat, nothing changed server-side and only the requesting device
// applied the optimistic flag that needs reverting. Broadcasting there would be
// pure noise on every other socket.
func TestConversationArchive_RefusedDefaultDoesNotBroadcast(t *testing.T) {
	const agentID = "arnix"
	h, sender, other := rosterTestHub(t, agentID)
	b := h.ensureBinding(sender, agentID, "conv-1")
	if err := h.deps.SessionIndex.SetDefaultChat(agentID, "app", b.chatID); err != nil {
		t.Fatalf("SetDefaultChat: %v", err)
	}
	drain(t, sender)
	drain(t, other)

	h.handleConversationArchive(sender, fap.ConversationArchive{ConversationID: b.convID, Archived: true})

	if _, seen := rosterConvs(t, sender); !seen {
		t.Error("requesting socket got no roster revert after a refused archive")
	}
	if _, seen := rosterConvs(t, other); seen {
		t.Error("a REFUSED archive must not broadcast — server state is unchanged and no other device applied the optimistic flag")
	}
}

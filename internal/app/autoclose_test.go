package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/fap"
	"foci/internal/platform"
	"foci/internal/session"
)

// sweepHub builds a hub wired for sweep tests: a session index and, when bag is
// non-nil, a synced settings bag holding exactly those keys (nil = nothing
// stored, so the defaults apply).
func sweepHub(t *testing.T, bag map[string]string) (*Hub, *session.SessionIndex) {
	t.Helper()
	idx := newTestIndex(t)
	h := newTestHub()
	h.deps = platform.ProviderDeps{SessionIndex: idx}
	if bag != nil {
		b, err := json.Marshal(bag)
		if err != nil {
			t.Fatal(err)
		}
		if err := idx.SetSystemState(systemStateAppSettings, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	return h, idx
}

// sweepBinding installs a durable binding whose last visible frame is age old
// (age 0 leaves lastActMs at 0 — "no visible message yet").
func sweepBinding(h *Hub, id string, age time.Duration, now time.Time) *convBinding {
	b := &convBinding{convID: id, agentID: "clutch", chatID: chatIDForConv(id), sessionKey: session.NewChatSessionKey("clutch", chatIDForConv(id))}
	if age > 0 {
		b.lastActMs = now.Add(-age).UnixMilli()
	}
	h.convs[id] = b
	return b
}

// sweepPair installs two equally stale chats: the one the test exempts
// ("kept") and its fail-arm sibling ("gone"), which must close when nothing
// else holds it — proving the exemption in question is what kept "kept".
func sweepPair(h *Hub, now time.Time) (kept, gone *convBinding) {
	return sweepBinding(h, "kept", 25*time.Hour, now), sweepBinding(h, "gone", 25*time.Hour, now)
}

// seedMessage stores one visible message frame with id, the durable record a
// read watermark is compared against. Hand-minted crockford ids keep the
// ULID ordering readWatermarkAdvances relies on deterministic.
func seedMessage(t *testing.T, s *frameStore, convID, msgID string) {
	t.Helper()
	wire, err := fap.Encode(fap.ServerMessage{ConversationID: convID, MessageID: msgID, Role: "assistant", Text: "hi"}, 1, 0, "", "")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s.insert(frameWrite{convID: convID, agentID: "clutch", seq: 1, wire: wire, sentMs: time.Now().UnixMilli(), visible: true})
}

// Hand-minted ULIDs, ordered: oldWatermark < newestMessage.
const (
	sweepOldWatermark = "0000000000000000000000000A"
	sweepNewestMsg    = "0000000000000000000000000B"
)

// TestParseAutoCloseSettings pins the bag parsing: missing keys mean the
// defaults (on, 24), unparseable values fall back to the same defaults, and a
// parsed hour count clamps into [1, 720].
func TestParseAutoCloseSettings(t *testing.T) {
	cases := []struct {
		name string
		bag  map[string]string
		want autoCloseSettings
	}{
		{"missing keys mean on/24", nil, autoCloseSettings{enabled: true, hours: 24}},
		{"disabled", map[string]string{settingAutoCloseEnabled: "false"}, autoCloseSettings{enabled: false, hours: 24}},
		{"enabled explicitly", map[string]string{settingAutoCloseEnabled: "true"}, autoCloseSettings{enabled: true, hours: 24}},
		{"hours honoured", map[string]string{settingAutoCloseAfterHours: "2"}, autoCloseSettings{enabled: true, hours: 2}},
		{"zero clamps to 1", map[string]string{settingAutoCloseAfterHours: "0"}, autoCloseSettings{enabled: true, hours: 1}},
		{"negative clamps to 1", map[string]string{settingAutoCloseAfterHours: "-5"}, autoCloseSettings{enabled: true, hours: 1}},
		{"9999 clamps to 720", map[string]string{settingAutoCloseAfterHours: "9999"}, autoCloseSettings{enabled: true, hours: 720}},
		{"unparseable hours means 24", map[string]string{settingAutoCloseAfterHours: "abc"}, autoCloseSettings{enabled: true, hours: 24}},
		{"unparseable bool means on", map[string]string{settingAutoCloseEnabled: "maybe"}, autoCloseSettings{enabled: true, hours: 24}},
		{"padded value parses", map[string]string{settingAutoCloseAfterHours: " 6 "}, autoCloseSettings{enabled: true, hours: 6}},
	}
	for _, tc := range cases {
		if got := parseAutoCloseSettings(tc.bag); got != tc.want {
			t.Errorf("%s: parseAutoCloseSettings(%v) = %+v, want %+v", tc.name, tc.bag, got, tc.want)
		}
	}
}

// TestSweepOpenChats_DefaultOnRemovesStaleKeepsFreshAndBroadcasts pins the
// default-on threshold through the whole round: with an empty bag a chat whose
// last message is 25h old leaves the open-set while a 23h-old one stays, the
// survivors keep their original order in the stored set, and a second socket
// receives a conversation.openSync carrying exactly them.
func TestSweepOpenChats_DefaultOnRemovesStaleKeepsFreshAndBroadcasts(t *testing.T) {
	h, _ := sweepHub(t, nil)
	now := time.Now()
	sweepBinding(h, "freshA", 23*time.Hour, now)
	sweepBinding(h, "stale", 25*time.Hour, now)
	sweepBinding(h, "freshB", 23*time.Hour, now)
	other := fakeClient()
	h.clients[other] = struct{}{}
	h.storeOpenChats([]string{"freshA", "stale", "freshB"})

	h.sweepOpenChats(now)

	if got := h.loadOpenChats(); len(got) != 2 || got[0] != "freshA" || got[1] != "freshB" {
		t.Errorf("stored open-set = %v, want [freshA freshB] in their original order", got)
	}
	if ids, seen := lastOpenSync(t, other); !seen || len(ids) != 2 || ids[0] != "freshA" || ids[1] != "freshB" {
		t.Errorf("other client OpenSync = %v (seen=%v), want [freshA freshB]", ids, seen)
	}
}

// TestSweepOpenChats_DisabledRemovesNothing proves auto_close_enabled="false"
// removes nothing and sends nothing.
func TestSweepOpenChats_DisabledRemovesNothing(t *testing.T) {
	h, _ := sweepHub(t, map[string]string{settingAutoCloseEnabled: "false"})
	now := time.Now()
	sweepBinding(h, "stale", 25*time.Hour, now)
	other := fakeClient()
	h.clients[other] = struct{}{}
	h.storeOpenChats([]string{"stale"})

	h.sweepOpenChats(now)

	if got := h.loadOpenChats(); len(got) != 1 || got[0] != "stale" {
		t.Errorf("stored open-set = %v, want [stale] untouched while disabled", got)
	}
	if _, seen := lastOpenSync(t, other); seen {
		t.Error("a disabled sweep must not broadcast")
	}
}

// TestSweepOpenChats_HoursHonouredAndClamped drives the hours key end-to-end:
// "2" closes a 3h-old chat, "0" clamps to 1h, "9999" clamps to 720h, and "abc"
// behaves as the 24h default.
func TestSweepOpenChats_HoursHonouredAndClamped(t *testing.T) {
	cases := []struct {
		name string
		bag  map[string]string
		ages map[string]bool // conv id (c<duration>) → should close
	}{
		{"two hours closes a three-hour chat",
			map[string]string{settingAutoCloseAfterHours: "2"},
			map[string]bool{"c3h": true, "c1h": false}},
		{"zero clamps to one hour",
			map[string]string{settingAutoCloseAfterHours: "0"},
			map[string]bool{"c90m": true, "c30m": false}},
		{"9999 clamps to 720 hours",
			map[string]string{settingAutoCloseAfterHours: "9999"},
			map[string]bool{"c721h": true, "c719h": false}},
		{"garbage means the 24-hour default",
			map[string]string{settingAutoCloseAfterHours: "abc"},
			map[string]bool{"c25h": true, "c23h": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := sweepHub(t, tc.bag)
			now := time.Now()
			var ids []string
			for id := range tc.ages {
				age, err := time.ParseDuration(strings.TrimPrefix(id, "c"))
				if err != nil {
					t.Fatal(err)
				}
				sweepBinding(h, id, age, now)
				ids = append(ids, id)
			}
			h.storeOpenChats(ids)

			h.sweepOpenChats(now)

			got := h.loadOpenChats()
			for id, wantClosed := range tc.ages {
				if present := slices.Contains(got, id); present == wantClosed {
					t.Errorf("%s: open=%v after sweep, want closed=%v", id, present, wantClosed)
				}
			}
		})
	}
}

// TestSweepOpenChats_UnreadableBagDoesNothing proves a bag that cannot be read
// (system_state holds non-JSON) neither removes nor sends anything — the sweep
// never falls back to defaults on a read error (#2081).
func TestSweepOpenChats_UnreadableBagDoesNothing(t *testing.T) {
	h, idx := sweepHub(t, nil)
	if err := idx.SetSystemState(systemStateAppSettings, "this is not json"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sweepBinding(h, "stale", 25*time.Hour, now)
	other := fakeClient()
	h.clients[other] = struct{}{}
	h.storeOpenChats([]string{"stale"})

	h.sweepOpenChats(now)

	if got := h.loadOpenChats(); len(got) != 1 || got[0] != "stale" {
		t.Errorf("stored open-set = %v, want [stale] untouched on an unreadable bag", got)
	}
	if _, seen := lastOpenSync(t, other); seen {
		t.Error("a sweep that could not read its setting must not broadcast")
	}
}

// TestSweepOpenChats_KeepsViewedChat proves a chat a live socket is viewing
// right now never closes, while its unviewed stale sibling does.
func TestSweepOpenChats_KeepsViewedChat(t *testing.T) {
	h, _ := sweepHub(t, nil)
	now := time.Now()
	kept, _ := sweepPair(h, now)
	viewer := fakeClient()
	h.clients[viewer] = struct{}{}
	h.handleViewingPut(viewer, fap.ViewingPut{ConversationID: kept.convID})
	drain(t, viewer)
	h.storeOpenChats([]string{"kept", "gone"})

	h.sweepOpenChats(now)

	got := h.loadOpenChats()
	if !slices.Contains(got, "kept") {
		t.Error("a chat viewed on a connected device must stay open")
	}
	if slices.Contains(got, "gone") {
		t.Error("control: the unviewed stale sibling must close")
	}
}

// TestSweepOpenChats_KeepsChatWithActivityInFlight proves a chat the roster
// would report activity for (a tool turn running; awaiting another agent)
// never closes, while its idle stale sibling does.
func TestSweepOpenChats_KeepsChatWithActivityInFlight(t *testing.T) {
	h, _ := sweepHub(t, nil)
	now := time.Now()
	tool := sweepBinding(h, "tool", 25*time.Hour, now)
	wait := sweepBinding(h, "wait", 25*time.Hour, now)
	sweepBinding(h, "gone", 25*time.Hour, now)
	tool.setTurnTool(fap.ActivityKindTool, "npm install", "", false)
	wait.setWaitingDetail("other-agent")
	h.storeOpenChats([]string{"tool", "wait", "gone"})

	h.sweepOpenChats(now)

	got := h.loadOpenChats()
	if !slices.Contains(got, "tool") || !slices.Contains(got, "wait") {
		t.Errorf("chats with a turn in flight must stay open, got %v", got)
	}
	if slices.Contains(got, "gone") {
		t.Error("control: the idle stale sibling must close")
	}
}

// TestSweepOpenChats_KeepsDefaultChat proves the agent's default app chat never
// closes, while its non-default stale sibling does.
func TestSweepOpenChats_KeepsDefaultChat(t *testing.T) {
	h, idx := sweepHub(t, nil)
	now := time.Now()
	kept, _ := sweepPair(h, now)
	if err := idx.SetDefaultChat("clutch", "app", kept.chatID); err != nil {
		t.Fatal(err)
	}
	h.storeOpenChats([]string{"kept", "gone"})

	h.sweepOpenChats(now)

	got := h.loadOpenChats()
	if !slices.Contains(got, "kept") {
		t.Error("the agent's default chat must stay open")
	}
	if slices.Contains(got, "gone") {
		t.Error("control: the non-default stale sibling must close")
	}
}

// TestSweepOpenChats_KeepsPinnedChat proves a chat with conv_pinned="1" never
// closes, while its unpinned (never-written) stale sibling does — pinning the
// ruling that a never-written pin counts as NOT pinned.
func TestSweepOpenChats_KeepsPinnedChat(t *testing.T) {
	h, idx := sweepHub(t, nil)
	now := time.Now()
	kept, _ := sweepPair(h, now)
	if err := idx.SetChatMetadata("clutch", "app", kept.chatID, chatMetaConvPinned, "1"); err != nil {
		t.Fatal(err)
	}
	h.storeOpenChats([]string{"kept", "gone"})

	h.sweepOpenChats(now)

	got := h.loadOpenChats()
	if !slices.Contains(got, "kept") {
		t.Error("a pinned chat must stay open")
	}
	if slices.Contains(got, "gone") {
		t.Error("control: the never-written-pin stale sibling must close")
	}
}

// TestSweepOpenChats_KeepsUnreadChat proves a chat whose newest visible
// message is past the stored last_read watermark (unread) never closes, while
// its message-less stale sibling does.
func TestSweepOpenChats_KeepsUnreadChat(t *testing.T) {
	h, idx := sweepHub(t, nil)
	h.frames = tempFrameStore(t)
	now := time.Now()
	kept, _ := sweepPair(h, now)
	seedMessage(t, h.frames, "kept", sweepNewestMsg)
	if err := idx.SetChatMetadata("clutch", "app", kept.chatID, "last_read", sweepOldWatermark); err != nil {
		t.Fatal(err)
	}
	h.storeOpenChats([]string{"kept", "gone"})

	h.sweepOpenChats(now)

	got := h.loadOpenChats()
	if !slices.Contains(got, "kept") {
		t.Error("an unread chat (newest message past the watermark) must stay open")
	}
	if slices.Contains(got, "gone") {
		t.Error("control: the message-less stale sibling must close")
	}
}

// TestSweepOpenChats_KeepsChatWithEmptyWatermarkAndMessage proves a chat with
// a visible message but NO stored last_read (never read on any device) counts
// as unread and never closes.
func TestSweepOpenChats_KeepsChatWithEmptyWatermarkAndMessage(t *testing.T) {
	h, _ := sweepHub(t, nil)
	h.frames = tempFrameStore(t)
	now := time.Now()
	sweepPair(h, now)
	seedMessage(t, h.frames, "kept", sweepNewestMsg)
	// No last_read row at all for kept.
	h.storeOpenChats([]string{"kept", "gone"})

	h.sweepOpenChats(now)

	got := h.loadOpenChats()
	if !slices.Contains(got, "kept") {
		t.Error("a chat with a message and no read watermark is unread — it must stay open")
	}
	if slices.Contains(got, "gone") {
		t.Error("control: the message-less stale sibling must close")
	}
}

// TestSweepOpenChats_KeepsChatWithNoVisibleMessage proves a chat with no known
// last activity (lastActMs==0 — a just-minted facet chat, a just-created empty
// chat) is never closed by the next sweep.
func TestSweepOpenChats_KeepsChatWithNoVisibleMessage(t *testing.T) {
	h, _ := sweepHub(t, nil)
	now := time.Now()
	sweepBinding(h, "kept", 0, now) // lastActMs stays 0
	sweepBinding(h, "gone", 25*time.Hour, now)
	h.storeOpenChats([]string{"kept", "gone"})

	h.sweepOpenChats(now)

	got := h.loadOpenChats()
	if !slices.Contains(got, "kept") {
		t.Error("a chat with no visible message yet must stay open")
	}
	if slices.Contains(got, "gone") {
		t.Error("control: the stale sibling must close")
	}
}

// TestSweepOpenChats_KeepsChatWithNoBinding proves an open-set id with no live
// binding is kept: there is no activity to judge it by.
func TestSweepOpenChats_KeepsChatWithNoBinding(t *testing.T) {
	h, _ := sweepHub(t, nil)
	now := time.Now()
	sweepBinding(h, "gone", 25*time.Hour, now)
	h.storeOpenChats([]string{"ghost", "gone"})

	h.sweepOpenChats(now)

	got := h.loadOpenChats()
	if !slices.Contains(got, "ghost") {
		t.Error("an open-set id with no binding must stay")
	}
	if slices.Contains(got, "gone") {
		t.Error("control: the bound stale sibling must close")
	}
}

// TestSweepOpenChats_ReadUpToNewestCloses proves read is not unread: a chat
// whose last_read equals its newest visible message's id is fully read, so a
// stale one closes.
func TestSweepOpenChats_ReadUpToNewestCloses(t *testing.T) {
	h, idx := sweepHub(t, nil)
	h.frames = tempFrameStore(t)
	now := time.Now()
	only := sweepBinding(h, "read", 25*time.Hour, now)
	seedMessage(t, h.frames, "read", sweepNewestMsg)
	if err := idx.SetChatMetadata("clutch", "app", only.chatID, "last_read", sweepNewestMsg); err != nil {
		t.Fatal(err)
	}
	h.storeOpenChats([]string{"read"})

	h.sweepOpenChats(now)

	if slices.Contains(h.loadOpenChats(), "read") {
		t.Error("a chat read up to its newest message is read — a stale one must close")
	}
}

// TestSweepOpenChats_NoopWhenNothingStale proves a round that removes nothing
// rewrites nothing and sends nothing: the stored row stays byte-identical and
// a connected client receives no frame.
func TestSweepOpenChats_NoopWhenNothingStale(t *testing.T) {
	h, idx := sweepHub(t, nil)
	now := time.Now()
	sweepBinding(h, "fresh", time.Hour, now)
	other := fakeClient()
	h.clients[other] = struct{}{}
	h.storeOpenChats([]string{"fresh"})
	before, err := idx.GetSystemState(systemStateOpenChats)
	if err != nil {
		t.Fatal(err)
	}

	h.sweepOpenChats(now)

	after, err := idx.GetSystemState(systemStateOpenChats)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("open_chats rewritten by a no-op sweep: %q → %q", before, after)
	}
	if _, seen := lastOpenSync(t, other); seen {
		t.Error("a no-op sweep must not broadcast")
	}
}

// TestSweepOpenChats_LastStaleChatClosesToEmptySet proves the sweep empties
// the set all the way down: the stored row becomes [] (not null) and the
// broadcast carries an empty list.
func TestSweepOpenChats_LastStaleChatClosesToEmptySet(t *testing.T) {
	h, idx := sweepHub(t, nil)
	now := time.Now()
	sweepBinding(h, "only", 25*time.Hour, now)
	other := fakeClient()
	h.clients[other] = struct{}{}
	h.storeOpenChats([]string{"only"})

	h.sweepOpenChats(now)

	if raw, _ := idx.GetSystemState(systemStateOpenChats); raw != "[]" {
		t.Errorf("stored open-set = %q, want []", raw)
	}
	if ids, seen := lastOpenSync(t, other); !seen || len(ids) != 0 {
		t.Errorf("other client OpenSync = %v (seen=%v), want an empty list", ids, seen)
	}
}

// TestSweepOpenChats_ConcurrentOpenSetNeverLost proves the open-set's three
// writers are serialised: a sweep racing a client's full replace (and a facet
// mint) can neither lose the tab the other writer added nor resurrect the
// stale chats it just swept. Red on a sweep that does not take openSetMu — its
// load→filter→store overwrites the handler's newer set — and on an unlocked
// mint, whose stale copy undoes the sweep's store.
func TestSweepOpenChats_ConcurrentOpenSetNeverLost(t *testing.T) {
	t.Run("handler replace", func(t *testing.T) {
		h, _ := sweepHub(t, nil)
		now := time.Now()
		fleet := make([]string, 0, 24)
		for i := range 24 {
			id := fmt.Sprintf("stale%02d", i)
			sweepBinding(h, id, 25*time.Hour, now)
			fleet = append(fleet, id)
		}
		client := fakeClient()
		for range 200 {
			h.storeOpenChats(fleet)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				h.handleConversationOpenSet(client, fap.ConversationOpenSet{ConversationIDs: []string{"fresh-tab"}})
			}()
			go func() {
				defer wg.Done()
				h.sweepOpenChats(now)
			}()
			wg.Wait()
			if got := h.loadOpenChats(); !slices.Contains(got, "fresh-tab") {
				t.Fatalf("the handler's tab was lost to the racing sweep; stored set = %v", got)
			}
		}
	})

	t.Run("facet mint", func(t *testing.T) {
		h, _ := sweepHub(t, nil)
		registerBareAgent(h, "clutch")
		now := time.Now()
		fleet := make([]string, 0, 24)
		for i := range 24 {
			id := fmt.Sprintf("stale%02d", i)
			sweepBinding(h, id, 25*time.Hour, now)
			fleet = append(fleet, id)
		}
		facetKey := session.NewChatSessionKey("clutch", 777)
		for range 400 {
			h.storeOpenChats(fleet)
			var wg sync.WaitGroup
			minted := make(chan string, 1)
			wg.Add(2)
			go func() {
				defer wg.Done()
				id, err := h.mintFacetConversation("clutch", facetKey)
				if err != nil {
					t.Error(err)
				}
				minted <- id
			}()
			go func() {
				defer wg.Done()
				h.sweepOpenChats(now)
			}()
			wg.Wait()
			got := h.loadOpenChats()
			id := <-minted
			if !slices.Contains(got, id) {
				t.Fatalf("the minted tab %s was lost to the racing sweep; stored set = %v", id, got)
			}
			for _, stale := range fleet {
				if slices.Contains(got, stale) {
					t.Fatalf("the mint's stale copy resurrected swept chat %s; stored set = %v", stale, got)
				}
			}
		}
	})
}

// TestStartAll_AutoCloseSweepRunsPeriodicallyAndStops proves the ticker loop:
// a hub with a gateway context sweeps on its (shrunk) cadence until the
// context is cancelled, a double StartAll still leaves exactly one working
// loop, and the loop rides deps.Ctx — NOT StartAll's own parameter (passed
// here as an uncancellable Background, which would keep a wrongly-parametered
// loop sweeping after cancel and redden the second half).
func TestStartAll_AutoCloseSweepRunsPeriodicallyAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h, _ := sweepHub(t, map[string]string{settingAutoCloseAfterHours: "1"})
	h.deps.Ctx = ctx
	h.autoCloseEvery = 2 * time.Millisecond
	now := time.Now()
	sweepBinding(h, "stale", 25*time.Hour, now)
	h.storeOpenChats([]string{"stale"})

	h.StartAll(context.Background())
	h.StartAll(context.Background()) // double start: sync.Once must keep one loop

	deadline := time.Now().Add(5 * time.Second)
	for slices.Contains(h.loadOpenChats(), "stale") {
		if time.Now().After(deadline) {
			t.Fatal("the periodic sweep never closed the stale chat within its interval")
		}
		time.Sleep(time.Millisecond)
	}

	// Fail-armed non-event: cancel, let any in-flight round drain, then re-seed
	// a stale chat and give a (former) loop many intervals to sweep it. The
	// positive half above proves 2ms rounds sweep, so only a loop that
	// outlived the context could close this one.
	cancel()
	time.Sleep(10 * time.Millisecond)
	sweepBinding(h, "stale2", 25*time.Hour, now)
	h.storeOpenChats([]string{"stale2"})
	time.Sleep(50 * time.Millisecond)
	if got := h.loadOpenChats(); !slices.Contains(got, "stale2") {
		t.Fatal("the sweep kept running after its context was cancelled")
	}
}

// TestStartAll_WithoutContextStartsNoSweep proves a hub with no gateway
// context (the shape of every test-built hub) starts no loop: its stale chat
// survives far past the shrunk cadence. The positive control is
// TestStartAll_AutoCloseSweepRunsPeriodicallyAndStops, which sweeps at the
// same cadence once a context exists.
func TestStartAll_WithoutContextStartsNoSweep(t *testing.T) {
	h, _ := sweepHub(t, map[string]string{settingAutoCloseAfterHours: "1"})
	h.autoCloseEvery = time.Millisecond
	now := time.Now()
	sweepBinding(h, "stale", 25*time.Hour, now)
	h.storeOpenChats([]string{"stale"})

	h.StartAll(context.Background())  // deps.Ctx is nil here
	time.Sleep(30 * time.Millisecond) // 30 intervals at the shrunk cadence

	if got := h.loadOpenChats(); !slices.Contains(got, "stale") {
		t.Fatal("a ctx-less hub must not start the auto-close sweep (the stale chat would have closed)")
	}
}

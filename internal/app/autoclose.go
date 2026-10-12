package app

import (
	"strconv"
	"strings"
	"time"

	"foci/internal/fap"
	"foci/internal/session"
)

// Auto-close sweep (#1243). The user's open chats — the tabs open across their
// devices — are one shared set (system_state open_chats) that keepalive warms
// in full (OpenSessionsForAgent), so a tab left open for weeks used to be kept
// warm for weeks. This sweep is the third writer of that set (beside
// handleConversationOpenSet's replace and mintFacetConversation's append): on
// a timer it removes chats whose last message is older than a settings-bag
// threshold and mirrors the result to every device. Dick's ruling
// (2026-10-11): it exempts the same chats as auto-archive (pinned, default,
// unread) plus chats on screen and chats with a turn in flight, is ON by
// default at 24 hours, and closing only removes the tab — the conversation,
// its frames and its roster row stay, and reopening is just another open-set
// change.

// The sweep's two keys in the synced settings bag (system_state app_settings,
// read with loadAppSettings). The client encodes bools as "true"/"false" and
// ints as decimal strings (SettingsStore readSynced/writeSynced) — the same
// bag and encoding as auto_archive_*. The Settings toggle that writes them is
// separate foci-client ticket work; until it lands the server runs on the
// defaults.
const (
	settingAutoCloseEnabled    = "auto_close_enabled"
	settingAutoCloseAfterHours = "auto_close_after_hours"
)

// Threshold bounds and cadence. The clamp mirrors the client's auto-archive
// days clamp; the sweep interval is a quarter of the finest user-selectable
// threshold, so the 1-hour minimum holds to within one interval. An internal
// cadence like setupInteractiveCleanup's, not a per-deployment knob: every
// user-facing setting lives in the bag by the ruling.
const (
	defaultAutoCloseEnabled = true
	defaultAutoCloseHours   = 24
	minAutoCloseHours       = 1
	maxAutoCloseHours       = 720
	autoCloseSweepInterval  = 15 * time.Minute
)

// autoCloseSettings is the sweep's configuration, resolved from the synced bag.
type autoCloseSettings struct {
	enabled bool
	hours   int
}

// parseAutoCloseSettings resolves the sweep's two bag keys. A missing or
// unparseable key means its default (a value the client could not encode
// better must not silently disable the sweep or retime it wildly); a parsed
// hour count is clamped into [minAutoCloseHours, maxAutoCloseHours].
func parseAutoCloseSettings(bag map[string]string) autoCloseSettings {
	cfg := autoCloseSettings{enabled: defaultAutoCloseEnabled, hours: defaultAutoCloseHours}
	if v, ok := bag[settingAutoCloseEnabled]; ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.enabled = b
		}
	}
	if v, ok := bag[settingAutoCloseAfterHours]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			cfg.hours = min(max(n, minAutoCloseHours), maxAutoCloseHours)
		}
	}
	return cfg
}

// sweepOpenChats runs one auto-close round at now (a parameter so tests fix
// the clock): it resolves the setting from the synced bag, removes stale chats
// from the shared open-set, stores the survivors in their original order and
// broadcasts a conversation.openSync of them to every device. An unreadable
// bag skips the round without touching anything — sweeping on defaults after a
// read error is the #2081 shape — and a round that removes nothing writes and
// sends nothing. The whole load→filter→store→fan-out runs under openSetMu with
// the other two open-set writers, so a concurrent open or mint can neither be
// lost to this store nor overridden by a stale one.
func (h *Hub) sweepOpenChats(now time.Time) {
	idx := h.deps.SessionIndex
	if idx == nil {
		return
	}
	h.settingsMu.Lock()
	bag, err := h.loadAppSettings(idx)
	h.settingsMu.Unlock()
	if err != nil {
		appLog.Warnf("auto-close: skipping this round, cannot read the settings bag: %v", err)
		return
	}
	cfg := parseAutoCloseSettings(bag)
	if !cfg.enabled {
		return
	}

	h.openSetMu.Lock()
	defer h.openSetMu.Unlock()
	ids := h.loadOpenChats()
	kept := make([]string, 0, len(ids)) // non-nil, so an emptied set stores and sends [] rather than null
	defaultChats := make(map[string]int64)
	for _, id := range ids {
		if h.shouldAutoClose(id, cfg, now, defaultChats) {
			continue
		}
		kept = append(kept, id)
	}
	if len(kept) == len(ids) {
		return
	}
	h.storeOpenChats(kept)
	// sender=nil: this close is server-originated, so — as in
	// mintFacetConversation — there is no device to exclude; every connected
	// device must reconcile its tabs, including one that is viewing none.
	h.broadcastExcept(nil, fap.ConversationOpenSync{ConversationIDs: kept})
	appLog.Infof("auto-close: closed %d open chat(s) past %dh of inactivity (%d remain)", len(ids)-len(kept), cfg.hours, len(kept))
}

// shouldAutoClose reports whether id may leave the open-set this round: every
// exemption (live binding, known-and-stale activity, not viewed, idle roster,
// not the default chat, not pinned, not unread) must clear, checked cheapest
// first so the chat_metadata reads run only for otherwise-stale chats. Every
// per-chat read that errors fails toward KEEP — a chat whose state cannot be
// read is never closed on a guess. defaultChats memoises per-agent
// default-chat lookups across one round (agentRoster's memoisation).
func (h *Hub) shouldAutoClose(id string, cfg autoCloseSettings, now time.Time, defaultChats map[string]int64) bool {
	idx := h.deps.SessionIndex
	if idx == nil {
		return false // unreachable via sweepOpenChats (the bag read needs the index)
	}
	// No live binding → keep: there is no activity to judge the chat by.
	h.mu.RLock()
	b := h.convs[id]
	h.mu.RUnlock()
	if b == nil {
		return false
	}
	// Staleness is the roster's LastActivityTs (convBinding.lastActMs): the
	// last visible frame's send time. Unknown (0 — a just-minted facet chat, a
	// just-created empty chat) keeps the chat; a known age must be strictly
	// older than the threshold.
	if last := b.lastActivity(); last == 0 || last >= now.Add(-time.Duration(cfg.hours)*time.Hour).UnixMilli() {
		return false
	}
	if h.isViewed(id) || !b.activityIdle() {
		return false // on screen on some device, or a turn still in flight
	}
	if dc := memoDefaultChat(idx, defaultChats, b.agentID); dc != 0 && dc == b.chatID {
		return false // the agent's default app chat
	}
	// Pinned stays; never-written counts as NOT pinned (the ruling: closing a
	// tab is cheap to undo, and no client uploads its local pins yet); a row
	// that cannot be read keeps the chat rather than guessing.
	if pinned, _, err := h.conversationPinned(b); err != nil {
		appLog.Warnf("auto-close: conv=%s: read pin state: %v — keeping", id, err)
		return false
	} else if pinned {
		return false
	}
	// Unread stays. The newest visible MESSAGE id comes from the durable frame
	// history, never from lastActMs (a frame's send time is stamped after its
	// message id is minted, so comparing the two would mark every read chat
	// unread); readWatermarkAdvances orders it against the stored last_read
	// watermark exactly as handleRead does — including "no watermark yet" as
	// unread, and a watermark read error ("" below) as unread: keep.
	msg, ok, err := h.frames.LastVisibleMessageID(id)
	if err != nil {
		appLog.Warnf("auto-close: conv=%s: read newest message: %v — keeping", id, err)
		return false
	}
	if ok {
		lastRead, _ := idx.GetChatMetadata(b.agentID, "app", b.chatID, "last_read")
		if readWatermarkAdvances(lastRead, msg) {
			return false
		}
	}
	// No message-carrying frame at all (an ask-only history): nothing a read
	// watermark could cover — judged read.
	return true
}

// memoDefaultChat resolves an agent's default app chat through defaultChats,
// querying the index at most once per agent per round (agentRoster's
// memoisation — a fleet of one agent's chats must not be one query each).
func memoDefaultChat(idx *session.SessionIndex, defaultChats map[string]int64, agentID string) int64 {
	if v, ok := defaultChats[agentID]; ok {
		return v
	}
	v := idx.DefaultChatForAgent(agentID, "app")
	defaultChats[agentID] = v
	return v
}

// startAutoCloseSweep starts the periodic sweep, once per hub. It rides the
// gateway context (h.deps.Ctx — the one runShutdown cancels, which retires the
// hub's other background goroutines), NOT StartAll's parameter, which callers
// may legitimately pass as an uncancellable context. Started from StartAll so
// the first round judges rebuilt bindings. A hub with no context (every
// test-built hub) starts no loop at all.
func (h *Hub) startAutoCloseSweep() {
	ctx := h.deps.Ctx
	if ctx == nil {
		return
	}
	h.autoCloseOnce.Do(func() {
		every := h.autoCloseEvery
		if every <= 0 {
			every = autoCloseSweepInterval
		}
		safeGo("auto-close-sweep", func() {
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// The re-check: a tick chosen over an already-done ctx in
					// the same select must not fire one last round past
					// shutdown (Hub.Close follows the cancel and closes the
					// frame store).
					if ctx.Err() != nil {
						return
					}
					h.sweepOpenChats(time.Now())
				}
			}
		})
	})
}

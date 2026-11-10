package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/fap"
)

// Stop-view subagent summaries (#2960). While the app's Stop view is open it
// sends subagent.summarize every few seconds with the ids of the subagents and
// spawns it lists. The server keeps each one's latest steps (its text blocks
// and tool calls, fed by NoteSubagentActivity and SpawnObserver) and asks the
// pin summary's fast model (#2956: same model, provider and key) for a label
// of at most six words of the CURRENT step. The answer is never cut: the
// shared path drops one longer than 1.5 times that (summaryAsk.maxWords,
// #2962; Dick, 2026-11-08). It answers each id with
// subagent.summary: the last summary when nothing changed since, else a new one.
// A model call is made only on a request, so nothing is spent while nobody
// looks, and at most once per entry per subagentSummaryMinGap.

const (
	// subagentActivityItems is how many latest steps the summary reads
	// (Dick: "Perhaps last three messages as input").
	subagentActivityItems = 3
	// subagentActivityItemRunes caps one step's text.
	subagentActivityItemRunes = 600
	// subagentActivityMaxEntries bounds the store: an entry whose end was
	// never reported is evicted, oldest touched first.
	subagentActivityMaxEntries = 256
	// subagentSummaryMinGap is the least time between two model calls for one
	// entry (clutch's default on the ticket: at most once every 10 s).
	subagentSummaryMinGap = 10 * time.Second
	// subagentSummaryWords is the label length the prompt asks for (Dick:
	// "max-six-word summary").
	subagentSummaryWords = 6
	// subagentSummaryMaxIDs caps the ids read from one request.
	subagentSummaryMaxIDs = 32
	// subagentSummaryMaxTokens is ample for six words.
	subagentSummaryMaxTokens = 30
)

const subagentSummaryPrompt = "You label what an AI agent is doing right now, for a status list. " +
	"The user sends the agent's latest activity, oldest first: its messages and its tool calls. " +
	"Describe its CURRENT step (the latest activity), not its whole task, in AT MOST SIX WORDS. " +
	"This limit is strict: never more than six words. Start with a verb ending in -ing. " +
	"Reply with the label only: no quotes, no preamble, no trailing full stop, one line."

var subagentSummaryAsk = summaryAsk{
	what:          "subagent summary",
	system:        subagentSummaryPrompt,
	maxInputRunes: pinSummaryMaxInputRunes,
	maxTokens:     subagentSummaryMaxTokens,
	maxWords:      subagentSummaryWords,
	timeout:       pinSummaryTimeout,
}

// subagentActivityStore holds the latest steps of every running subagent and
// spawn, keyed by session and group key, with each one's last summary.
type subagentActivityStore struct {
	mu      sync.Mutex
	entries map[string]*subagentActivityEntry
	now     func() time.Time // nil = time.Now; a test sets it
}

type subagentActivityEntry struct {
	items   []string // the latest steps, oldest first, at most subagentActivityItems
	version uint64   // bumped on every step

	summary        string    // the last summary ("" = none yet)
	summaryVersion uint64    // the version the summary was made from
	lastCall       time.Time // when the last model call started
	inflight       bool
	touched        time.Time
}

func newSubagentActivityStore() *subagentActivityStore {
	return &subagentActivityStore{entries: make(map[string]*subagentActivityEntry)}
}

func (s *subagentActivityStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func activityKey(sessionKey, groupKey string) string { return sessionKey + "\x00" + groupKey }

// note records one step: kind "text" or "tool" adds it, "end" forgets the entry.
func (s *subagentActivityStore) note(sessionKey, groupKey, kind, text string) {
	if groupKey == "" {
		return
	}
	key := activityKey(sessionKey, groupKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "end" {
		delete(s.entries, key)
		return
	}
	line := activityLine(kind, text)
	if line == "" {
		return
	}
	e := s.entries[key]
	if e == nil {
		if len(s.entries) >= subagentActivityMaxEntries {
			s.evictOldestLocked()
		}
		e = &subagentActivityEntry{}
		s.entries[key] = e
	}
	e.items = append(e.items, line)
	if n := len(e.items); n > subagentActivityItems {
		e.items = append([]string(nil), e.items[n-subagentActivityItems:]...)
	}
	e.version++
	e.touched = s.clock()
}

func (s *subagentActivityStore) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range s.entries {
		if oldestKey == "" || e.touched.Before(oldest) {
			oldestKey, oldest = k, e.touched
		}
	}
	delete(s.entries, oldestKey)
}

// activityLine is one step as the model reads it: a label, then the text
// with whitespace collapsed, cut to subagentActivityItemRunes.
func activityLine(kind, text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	if r := []rune(text); len(r) > subagentActivityItemRunes {
		text = string(r[:subagentActivityItemRunes]) + "…"
	}
	if kind == "tool" {
		return "Tool call: " + text
	}
	return "Message: " + text
}

// summaryPlan is what to do for one requested id.
type summaryPlan struct {
	cached  string // the summary to send now ("" = none)
	start   bool   // make a model call for input
	input   string // the activity, when start
	version uint64 // the version input was taken at
	entry   *subagentActivityEntry
}

// plan decides, for one entry, whether to answer from the last summary and
// whether to start a model call: only when the activity changed since the
// last summary, no call is in flight, and the last call started at least
// subagentSummaryMinGap ago.
func (s *subagentActivityStore) plan(sessionKey, groupKey string) summaryPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[activityKey(sessionKey, groupKey)]
	if e == nil {
		return summaryPlan{}
	}
	p := summaryPlan{cached: e.summary, entry: e}
	now := s.clock()
	if e.version == e.summaryVersion || e.inflight || (!e.lastCall.IsZero() && now.Sub(e.lastCall) < subagentSummaryMinGap) {
		return p
	}
	e.inflight = true
	e.lastCall = now
	p.start = true
	p.version = e.version
	p.input = strings.Join(e.items, "\n")
	return p
}

// finish records a model call's outcome. A failed call ("" summary) keeps the
// last summary; the entry is tried again after the gap.
func (s *subagentActivityStore) finish(e *subagentActivityEntry, version uint64, summary string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.inflight = false
	if summary != "" {
		e.summary = summary
		e.summaryVersion = version
	}
}

// NoteSubagentActivity records one step of a subagent of sessionKey (#2960):
// kind "text" (a text block), "tool" (a tool call, as its name and input) or
// "end" (the run ended). The agent's subagent-activity hook calls it. A no-op
// when the app provider is not running.
func NoteSubagentActivity(sessionKey, groupKey, kind, text string) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil || h.subAct == nil {
		return
	}
	h.subAct.note(sessionKey, groupKey, kind, text)
}

// handleSubagentSummarize answers one subagent.summarize on the asking socket
// (#2960): each id's last summary at once, and a new one when its model call
// returns. An id with nothing to say gets no answer, so its entry shows as
// before.
func (h *Hub) handleSubagentSummarize(client *wsClient, f fap.SubagentSummarize) {
	if h.subAct == nil || h.pinSum == nil || !h.pinSum.usable(useShared) {
		return // off or no key: the shared path's startup warning covers it
	}
	b := h.convForReliability(f.ConversationID)
	if b == nil {
		return
	}
	session, agentID := b.key(), b.agent()
	ids := f.IDs
	if len(ids) > subagentSummaryMaxIDs {
		ids = ids[:subagentSummaryMaxIDs]
	}
	for _, id := range ids {
		p := h.subAct.plan(session, id)
		if p.cached != "" {
			client.sendRaw(fap.SubagentSummary{ConversationID: f.ConversationID, ID: id, Summary: p.cached})
		}
		if !p.start {
			continue
		}
		go h.summariseSubagent(client, f.ConversationID, id, session, agentID, p)
	}
}

func (h *Hub) summariseSubagent(client *wsClient, convID, id, session, agentID string, p summaryPlan) {
	ctx, cancel := context.WithCancel(context.Background()) // ask applies the timeout
	defer cancel()
	if client.done != nil {
		go func() {
			select {
			case <-client.done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	summary, err := h.pinSum.ask(ctx, subagentSummaryAsk, p.input, session, agentID)
	if err == nil && summary == "" {
		err = fmt.Errorf("feature off")
	}
	h.subAct.finish(p.entry, p.version, summary)
	if err != nil {
		appLog.Debugf("subagent summary %s/%s: %v", convID, id, err)
		return
	}
	client.sendRaw(fap.SubagentSummary{ConversationID: convID, ID: id, Summary: summary})
}

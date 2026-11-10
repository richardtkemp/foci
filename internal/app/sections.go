package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foci/internal/config"
	"foci/internal/epdispatch"
	"foci/internal/fap"
	"foci/internal/fastload"
	"foci/internal/session"
)

// Chat sections (#2993): a table of contents per app chat, found by the fast
// summary model (pinSummariser, the #2956 path) as the chat goes. The app
// shows it from the chat's top-bar overflow menu ("Contents") and jumps to a
// section's first message when one is tapped.
//
// State per chat lives in chat_metadata "sections" (JSON, chatSections): the
// sections found so far, oldest first, and the frame seq of the last message
// the last run read. It is the same on every device because the app asks the
// server for it (sections.get / sections) when it opens a chat or reconnects,
// and a run that changes the titled sections pushes them to every socket
// (#3010). The agent never sees it (Dick,
// 2026-11-09 Q50): nothing puts this key in a prompt.
//
// Rulings (Dick, 2026-11-09):
//   - only new messages are sectioned (Q49): a chat's first section starts at
//     the first chat message stored after this code first sees the chat;
//   - a run every chat_sections_every (5) new messages; after a run that
//     found a break, chat_sections_after_break (10) messages must pass before
//     the next run (F43);
//   - a run reads the CURRENT section only: its start, cut to the first
//     sectionHeadChars (5,000) characters, then its newest sectionTailMsgs
//     (10) messages (F43);
//   - messages carry compact ids M1, M2, ... in the input; the server maps the
//     model's answer back to real messages (F44);
//   - the run also updates the current section's title (F46); tiny sections
//     are allowed (F45).
//
// A failed call or an answer that does not parse changes nothing and is
// logged; the chat then waits sectionRetryGap before the next try.

// sectionsWhat names the chat sections' calls in the log, the load figures
// and the endpoint dispatcher's replacement key (#3019).
const sectionsWhat = "chat sections"

const (
	defaultSectionsEvery      = 5  // Dick F43: "Run every 5 messages"
	defaultSectionsAfterBreak = 10 // Dick F43: "minimum 10 messages to pass after a section break"

	// sectionHeadChars caps the section's start in the input (Dick F43:
	// "truncate section start to 5k characters").
	sectionHeadChars = 5000
	// sectionTailMsgs is how many of the section's newest messages follow the
	// start (Dick F43: "then show most recent ten messages").
	sectionTailMsgs = 10
	// sectionHeadMaxMsgs bounds the read of the section's start: 5,000
	// characters are reached long before this many messages unless they are
	// near empty.
	sectionHeadMaxMsgs = 200
	// sectionMsgRunes caps one of the newest messages in the input (its start
	// is kept), as autoNameMsgRunes does for the chat name.
	sectionMsgRunes = 4000
	// sectionTitleWords is the most words the prompt asks for in a title; a
	// title over 1.5 times this fails the run (summaryWordsOK).
	sectionTitleWords = 8
	// sectionTimeout bounds one model call.
	sectionTimeout = 45 * time.Second
	// sectionRetryGap is how long a chat whose run failed is left alone.
	sectionRetryGap = 10 * time.Minute
	// sectionMaxTokens is ample for two titles and the form around them.
	sectionMaxTokens = 100

	chatMetaSections = "sections"
)

const sectionSystemPrompt = "You divide chat conversations between a user and an AI agent into sections by topic, and give each section a short title. " +
	"The user message holds part of a conversation, and its instructions come at the end, after the conversation."

// chatSections is a chat's stored section state (chat_metadata "sections").
type chatSections struct {
	Sections []storedSection `json:"sections"`
	// ReadSeq is the frame seq of the newest message the last run read; at
	// the start, the seq just before the chat's first sectioned message.
	ReadSeq int64 `json:"readSeq"`
	// Broke is true when the last run found a section break, so the next run
	// waits for afterBreak messages instead of every.
	Broke bool `json:"broke,omitempty"`
}

// storedSection is one section: its first message and its title ("" until a
// run titles it).
type storedSection struct {
	StartSeq  int64  `json:"startSeq"`
	MessageID string `json:"messageId"`
	StartMs   int64  `json:"startMs"`
	Title     string `json:"title,omitempty"`
}

// sectionSettings is the live [platforms.app] chat_sections* config.
type sectionSettings struct {
	enabled    bool
	every      int // new messages between runs
	afterBreak int // new messages before the run after a break
}

// sectioner holds the live settings and which chats have a run going or are
// waiting out a failure (convClaims, shared with the autoNamer).
type sectioner struct {
	mu  sync.Mutex
	set sectionSettings
	convClaims

	// serveMu orders what the sockets hear (#3010): a run holds it from its
	// save to the end of its push, and a sections.get from its read to its
	// answer, so an answer read before a save cannot reach a socket after
	// that save's push and undo it there.
	serveMu sync.Mutex
}

func newSectioner(app *config.AppSpecific) *sectioner {
	s := &sectioner{convClaims: newConvClaims()}
	s.configure(app)
	return s
}

// configure applies [platforms.app] chat_sections, chat_sections_every and
// chat_sections_after_break. A nil app or an unset or non-positive field
// takes the code default.
func (s *sectioner) configure(app *config.AppSpecific) {
	set := sectionSettings{enabled: true, every: defaultSectionsEvery, afterBreak: defaultSectionsAfterBreak}
	if app != nil {
		if app.ChatSections != nil {
			set.enabled = *app.ChatSections
		}
		if app.ChatSectionsEvery != nil && *app.ChatSectionsEvery > 0 {
			set.every = *app.ChatSectionsEvery
		}
		if app.ChatSectionsAfterBreak != nil && *app.ChatSectionsAfterBreak > 0 {
			set.afterBreak = *app.ChatSectionsAfterBreak
		}
	}
	s.mu.Lock()
	s.set = set
	s.mu.Unlock()
}

// ConfigureChatSections is the live applier for [platforms.app]
// chat_sections, chat_sections_every and chat_sections_after_break
// (cmd/foci-gw/liveapply.go). A no-op when the app provider is not running.
func ConfigureChatSections(app *config.AppSpecific) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil || h.sections == nil {
		return
	}
	h.sections.configure(app)
}

// begin claims convID for one run and returns the settings to use; false
// when the feature is off or convClaims.claim refuses.
func (s *sectioner) begin(convID string) (sectionSettings, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set.enabled || !s.claim(convID) {
		return sectionSettings{}, false
	}
	return s.set, true
}

// end releases convID; failed starts its retry gap. It returns true when a
// message was stored during the run, so the caller checks again.
func (s *sectioner) end(convID string, failed bool) (again bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.release(convID, failed, sectionRetryGap)
}

// chatMessageStored is convBinding.onMessageStored: it runs, on a goroutine of
// its own, after a chat message of b is in the frame store, and starts the
// automatic chat name check (#2962) and the sections check (#2993), each on
// its own goroutine so one slow model call does not hold the other. It also
// marks the minute active for the fast-summary load figures (#3012).
func (h *Hub) chatMessageStored(b *convBinding) {
	fastload.Default.MarkActive()
	safeGo("chat-sections", func() { h.sectionCheck(b) })
	h.autoNameCheck(b)
}

// sectionCheck runs a sections run of b's chat when one is due; see the file
// comment.
func (h *Hub) sectionCheck(b *convBinding) {
	idx := h.deps.SessionIndex
	if h.sections == nil || h.frames == nil || idx == nil || !h.pinSum.usable(useSections) {
		return
	}
	// A newer message: a run of this chat still waiting for a local endpoint
	// is out of date. Replace it; its loop runs again (#3019).
	h.pinSum.replaceWaiting(useSections, sectionsWhat, b.convID)
	for {
		set, ok := h.sections.begin(b.convID)
		if !ok {
			return
		}
		failed := h.sectionRun(b, idx, set)
		if !h.sections.end(b.convID, failed) {
			return
		}
	}
}

// loadSections reads b's stored section state; ok is false when the chat has
// none yet. A value that does not decode is logged and read as none.
func loadSections(idx *session.SessionIndex, agentID string, chatID int64) (chatSections, bool) {
	raw, err := idx.GetChatMetadata(agentID, "app", chatID, chatMetaSections)
	if err != nil || strings.TrimSpace(raw) == "" {
		return chatSections{}, false
	}
	var st chatSections
	if err := json.Unmarshal([]byte(raw), &st); err != nil || len(st.Sections) == 0 {
		appLog.Warnf("chat sections: chat %d of %s: stored state does not decode: %v", chatID, agentID, err)
		return chatSections{}, false
	}
	return st, true
}

func saveSections(idx *session.SessionIndex, agentID string, chatID int64, st chatSections) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return idx.SetChatMetadata(agentID, "app", chatID, chatMetaSections, string(raw))
}

// sectionRun is one claimed check of b's chat. It returns true when it failed
// (the model call, its answer, or the write) and the chat should wait
// sectionRetryGap.
func (h *Hub) sectionRun(b *convBinding, idx *session.SessionIndex, set sectionSettings) (failed bool) {
	b.mu.Lock()
	hidden := b.depthRoot != "" || b.leaf != nil // a depth leaf's chat has no roster row
	b.mu.Unlock()
	if hidden {
		return false
	}
	agentID, sessionKey := b.identity()
	st, ok := loadSections(idx, agentID, b.chatID)
	if !ok {
		// The first message this code sees in the chat starts its first
		// section (Q49: only new messages). Stored at once, so the start does
		// not move when the first run comes.
		newest := h.frames.LastMessagesAfter(b.convID, 0, 1)
		if len(newest) == 0 {
			return false
		}
		m := newest[0]
		st = chatSections{Sections: []storedSection{{StartSeq: m.seq, MessageID: m.id, StartMs: m.sentMs}}, ReadSeq: m.seq - 1}
		if err := saveSections(idx, agentID, b.chatID, st); err != nil {
			appLog.Warnf("chat sections %s: record start: %v", b.convID, err)
			return true
		}
	}

	due := set.every
	if st.Broke {
		due = max(set.every, set.afterBreak)
	}
	if h.frames.CountMessagesAfter(b.convID, st.ReadSeq, due) < due {
		return false
	}

	prev := chatSections{Sections: slices.Clone(st.Sections)}
	cur := st.Sections[len(st.Sections)-1]
	head := h.frames.FirstMessagesAfter(b.convID, cur.StartSeq-1, sectionHeadMaxMsgs)
	tail := h.frames.LastMessagesAfter(b.convID, cur.StartSeq-1, sectionTailMsgs)
	input, shown := sectionInput(head, tail)
	if len(shown) == 0 {
		return false
	}

	ask := summaryAsk{
		what:          sectionsWhat,
		use:           useSections,
		chat:          b.convID,
		timeout:       sectionTimeout,
		system:        sectionSystemPrompt,
		maxInputRunes: utf8.RuneCountInString(input),
		maxTokens:     sectionMaxTokens,
		raw:           true,
	}
	answer, err := h.pinSum.ask(context.Background(), ask, input, sessionKey, agentID)
	if errors.Is(err, epdispatch.ErrReplaced) {
		return false // a newer check replaced it while it waited; it runs next
	}
	if err != nil {
		appLog.Warnf("chat sections %s: %v", b.convID, err)
		return true
	}
	if answer == "" { // the model was turned off under the call
		return false
	}
	res, err := parseSectionAnswer(answer, len(shown), sectionTitleWords)
	if err != nil {
		appLog.Warnf("chat sections %s: answer not used: %v: %q", b.convID, err, truncateRunes(answer, 300))
		return true
	}

	st.Sections[len(st.Sections)-1].Title = res.title
	st.Broke = false
	if res.newAt > 0 {
		m := shown[res.newAt-1]
		st.Sections = append(st.Sections, storedSection{StartSeq: m.seq, MessageID: m.id, StartMs: m.sentMs, Title: res.newTitle})
		st.Broke = true
	}
	st.ReadSeq = shown[len(shown)-1].seq
	h.sections.serveMu.Lock()
	defer h.sections.serveMu.Unlock()
	if err := saveSections(idx, agentID, b.chatID, st); err != nil {
		appLog.Warnf("chat sections %s: save: %v", b.convID, err)
		return true
	}
	// Push the change to every socket (#3010), so an open chat's dividers and
	// Contents update without asking. Only when what a socket is shown (the
	// titled sections) changed: most runs keep the same title.
	if pushed := titledSections(st); !slices.Equal(pushed, titledSections(prev)) {
		h.broadcastExcept(nil, fap.Sections{ConversationID: b.convID, Sections: pushed})
	}
	if res.newAt > 0 {
		appLog.Infof("chat sections %s: %q ends; %q starts at %s", b.convID, res.title, res.newTitle, shown[res.newAt-1].id)
	} else {
		appLog.Infof("chat sections %s: current section %q", b.convID, res.title)
	}
	return false
}

// sectionInput is the model's input for a run over the current section: its
// start (head, oldest first) cut to sectionHeadChars characters of message
// text, then its newest messages (tail, oldest first) that the start did not
// already hold, each numbered M1, M2, ... in order; then the instructions
// (after the text, where the 8B model follows them: the 03:32 tests of
// #2962). shown is the numbered messages: shown[k-1] is Mk. Messages with no
// text are left out and get no number.
func sectionInput(head, tail []chatMessage) (input string, shown []chatMessage) {
	var blocks []string
	add := func(m chatMessage, text string) {
		shown = append(shown, m)
		who := "Agent"
		if m.user {
			who = "User"
		}
		blocks = append(blocks, fmt.Sprintf("[M%d] %s: %s", len(shown), who, text))
	}

	used := 0
	var lastHead int64 = -1
	for _, m := range head {
		text := strings.Join(strings.Fields(m.text), " ")
		if text == "" {
			continue
		}
		left := sectionHeadChars - used
		if left <= 0 {
			break
		}
		lastHead = m.seq
		n := utf8.RuneCountInString(text)
		if n > left {
			add(m, truncateRunes(text, left)+" [...]")
			break
		}
		used += n
		add(m, text)
	}
	// leftOut reports whether a message with text lies between the start's
	// last shown message and seq next, so the input marks the gap.
	leftOut := func(next int64) bool {
		for _, m := range head {
			if m.seq > lastHead && m.seq < next && strings.TrimSpace(m.text) != "" {
				return true
			}
		}
		// The start read stopped at sectionHeadMaxMsgs: what lies beyond it
		// is unknown, so say a gap may be there.
		return len(head) >= sectionHeadMaxMsgs && head[len(head)-1].seq < next-1
	}

	first := true
	for _, m := range tail {
		if m.seq <= lastHead {
			continue
		}
		text := strings.Join(strings.Fields(m.text), " ")
		if text == "" {
			continue
		}
		if first && leftOut(m.seq) {
			blocks = append(blocks, "[... messages left out ...]")
		}
		first = false
		add(m, truncateRunes(text, sectionMsgRunes))
	}
	if len(shown) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("CONVERSATION (the current section, from its first message):\n\n")
	sb.WriteString(strings.Join(blocks, "\n\n"))
	sb.WriteString("\n\nEND OF CONVERSATION.\n\n")
	// The form with its two examples is the one that came back right on all
	// four live Llama 3.1 8B probes of 2026-11-09 (break at M5, break at M9,
	// two without); a form stated without examples lost the TITLE: label and
	// missed both breaks. The example's M7 was not copied.
	fmt.Fprintf(&sb, "Above is the current section of a chat between a user and an AI agent, messages M1 to M%d. "+
		"Does the chat move on to a clearly different topic at some message after M1? "+
		"Answer in exactly this form, two lines, nothing else:\n"+
		"TITLE: <title of the section from M1 until the new topic starts, or until the end if there is none>\n"+
		"NEW: <none, or the id of the message where the new topic starts, then | and a title for the new topic>\n\n"+
		"Example where a new topic starts at M7:\n"+
		"TITLE: Fixing the login bug\nNEW: M7 | Planning the team offsite\n\n"+
		"Example where the topic does not change:\n"+
		"TITLE: Fixing the login bug\nNEW: none\n\n"+
		"Each title is AT MOST %d WORDS, no quotes, no full stop.",
		len(shown), sectionTitleWords)
	return sb.String(), shown
}

// sectionResult is a parsed answer: the current section's title, and, when
// newAt > 0, the compact number of the message where a new section starts
// and its title.
type sectionResult struct {
	title    string
	newAt    int
	newTitle string
}

var sectionNewRe = regexp.MustCompile(`(?i)^\[?M(\d+)\]?\s*(?:[|:\-–—]\s*)?(.*)$`)

// parseSectionAnswer reads the model's two-line answer (see sectionInput) for
// a run that showed n messages. A missing NEW line reads as none, NEW at M1
// (the section's own start) as no break, and an unlabelled first line as the
// title. Any other deviation (no title, an
// id outside M1..Mn, a title over 1.5 times words) is an error, so the run
// changes nothing.
func parseSectionAnswer(answer string, n, words int) (sectionResult, error) {
	var res sectionResult
	var titleSeen, newSeen bool
	var newVal, bare string
	for _, line := range strings.Split(answer, "\n") {
		line = strings.ReplaceAll(strings.TrimSpace(line), "**", "")
		line = strings.TrimSpace(strings.TrimLeft(line, "-*#> "))
		upper := strings.ToUpper(line)
		switch {
		case !titleSeen && strings.HasPrefix(upper, "TITLE:"):
			res.title = cleanPinSummary(line[len("TITLE:"):])
			titleSeen = true
		case !newSeen && strings.HasPrefix(upper, "NEW:"):
			newVal = strings.TrimSpace(line[len("NEW:"):])
			newSeen = true
		case bare == "" && !newSeen && line != "":
			bare = line
		}
	}
	// The 8B model sometimes writes the title with no label (seen live,
	// 2026-11-09): a lone first line before a NEW: line is the title. Without
	// a NEW: line the answer is not in the form at all (prose), so no.
	if !titleSeen && newSeen && bare != "" {
		res.title = cleanPinSummary(bare)
		titleSeen = true
	}
	if !titleSeen || res.title == "" {
		return sectionResult{}, fmt.Errorf("no TITLE line")
	}
	if !summaryWordsOK(res.title, words) {
		return sectionResult{}, fmt.Errorf("title has %d words, limit %d", len(strings.Fields(res.title)), words)
	}
	switch strings.ToLower(strings.TrimRight(newVal, ".")) {
	case "", "none", "no", "-", "n/a":
		return res, nil
	}
	m := sectionNewRe.FindStringSubmatch(newVal)
	if m == nil {
		return sectionResult{}, fmt.Errorf("NEW %q names no message", newVal)
	}
	k, err := strconv.Atoi(m[1])
	if err != nil || k < 1 || k > n {
		return sectionResult{}, fmt.Errorf("NEW names M%s, outside M1..M%d", m[1], n)
	}
	if k == 1 {
		return res, nil // the section's own start: no break
	}
	newTitle := cleanPinSummary(m[2])
	if newTitle == "" {
		return sectionResult{}, fmt.Errorf("NEW at M%d has no title", k)
	}
	if !summaryWordsOK(newTitle, words) {
		return sectionResult{}, fmt.Errorf("new title has %d words, limit %d", len(strings.Fields(newTitle)), words)
	}
	res.newAt, res.newTitle = k, newTitle
	return res, nil
}

// handleSectionsGet answers one sections.get (#2993) on the asking socket
// with the chat's titled sections, oldest first; [] when it has none, the
// conversation is unknown, or there is no session index.
func (h *Hub) handleSectionsGet(client *wsClient, f fap.SectionsGet) {
	if f.RequestID == "" || f.ConversationID == "" {
		return
	}
	if h.sections != nil {
		h.sections.serveMu.Lock()
		defer h.sections.serveMu.Unlock()
	}
	client.sendRaw(h.sectionsFor(f.RequestID, f.ConversationID))
}

func (h *Hub) sectionsFor(requestID, convID string) fap.Sections {
	out := fap.Sections{RequestID: requestID, ConversationID: convID, Sections: []fap.ChatSection{}}
	idx := h.deps.SessionIndex
	if idx == nil {
		return out
	}
	h.mu.RLock()
	b := h.convs[convID]
	h.mu.RUnlock()
	if b == nil {
		return out
	}
	st, ok := loadSections(idx, b.agent(), b.chatID)
	if !ok {
		return out
	}
	out.Sections = titledSections(st)
	return out
}

// titledSections is st's sections as the app sees them: the titled ones,
// oldest first, never nil.
func titledSections(st chatSections) []fap.ChatSection {
	out := []fap.ChatSection{}
	for _, s := range st.Sections {
		if s.Title == "" {
			continue
		}
		out = append(out, fap.ChatSection{MessageID: s.MessageID, Title: s.Title, StartMs: s.StartMs})
	}
	return out
}

package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"foci/internal/config"
	"foci/internal/fap"
	"foci/internal/session"
)

// Automatic chat name (#2962). An open app chat whose name was not set on
// purpose gets a name from its conversation, written by the pin summary's
// fast model (#2956: same model, provider order and key). The first name is
// made once the chat holds auto_name_first messages; after that the name is
// checked again every auto_name_every new messages, and the model keeps it
// unless it no longer fits.
//
// "Set on purpose" is every name this code did not write: the user's rename,
// an agent's set_session_alias, a backend thread name. The name this code
// writes is kept in chat_metadata alias_summary beside alias, so a chat is
// ours to rename only while alias is empty or still equal to alias_summary;
// any other writer changes alias and so takes the chat out of the feature
// without touching this code. The write itself is a compare-and-set
// (SessionIndex.SetChatAliasIfUnchanged), so a rename that lands while the
// model call runs is never overwritten.
//
// The count is read from the durable frame store (chatMessageWhere), from the
// frame seq recorded in alias_summary_seq at the last naming, so it survives
// a restart. A failed, slow or over-long answer changes nothing; the chat is
// then not tried again for autoNameRetryGap.

const (
	defaultAutoNameFirst = 10 // Dick: "First summary in a new session to run after ten messages"
	defaultAutoNameEvery = 50 // Dick: "every ... ?50 messages"
	defaultAutoNameWords = 6  // clutch's default on the ticket: "at most six words"

	// autoNameMsgRunes caps one message's text in the model's input (its
	// start is kept).
	autoNameMsgRunes = 4000
	// autoNameInputRunes caps the conversation part of the input; the oldest
	// messages are left out first. About 30k tokens: the 03:32 tests named
	// 55k-109k token inputs in 3.4-6.3 s.
	autoNameInputRunes = 120000
	// autoNameTimeout bounds one model call.
	autoNameTimeout = 45 * time.Second
	// autoNameRetryGap is how long a chat whose naming failed is left alone,
	// so a broken provider is not called on every message.
	autoNameRetryGap = 10 * time.Minute
	// autoNameMaxTokens is ample for a title and stops a runaway answer.
	autoNameMaxTokens = 40

	chatMetaAliasSummary    = "alias_summary"     // the name this code last wrote
	chatMetaAliasSummarySeq = "alias_summary_seq" // the frame seq of the last message read at that naming
)

const autoNameSystemPrompt = "You write short titles for chat conversations between a user and an AI agent. " +
	"The user message holds the conversation, and its instructions come at the end, after the conversation."

// autoNameSettings is the live [platforms.app] auto_name* config.
type autoNameSettings struct {
	enabled bool
	first   int // messages before the first name
	every   int // new messages before the name is checked again
	words   int // the most words the prompt asks for
}

// autoNamer holds the live settings and which chats are being named or are
// waiting out a failure.
type autoNamer struct {
	mu  sync.Mutex
	set autoNameSettings
	convClaims
}

func newAutoNamer(app *config.AppSpecific) *autoNamer {
	n := &autoNamer{convClaims: newConvClaims()}
	n.configure(app)
	return n
}

// configure applies [platforms.app] auto_name, auto_name_first,
// auto_name_every and auto_name_words. A nil app or an unset or non-positive
// field takes the code default.
func (n *autoNamer) configure(app *config.AppSpecific) {
	set := autoNameSettings{enabled: true, first: defaultAutoNameFirst, every: defaultAutoNameEvery, words: defaultAutoNameWords}
	if app != nil {
		if app.AutoName != nil {
			set.enabled = *app.AutoName
		}
		if app.AutoNameFirst != nil && *app.AutoNameFirst > 0 {
			set.first = *app.AutoNameFirst
		}
		if app.AutoNameEvery != nil && *app.AutoNameEvery > 0 {
			set.every = *app.AutoNameEvery
		}
		if app.AutoNameWords != nil && *app.AutoNameWords > 0 {
			set.words = *app.AutoNameWords
		}
	}
	n.mu.Lock()
	n.set = set
	n.mu.Unlock()
}

// ConfigureAutoName is the live applier for [platforms.app] auto_name,
// auto_name_first, auto_name_every and auto_name_words
// (cmd/foci-gw/liveapply.go). A no-op when the app provider is not running.
func ConfigureAutoName(app *config.AppSpecific) {
	activeMu.RLock()
	h := activeHub
	activeMu.RUnlock()
	if h == nil || h.autoName == nil {
		return
	}
	h.autoName.configure(app)
}

// begin claims convID for one check and returns the settings to use. false
// when the feature is off, a check of convID is already running, or convID
// is waiting out a failure (convClaims.claim).
func (n *autoNamer) begin(convID string) (autoNameSettings, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.set.enabled || !n.claim(convID) {
		return autoNameSettings{}, false
	}
	return n.set, true
}

// end releases convID; failed starts its retry gap. It returns true when a
// message was stored during the check, so the caller checks again.
func (n *autoNamer) end(convID string, failed bool) (again bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.release(convID, failed, autoNameRetryGap)
}

// autoNameCounts reports whether frame is a chat message the automatic name
// counts and reads: a whole message row from the user or the agent, or the
// end of a streamed agent reply. It must accept the same frames
// chatMessageWhere selects from the store.
func autoNameCounts(frame fap.ServerFrame) bool {
	switch f := frame.(type) {
	case fap.ServerMessage:
		return f.Role != "system"
	case fap.TextEnd:
		return true
	}
	return false
}

// autoNameCheck runs after a chat message of b is stored (convBinding.
// onMessageStored, on a goroutine of its own). It names b's chat, or checks
// its automatic name, when the chat is due; see the file comment.
func (h *Hub) autoNameCheck(b *convBinding) {
	idx := h.deps.SessionIndex
	if h.autoName == nil || h.frames == nil || idx == nil || !h.pinSum.usable() {
		return
	}
	for {
		set, ok := h.autoName.begin(b.convID)
		if !ok {
			return
		}
		if !h.autoNameOnce(b, idx, set) {
			return
		}
	}
}

// autoNameOnce runs one claimed check and releases the claim; it returns
// true when the chat should be checked again at once.
func (h *Hub) autoNameOnce(b *convBinding, idx *session.SessionIndex, set autoNameSettings) bool {
	failed := h.autoNameRun(b, idx, set)
	return h.autoName.end(b.convID, failed)
}

// autoNameRun is one check of b's chat; it returns true when the check failed
// (the model call, or the write) and the chat should wait autoNameRetryGap.
func (h *Hub) autoNameRun(b *convBinding, idx *session.SessionIndex, set autoNameSettings) (failed bool) {
	b.mu.Lock()
	hidden := b.depthRoot != "" || b.leaf != nil // a depth leaf's chat has no roster row
	b.mu.Unlock()
	if hidden {
		return false
	}
	agentID, sessionKey := b.identity()
	alias, err := idx.GetChatMetadata(agentID, "app", b.chatID, "alias")
	if err != nil {
		return false
	}
	ours, _ := idx.GetChatMetadata(agentID, "app", b.chatID, chatMetaAliasSummary)
	markStr, _ := idx.GetChatMetadata(agentID, "app", b.chatID, chatMetaAliasSummarySeq)

	prev := ""
	if strings.TrimSpace(alias) != "" {
		if strings.TrimSpace(ours) == "" || strings.TrimSpace(alias) != strings.TrimSpace(ours) {
			return false // a name set on purpose: never replaced
		}
		prev = strings.TrimSpace(alias)
	}
	mark, _ := strconv.ParseInt(markStr, 10, 64)
	due := set.first
	if prev != "" {
		due = set.every
	}
	if h.frames.CountMessagesAfter(b.convID, mark, due) < due {
		return false
	}
	if !h.convOpen(b.convID) {
		return false
	}
	msgs := h.frames.LastMessagesAfter(b.convID, mark, max(set.first, set.every))
	if len(msgs) == 0 {
		return false
	}
	input := autoNameInput(msgs, prev, set.words)

	ctx, cancel := context.WithTimeout(context.Background(), autoNameTimeout)
	defer cancel()
	ask := summaryAsk{
		what:          "auto chat name",
		system:        autoNameSystemPrompt,
		maxInputRunes: utf8.RuneCountInString(input),
		maxTokens:     autoNameMaxTokens,
		maxWords:      set.words,
	}
	name, err := h.pinSum.ask(ctx, ask, input, sessionKey, agentID)
	if err != nil {
		appLog.Warnf("auto chat name %s: %v", b.convID, err)
		return true
	}
	if name == "" { // the model was turned off under the call
		return false
	}
	lastSeq := strconv.FormatInt(msgs[len(msgs)-1].seq, 10)

	if prev != "" && strings.EqualFold(strings.TrimSpace(name), prev) {
		if err := idx.SetChatMetadata(agentID, "app", b.chatID, chatMetaAliasSummarySeq, lastSeq); err != nil {
			appLog.Warnf("auto chat name %s: record check: %v", b.convID, err)
			return true
		}
		appLog.Infof("auto chat name %s: kept %q", b.convID, prev)
		return false
	}
	err = idx.SetChatAliasIfUnchanged(agentID, "app", b.chatID, alias, name, map[string]string{
		// alias_auto 1: an agent's set_session_alias may still replace it.
		"alias_auto":            "1",
		chatMetaAliasSummary:    name,
		chatMetaAliasSummarySeq: lastSeq,
	})
	switch {
	case errors.Is(err, session.ErrAliasChanged):
		appLog.Infof("auto chat name %s: the name changed during the call; %q not used", b.convID, name)
		return false
	case err != nil:
		appLog.Warnf("auto chat name %s: set %q: %v", b.convID, name, err)
		return true
	}
	if prev == "" {
		appLog.Infof("auto chat name %s: named %q", b.convID, name)
	} else {
		appLog.Infof("auto chat name %s: renamed %q to %q", b.convID, prev, name)
	}
	h.pushRosterAll()
	return false
}

// convOpen reports whether convID is in the shared open-set (open_chats).
func (h *Hub) convOpen(convID string) bool {
	for _, id := range h.loadOpenChats() {
		if id == convID {
			return true
		}
	}
	return false
}

// autoNameInput is the model's input: the conversation, then the
// instructions. The instructions come last because the 03:32 tests showed the
// model ignores an instruction in the system prompt alone over a long text
// and copies the text's start; after the text it obeys (Dick's ruling too:
// "The prompt (at the end) should include the previous name"). The newest
// messages are kept when the conversation is over autoNameInputRunes.
func autoNameInput(msgs []chatMessage, prev string, words int) string {
	blocks := make([]string, 0, len(msgs))
	total := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		text := strings.Join(strings.Fields(msgs[i].text), " ")
		if text == "" {
			continue
		}
		text = truncateRunes(text, autoNameMsgRunes)
		who := "Agent"
		if msgs[i].user {
			who = "User"
		}
		block := who + ": " + text
		n := utf8.RuneCountInString(block)
		if total+n > autoNameInputRunes && len(blocks) > 0 {
			break
		}
		blocks = append(blocks, block)
		total += n
	}
	for i, j := 0, len(blocks)-1; i < j; i, j = i+1, j-1 {
		blocks[i], blocks[j] = blocks[j], blocks[i]
	}

	var sb strings.Builder
	sb.WriteString("CONVERSATION:\n\n")
	sb.WriteString(strings.Join(blocks, "\n\n"))
	sb.WriteString("\n\nEND OF CONVERSATION.\n\n")
	const form = "Reply with the title only: no quotes, no preamble, no trailing full stop, one line."
	if prev == "" {
		fmt.Fprintf(&sb, "Above is a chat between a user and an AI agent. "+
			"Write a title for this chat that says what it is about, in AT MOST %d WORDS. "+
			"This limit is strict. %s", words, form)
	} else {
		fmt.Fprintf(&sb, "Above are the latest messages of a chat between a user and an AI agent. "+
			"The chat's current title is: %s\n"+
			"If that title still fits what the chat is about, reply with exactly that title. "+
			"Only if it no longer fits, write a new title that says what the chat is about now, in AT MOST %d WORDS. "+
			"This limit is strict. %s", prev, words, form)
	}
	return sb.String()
}

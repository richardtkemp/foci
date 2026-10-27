package app

// Depth mode (#2588): leaf views.
//
// A depth-mode node is an ordinary branch session whose app output belongs to
// the conversation of the session that started the tree (the root). Nothing is
// minted for a leaf: no conversation row, no roster entry, no open-set entry.
// Instead bySession[branchKey] points at a LEAF VIEW of the root's binding — a
// convBinding whose leaf field is set — so every existing path that finds a
// binding by session key (the turn sink, asks and permission prompts,
// send_to_chat, notices, activity) lands in the root conversation, each frame
// stamped with the leaf's id. The view owns no stream: sendThen forwards to the
// root binding (leafView.forward), which numbers, stores, replays and pushes
// the frame as its own. The view keeps only its own activity inputs, so a leaf
// turn never moves the root's indicator.
//
// Views hold no durable state: the depth manager (tools.DepthManager) rebuilds
// them from its node records at startup.

import (
	"errors"
	"strings"
	"sync"

	"foci/internal/fap"
	"foci/internal/tools"
)

// leafView is the depth-mode part of a leaf's binding.
type leafView struct {
	root  *convBinding // the tree root's conversation binding
	id    string       // leaf id, stamped on every frame
	title string       // leaf title, stamped where a notification needs it

	mu     sync.Mutex
	stream map[string]*strings.Builder // turnID → agent text streamed so far
	busy   bool                        // a turn is running (last Activity was not idle)
}

func depthLeafKey(convID, leafID string) string { return convID + "\x00" + leafID }

// DepthController is the depth manager side the hub calls back into. Set
// once at startup (SetDepthController); nil = depth mode off.
type DepthController interface {
	// LeafUserMessage runs before a user message to the leaf on branchKey is
	// queued (a done leaf becomes a side thread).
	LeafUserMessage(branchKey string)
	// LeafText reports a message the leaf on branchKey sent the user.
	LeafText(branchKey, text string)
	// LeafBusy reports that the leaf's turn started (true) or ended (false).
	LeafBusy(branchKey string, busy bool)
	// Ack and Dismiss are the feed's two actions on leafID of the tree whose
	// root session is rootKey.
	Ack(rootKey, leafID string) error
	Dismiss(rootKey, leafID string) error
}

var (
	depthCtlMu sync.RWMutex
	depthCtl   DepthController
)

// SetDepthController installs the depth manager the hub reports leaf events
// to and hands the feed's actions to.
func SetDepthController(c DepthController) {
	depthCtlMu.Lock()
	depthCtl = c
	depthCtlMu.Unlock()
}

func depthController() DepthController {
	depthCtlMu.RLock()
	defer depthCtlMu.RUnlock()
	return depthCtl
}

// forward stamps frame with the leaf's id and sends it on the root binding.
// Frames that describe the conversation as a whole (its model and cost, its
// cache warmth, its command palette) are dropped: a leaf's would overwrite the
// root's. b is the leaf's own binding (for its session key).
func (l *leafView) forward(b *convBinding, frame fap.ServerFrame, onRecorded func()) {
	stamped, ok := l.stamp(frame)
	if !ok {
		if onRecorded != nil {
			onRecorded()
		}
		return
	}
	text, isText := l.observeText(stamped)
	busy, busyChanged := l.observeBusy(stamped)
	l.root.sendThen(stamped, onRecorded)
	ctl := depthController()
	if ctl == nil {
		return
	}
	if isText {
		ctl.LeafText(b.sessionKey, text)
	}
	if busyChanged {
		ctl.LeafBusy(b.sessionKey, busy)
	}
}

// stamp returns frame with LeafID (and LeafTitle where the frame has one) set,
// or ok=false for a frame a leaf must not send to the root conversation.
// Frames not listed pass through unchanged.
func (l *leafView) stamp(frame fap.ServerFrame) (fap.ServerFrame, bool) {
	id, title := l.id, l.title
	switch f := frame.(type) {
	case fap.Meta, fap.CacheExpiry, fap.Commands:
		return nil, false
	case fap.TurnStart:
		f.LeafID = id
		return f, true
	case fap.TextDelta:
		f.LeafID = id
		return f, true
	case fap.TextEnd:
		f.LeafID, f.LeafTitle = id, title
		return f, true
	case fap.ServerMessage:
		f.LeafID, f.LeafTitle = id, title
		return f, true
	case fap.ExternalPrompt:
		f.LeafID = id
		return f, true
	case fap.Notification:
		f.LeafID = id
		return f, true
	case fap.Activity:
		f.LeafID = id
		return f, true
	case fap.Media:
		f.LeafID = id
		return f, true
	case fap.Interactive:
		f.LeafID, f.LeafTitle = id, title
		return f, true
	case fap.InteractiveEdit:
		f.LeafID = id
		return f, true
	case fap.InteractiveProgressEdit:
		f.LeafID = id
		return f, true
	case fap.InteractiveRemove:
		f.LeafID = id
		return f, true
	case fap.MessageConsumed:
		f.LeafID = id
		return f, true
	case fap.MessageEdit:
		f.LeafID = id
		return f, true
	}
	return frame, true
}

// observeText returns the text of an agent message the frame completes: a
// whole agent message, or the end of a streamed one (its final text, else what
// streamed). Deltas are collected on the way.
func (l *leafView) observeText(frame fap.ServerFrame) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch f := frame.(type) {
	case fap.TextDelta:
		if l.stream == nil {
			l.stream = make(map[string]*strings.Builder)
		}
		sb := l.stream[f.TurnID]
		if sb == nil {
			sb = &strings.Builder{}
			l.stream[f.TurnID] = sb
		}
		sb.WriteString(f.Text)
	case fap.TextEnd:
		var text string
		if f.FinalText != nil {
			text = *f.FinalText
		} else if sb := l.stream[f.TurnID]; sb != nil {
			text = sb.String()
		}
		delete(l.stream, f.TurnID)
		if strings.TrimSpace(text) != "" {
			return text, true
		}
	case fap.ServerMessage:
		if f.Role == "agent" && strings.TrimSpace(f.Text) != "" {
			return f.Text, true
		}
	}
	return "", false
}

// observeBusy tracks whether the leaf has a turn running, from its Activity
// frames, and reports a change.
func (l *leafView) observeBusy(frame fap.ServerFrame) (busy, changed bool) {
	a, ok := frame.(fap.Activity)
	if !ok {
		return false, false
	}
	busy = a.Kind != "" && a.Kind != string(fap.ActivityKindIdle)
	l.mu.Lock()
	defer l.mu.Unlock()
	if busy == l.busy {
		return busy, false
	}
	l.busy = busy
	return busy, true
}

// leafTitleOf returns the leaf title a frame carries, for its push preview.
func leafTitleOf(frame fap.ServerFrame) string {
	switch f := frame.(type) {
	case fap.ServerMessage:
		return f.LeafTitle
	case fap.TextEnd:
		return f.LeafTitle
	case fap.Interactive:
		return f.LeafTitle
	}
	return ""
}

var (
	errDepthNoRoot   = errors.New("the root session is not bound to an app conversation")
	errDepthBranchIn = errors.New("the branch session already has a conversation of its own")
)

// registerDepthLeaf binds branchKey to a leaf view of rootKey's conversation,
// replacing any earlier view of the same branch (a restart rebuild, a retitle).
func (h *Hub) registerDepthLeaf(rootKey, branchKey, leafID, title string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	root := h.bySession[rootKey]
	if root == nil {
		return errDepthNoRoot
	}
	if root.leaf != nil {
		root = root.leaf.root
	}
	if cur := h.bySession[branchKey]; cur != nil {
		if cur.leaf == nil {
			return errDepthBranchIn
		}
		delete(h.depthLeaves, depthLeafKey(cur.convID, cur.leaf.id))
	}
	lb := &convBinding{
		convID:     root.convID,
		sessionKey: branchKey,
		agentID:    root.agentID,
		chatID:     root.chatID,
		leaf:       &leafView{root: root, id: leafID, title: title},
	}
	h.bySession[branchKey] = lb
	if h.depthLeaves == nil {
		h.depthLeaves = make(map[string]*convBinding)
	}
	h.depthLeaves[depthLeafKey(root.convID, leafID)] = lb
	return nil
}

// unregisterDepthLeaf removes branchKey's leaf view, if it has one.
func (h *Hub) unregisterDepthLeaf(branchKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.bySession[branchKey]
	if cur == nil || cur.leaf == nil {
		return
	}
	delete(h.bySession, branchKey)
	delete(h.depthLeaves, depthLeafKey(cur.convID, cur.leaf.id))
}

// depthLeaf returns the view of leaf leafID in conversation convID, or nil.
func (h *Hub) depthLeaf(convID, leafID string) *convBinding {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.depthLeaves[depthLeafKey(convID, leafID)]
}

// rootBinding returns the conversation binding sessionKey's output lands in:
// its own, or for a leaf the root's. nil when it has none.
func (h *Hub) rootBinding(sessionKey string) *convBinding {
	b := h.bindingForSession(sessionKey)
	if b != nil && b.leaf != nil {
		return b.leaf.root
	}
	return b
}

// handleDepthAction hands the feed's Ack / Dismiss on leafID to the depth
// manager, or tells the device depth mode is not running here.
func (h *Hub) handleDepthAction(client *wsClient, convID, leafID string, ack bool) {
	if convID == "" || leafID == "" {
		return
	}
	h.mu.RLock()
	b := h.convs[convID]
	h.mu.RUnlock()
	ctl := depthController()
	if b == nil || ctl == nil {
		client.sendRaw(fap.ErrorFrame{ConversationID: convID, Code: "no_depth", Message: "depth mode is not running for this conversation"})
		return
	}
	var err error
	if ack {
		err = ctl.Ack(b.sessionKey, leafID)
	} else {
		err = ctl.Dismiss(b.sessionKey, leafID)
	}
	if err != nil {
		client.sendRaw(fap.ErrorFrame{ConversationID: convID, Code: "depth_refused", Message: err.Error()})
	}
}

// DepthSurface is the app side of depth mode, handed to the depth manager by
// the gateway. Every method is a no-op (or false) when the app provider is not
// running.
type DepthSurface struct{}

var _ tools.DepthSurface = DepthSurface{}

func depthHub() *Hub {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return activeHub
}

// RootBound reports whether rootKey is bound to an app conversation of its own
// (not a leaf view): only such a session may start a tree.
func (DepthSurface) RootBound(rootKey string) bool {
	h := depthHub()
	if h == nil {
		return false
	}
	b := h.bindingForSession(rootKey)
	return b != nil && b.leaf == nil
}

// RegisterLeaf routes branchKey's app output into rootKey's conversation as
// leaf leafID.
func (DepthSurface) RegisterLeaf(rootKey, branchKey, leafID, title string) error {
	h := depthHub()
	if h == nil {
		return errors.New("app provider not running")
	}
	return h.registerDepthLeaf(rootKey, branchKey, leafID, title)
}

// UnregisterLeaf drops branchKey's leaf view.
func (DepthSurface) UnregisterLeaf(branchKey string) {
	if h := depthHub(); h != nil {
		h.unregisterDepthLeaf(branchKey)
	}
}

// PushFeed sends the tree's whole state to rootKey's conversation as one
// durable depth.feed frame.
func (DepthSurface) PushFeed(rootKey, rootState string, nodes []fap.DepthNode) {
	h := depthHub()
	if h == nil {
		return
	}
	b := h.rootBinding(rootKey)
	if b == nil {
		return
	}
	if nodes == nil {
		nodes = []fap.DepthNode{}
	}
	b.send(fap.DepthFeed{ConversationID: b.convID, RootState: rootState, Nodes: nodes})
}

// ShowPrompt opens a leaf's exchange with the prompt it was given, as a
// durable prompt row from the parent session.
func (DepthSurface) ShowPrompt(branchKey, text, fromSession string) {
	deliverBoundPrompt(branchKey, text, "", fromSession)
}

// LeafBusy reports whether the leaf on branchKey has a turn running.
func (DepthSurface) LeafBusy(branchKey string) bool {
	h := depthHub()
	if h == nil {
		return false
	}
	b := h.bindingForSession(branchKey)
	if b == nil || b.leaf == nil {
		return false
	}
	b.leaf.mu.Lock()
	defer b.leaf.mu.Unlock()
	return b.leaf.busy
}

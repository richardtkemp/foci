package app

// Depth mode (#2588): leaf conversations.
//
// A depth-mode node is an ordinary branch session with an app conversation of
// its own (#2611): every per-conversation mechanism (seq stream, replay, the
// read watermark and its sync, unread, pushes, asks, activity) applies to a
// leaf exactly as to a chat. What makes it a leaf is the chat_metadata row
// depth_root = <the root's conversation id>, which keeps it out of the roster
// list (ConversationInfo.DepthRootID; the client's depth view shows it instead)
// and out of default-destination resolution. The binding's leaf field adds the
// depth manager's hooks: the leaf's messages (LeafText), busy flips (LeafBusy)
// and the user's messages to it (LeafUserMessage).
//
// The conversation outlives the view: the depth manager rebuilds the views
// from its node records at startup (RegisterLeaf with the node's stored
// conversation id), and a view dropped by UnregisterLeaf leaves the
// conversation, still hidden, with its history.

import (
	"errors"
	"strings"
	"sync"

	"foci/internal/fap"
	"foci/internal/tools"
)

// leafView is the depth-mode part of a leaf conversation's binding.
type leafView struct {
	rootConv string // the tree root's conversation id
	id       string // leaf id (the depth manager's name for the node)
	title    string // leaf title, prefixed to the leaf's push previews

	mu     sync.Mutex
	stream map[string]*strings.Builder // turnID → agent text streamed so far
	busy   bool                        // a turn is running (last Activity was not idle)
}

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

// observe runs after frame was sent on the leaf's own conversation: it tells
// the depth manager about a finished agent message and a busy flip. b is the
// leaf's binding (for its session key).
func (l *leafView) observe(b *convBinding, frame fap.ServerFrame) {
	text, isText := l.observeText(frame)
	busy, busyChanged := l.observeBusy(frame)
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

var (
	errDepthNoRoot   = errors.New("the root session is not bound to an app conversation")
	errDepthBranchIn = errors.New("the branch session already has a conversation of its own")
)

// chatMetaDepthRoot marks a leaf's conversation with its tree root's
// conversation id (#2611): hidden from the roster list, never a default
// destination.
const chatMetaDepthRoot = "depth_root"

// registerDepthLeaf binds branchKey to its leaf conversation convID ("" = mint
// one) under rootKey's conversation, and returns the conversation id. A
// re-registration (restart rebuild, retitle) reuses the stored id. Devices
// already connected are attached at once, so the leaf's first frames reach
// them live, and learn the conversation from a roster push.
func (h *Hub) registerDepthLeaf(rootKey, branchKey, leafID, title, convID string) (string, error) {
	h.mu.RLock()
	root := h.bySession[rootKey]
	cur := h.bySession[branchKey]
	h.mu.RUnlock()
	if root == nil || root.isDepthLeaf() {
		return "", errDepthNoRoot
	}
	if cur != nil && !cur.isDepthLeaf() {
		return "", errDepthBranchIn
	}
	if cur != nil {
		convID = cur.convID
	}
	minted := convID == ""
	if minted {
		convID = fap.NewULID()
	}
	chatID := chatIDForConv(convID)
	if idx := h.deps.SessionIndex; idx != nil {
		// Written BEFORE ensureBinding, so sessionKeyForChat resolves the branch
		// key from session_key and never writes registered=true: a leaf is not a
		// routable chat (DefaultSessionKeyForAgentOn filters on that row).
		_ = idx.SetChatMetadata(root.agentID, "app", chatID, "session_key", branchKey)
		_ = idx.SetChatMetadata(root.agentID, "app", chatID, chatMetaDepthRoot, root.convID)
	}
	b := h.ensureBinding(nil, root.agentID, convID)
	root.mu.Lock()
	rootFeatures := make(map[string]struct{}, len(root.features))
	for f := range root.features {
		rootFeatures[f] = struct{}{}
	}
	root.mu.Unlock()
	b.mu.Lock()
	if len(b.features) == 0 {
		// A leaf minted while no device is connected still renders its asks the
		// way the root's devices can show them.
		b.features = rootFeatures
	}
	b.depthRoot = root.convID
	b.leaf = &leafView{rootConv: root.convID, id: leafID, title: title}
	derived := b.sessionKey
	b.sessionKey = branchKey
	b.mu.Unlock()
	h.mu.Lock()
	if derived != branchKey && h.bySession[derived] == b {
		// No SessionIndex to hold session_key: ensureBinding derived the chat key.
		delete(h.bySession, derived)
	}
	h.bySession[branchKey] = b
	h.mu.Unlock()
	h.attachConnected(b)
	// A leaf registered under an archived root (a fan-out from an archived
	// chat, or a restart re-registering a tree archived before #2613) is
	// archived with it.
	archivedChanged := h.syncLeafArchived(root, b)
	if minted || cur == nil || archivedChanged {
		h.pushRosterAll()
	}
	return convID, nil
}

// Retention (#2613, Dick 2026-10-28): a leaf's conversation follows its root
// conversation. It is archived when the root is archived and unarchived with
// it; there is no time-based expiry. The app has no conversation delete, so
// there is no delete to follow yet: a future root delete must remove
// depthLeavesOf(root) with it.

// depthLeavesOf returns the live bindings of every leaf conversation of root,
// from any of its trees (a leaf's binding outlives its depth record).
func (h *Hub) depthLeavesOf(root *convBinding) []*convBinding {
	h.mu.RLock()
	all := make([]*convBinding, 0, len(h.convs))
	for _, b := range h.convs {
		all = append(all, b)
	}
	h.mu.RUnlock()
	var out []*convBinding
	for _, b := range all {
		if b == root || b.agentID != root.agentID {
			continue
		}
		b.mu.Lock()
		leaf := b.depthRoot == root.convID
		b.mu.Unlock()
		if leaf {
			out = append(out, b)
		}
	}
	return out
}

// syncDepthLeavesArchived sets every leaf conversation of root to archived,
// and returns how many flags it changed.
func (h *Hub) syncDepthLeavesArchived(root *convBinding, archived bool) int {
	idx := h.deps.SessionIndex
	if idx == nil {
		return 0
	}
	leaves := h.depthLeavesOf(root)
	if len(leaves) == 0 {
		return 0
	}
	set := idx.ArchivedChatsForAgent(root.agentID, "app")
	n := 0
	for _, l := range leaves {
		if set[l.chatID] == archived {
			continue
		}
		if err := idx.SetArchivedChat(l.agentID, "app", l.chatID, archived); err != nil {
			appLog.Warnf("archive depth leaf %s with root %s (archived=%v): %v", l.convID, root.convID, archived, err)
			continue
		}
		n++
	}
	if n > 0 {
		appLog.Infof("archived=%v %d depth leaf conversation(s) of %s", archived, n, root.convID)
	}
	return n
}

// syncLeafArchived gives leaf its root's archived flag, and reports whether
// it changed.
func (h *Hub) syncLeafArchived(root, leaf *convBinding) bool {
	idx := h.deps.SessionIndex
	if idx == nil {
		return false
	}
	set := idx.ArchivedChatsForAgent(root.agentID, "app")
	want := set[root.chatID]
	if set[leaf.chatID] == want {
		return false
	}
	if err := idx.SetArchivedChat(leaf.agentID, "app", leaf.chatID, want); err != nil {
		appLog.Warnf("archive depth leaf %s with root %s (archived=%v): %v", leaf.convID, root.convID, want, err)
		return false
	}
	return true
}

// attachConnected attaches every connected socket to b, from its current seq,
// as a hello's attachUnresumed would.
func (h *Hub) attachConnected(b *convBinding) {
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		if b.isAttached(c) {
			continue
		}
		b.attach(c)
		b.seedClientAck(c, b.currentSeq())
	}
}

// unregisterDepthLeaf drops branchKey's depth hooks. The conversation stays,
// hidden, with its history.
func (h *Hub) unregisterDepthLeaf(branchKey string) {
	b := h.bindingForSession(branchKey)
	if b == nil {
		return
	}
	b.mu.Lock()
	b.leaf = nil
	b.mu.Unlock()
}

// depthLeafOf returns b's depth hooks, or nil for an ordinary conversation (or
// a leaf whose view is not registered).
func (b *convBinding) depthLeafOf() *leafView {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.leaf
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
	return b != nil && !b.isDepthLeaf()
}

// RegisterLeaf binds branchKey to leaf leafID's own conversation under
// rootKey's (convID; "" = mint one) and returns its id.
func (DepthSurface) RegisterLeaf(rootKey, branchKey, leafID, title, convID string) (string, error) {
	h := depthHub()
	if h == nil {
		return "", errors.New("app provider not running")
	}
	return h.registerDepthLeaf(rootKey, branchKey, leafID, title, convID)
}

// UnregisterLeaf drops branchKey's depth hooks; its conversation stays.
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
	b := h.bindingForSession(rootKey)
	if b == nil || b.isDepthLeaf() {
		return
	}
	if nodes == nil {
		nodes = []fap.DepthNode{}
	}
	b.send(fap.DepthFeed{ConversationID: b.convID, RootState: rootState, Nodes: nodes})
}

// ShowQuestion opens a leaf's conversation with its question to the user as
// the leaf agent's first message (#2617): a durable agent row, the same frame
// an agent reply sends. The leaf's backend fork ends on the same text, so the
// user's reply is the leaf's first turn.
func (DepthSurface) ShowQuestion(branchKey, text string) {
	h := depthHub()
	if h == nil {
		return
	}
	if b := h.bindingForSession(branchKey); b != nil {
		b.send(fap.ServerMessage{ConversationID: b.convID, MessageID: fap.NewULID(), Role: "agent", Text: text})
	}
}

// LeafBusy reports whether the leaf on branchKey has a turn running.
func (DepthSurface) LeafBusy(branchKey string) bool {
	h := depthHub()
	if h == nil {
		return false
	}
	b := h.bindingForSession(branchKey)
	if b == nil {
		return false
	}
	l := b.depthLeafOf()
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.busy
}

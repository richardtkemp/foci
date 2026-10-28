package tools

// Depth mode (#2588): a conversation fans out into leaf branches the user
// talks to one by one, one leaf at a time inside the starting (root) app conversation.
//
// A node is a forked branch session (branch type "depth") whose app output a
// leaf view routes into the root conversation, tagged with the node's leaf id
// (internal/app/depth.go). A node reports ONCE (depth_report); when every
// child of a fan-out has reported, been dismissed or lost, the parent gets ONE
// injected [DEPTH RESULTS] turn and carries on. The tree lives as small
// records in agent_metadata (key depth:<session key>), so it survives
// restarts; DepthManager.Restore rebuilds the leaf views and finishes what the
// last process could not.
//
// Design: /home/foci/clutch/notes/2588-design.md (§2 model, §3 pieces).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"foci/internal/fap"
	"foci/internal/log"
	"foci/internal/session"
)

var depthLog = log.NewComponentLogger("depth")

// depthMetaPrefix heads the agent_metadata key of each persisted depth node.
const depthMetaPrefix = "depth:"

// depthApprovalLevel is the deepest level a node may fan out from without the
// user's approval (ruling 14:46): a fan-out from a node at this level or
// deeper waits for Approve / Deny.
const depthApprovalLevel = 5

// depthRetention is how long a finished tree's records are kept: a tree whose
// nodes are all done and untouched for longer is deleted at startup (its
// reports stay in the root's transcript).
const depthRetention = 30 * 24 * time.Hour

// depthCostTTL bounds how often one node's spend is re-read from the ledger
// for the feed, which is re-sent on every leaf message.
const depthCostTTL = 30 * time.Second

// Node states: the persisted ones of fap.DepthState*.
const (
	depthActive    = fap.DepthStateActive
	depthFanned    = fap.DepthStateFanned
	depthReported  = fap.DepthStateReported
	depthDismissed = fap.DepthStateDismissed
	depthSide      = fap.DepthStateSide
	depthLost      = fap.DepthStateLost
	depthDropped   = fap.DepthStateDropped
)

// DepthAckText is the turn the feed's Ack runs on a leaf (NQ1: Ack = report now).
const DepthAckText = "[DEPTH ACK] The user has acknowledged your last message and has nothing to add. Report now with foci_depth_report, using what you have; do not ask anything further."

// DepthSurface is the app side of depth mode (app.DepthSurface).
type DepthSurface interface {
	// RootBound reports whether rootKey has an app conversation of its own.
	RootBound(rootKey string) bool
	// RegisterLeaf routes branchKey's app output into rootKey's conversation
	// as leaf leafID; UnregisterLeaf undoes it.
	RegisterLeaf(rootKey, branchKey, leafID, title string) error
	UnregisterLeaf(branchKey string)
	// PushFeed sends the whole tree to rootKey's conversation.
	PushFeed(rootKey, rootState string, nodes []fap.DepthNode)
	// ShowPrompt opens a leaf's exchange with its prompt, from fromSession.
	ShowPrompt(branchKey, text, fromSession string)
	// LeafBusy reports whether the leaf on branchKey has a turn running.
	LeafBusy(branchKey string) bool
}

// DepthDeps is what a DepthManager needs from the gateway.
type DepthDeps struct {
	AgentID string
	// Enabled is the live [depth] enabled switch (#2603); nil = always on.
	// While it is off the tools refuse, a parked approval answered Approve is
	// refused, and Restore waits for OnEnable.
	Enabled func() bool
	Store   SpawnStore
	Surface DepthSurface
	// Sessions forks the caller into each leaf (ForkSession).
	Sessions SessionBrancher
	// CanBranch reports whether the agent's backend can fork a conversation.
	CanBranch func() bool
	// SetNoCompact marks a node's branch no_compact (§3.9).
	SetNoCompact func(sessionKey string, value bool)
	// OrientationTemplate is the resolved leaf orientation; its {report_rule}
	// takes each leaf's own orientation text.
	OrientationTemplate string
	// RunTurn queues text as an injected turn on sessionKey whose output goes
	// to that session's app binding (a leaf's goes to its item). started runs
	// when the turn begins; done(true) once it ran, done(false) when the inbox
	// refused it (shutdown, full inbox).
	RunTurn func(sessionKey, text, trigger string, started func(), done func(ran bool))
	// Notifier delivers the [DEPTH RESULTS] fan-in durably (InjectToAgentThen).
	Notifier *AsyncNotifier
	// MaxConcurrent bounds how many leaves run their FIRST turn at once (the
	// max_concurrent_spawns limit), so a wide fan-out does not launch every
	// backend in the same second. 0 = no bound.
	MaxConcurrent int
	// CancelAsk cancels the session's pending foci_ask, if any (Ack).
	CancelAsk func(sessionKey string)
	// StopTurn stops the session's running turn, if any (Dismiss, reset).
	StopTurn func(sessionKey string)
	// CostUSD returns a session's spend so far; ok=false when unknown.
	CostUSD func(sessionKey string) (usd float64, ok bool)
	// ContextTokens returns a session's current context size (0 = unknown).
	ContextTokens func(sessionKey string) int
	// CanResume reports whether a branch still has a conversation to continue.
	CanResume func(branchKey string) bool
	// Approve asks the user, in sessionKey's chat (a leaf's item), to approve
	// a fan-out; cb runs once with the answer. Nil = approval impossible: deep
	// fan-outs are refused.
	Approve func(sessionKey, promptID, text string, cb func(approved bool)) error
	// Now is the clock; nil = time.Now.
	Now func() time.Time
}

// depthLeafSpec is one leaf a fan-out asks for.
type depthLeafSpec struct {
	Title       string `json:"title"`
	Prompt      string `json:"prompt"`
	Orientation string `json:"orientation,omitempty"`
}

// depthApproval is a fan-out waiting for the user's approval (level > 5).
type depthApproval struct {
	PromptID    string          `json:"prompt_id"`
	Leaves      []depthLeafSpec `json:"leaves"`
	Orientation string          `json:"orientation,omitempty"`
}

// depthClaim is one entry of a tree's claims board (ruling 3).
type depthClaim struct {
	Node string    `json:"node"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// depthNode is the persisted record of one node. The root (the chat that first
// fanned out) is a node too, with no LeafID and Level 0.
type depthNode struct {
	Key         string    `json:"key"`               // the node's session key
	LeafID      string    `json:"leaf_id,omitempty"` // "" for the root
	Tree        string    `json:"tree"`              // the root's session key
	Parent      string    `json:"parent,omitempty"`  // the parent node's session key
	Level       int       `json:"level"`
	Title       string    `json:"title,omitempty"`
	Prompt      string    `json:"prompt,omitempty"`
	Orientation string    `json:"orientation,omitempty"`
	State       string    `json:"state"`
	Report      string    `json:"report,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	ReportedAt  time.Time `json:"reported_at,omitempty"`
	// Reason says why a node is lost or dropped.
	Reason string `json:"reason,omitempty"`

	// FirstTurnStarted: the leaf's first turn began running. A restart re-runs
	// a first turn that never started (its input never reached the backend).
	FirstTurnStarted bool `json:"first_turn_started,omitempty"`
	// AckPending: the user acknowledged; the report has not landed yet.
	AckPending      bool      `json:"ack_pending,omitempty"`
	AckAt           time.Time `json:"ack_at,omitempty"`
	AckTurnStarted  bool      `json:"ack_turn_started,omitempty"`
	LastLine        string    `json:"last_line,omitempty"`
	LastLineAt      time.Time `json:"last_line_at,omitempty"`
	ContextAtFanout int       `json:"context_at_fanout,omitempty"`

	// Children are the session keys of the node's CURRENT fan-out.
	Children []string `json:"children,omitempty"`
	// Results is a [DEPTH RESULTS] message the node has not taken yet; a
	// restart delivers it.
	Results  string         `json:"results,omitempty"`
	Approval *depthApproval `json:"approval,omitempty"`
	// Claims is the tree's claims board (root only).
	Claims []depthClaim `json:"claims,omitempty"`
}

// done reports whether a node counts as finished for its parent's fan-in.
func (n *depthNode) done() bool {
	switch n.State {
	case depthReported, depthDismissed, depthLost, depthDropped, depthSide:
		return true
	}
	return false
}

// DepthManager owns one agent's depth-mode trees.
type DepthManager struct {
	deps DepthDeps
	sem  chan struct{}

	mu       sync.Mutex
	nodes    map[string]*depthNode // session key → node
	queued   map[string]int        // session key → turns queued by the manager, not yet ended
	costs    map[string]depthCost  // session key → cached spend
	restored bool
	// restoreDue is set by the startup Restore call: from then on, turning
	// depth mode on (OnEnable) runs the restore it skipped while off.
	restoreDue bool
}

type depthCost struct {
	text string
	at   time.Time
}

// NewDepthManager builds an agent's depth manager and registers it for the app
// hub's callbacks (DepthDispatch).
func NewDepthManager(deps DepthDeps) *DepthManager {
	m := &DepthManager{
		deps:   deps,
		nodes:  make(map[string]*depthNode),
		queued: make(map[string]int),
		costs:  make(map[string]depthCost),
	}
	if deps.MaxConcurrent > 0 {
		m.sem = make(chan struct{}, deps.MaxConcurrent)
	}
	registerDepthManager(deps.AgentID, m)
	return m
}

// Enabled reports the live [depth] enabled switch.
func (m *DepthManager) Enabled() bool {
	return m.deps.Enabled == nil || m.deps.Enabled()
}

// OnEnable is called when [depth] enabled turns on (#2603): it restores the
// persisted trees a startup Restore skipped while depth mode was off. Before
// that startup point (the platforms may not be up) and after a restore it
// does nothing.
func (m *DepthManager) OnEnable() {
	m.mu.Lock()
	due := m.restoreDue
	m.mu.Unlock()
	if due {
		m.restore()
	}
}

func (m *DepthManager) now() time.Time {
	if m.deps.Now != nil {
		return m.deps.Now()
	}
	return time.Now()
}

func newDepthID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// --- persistence ---

// persist writes the given nodes' records (snapshots taken under m.mu).
func (m *DepthManager) persist(recs ...depthNode) {
	if m.deps.Store == nil {
		return
	}
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			depthLog.Errorf("encode node %s: %v", r.Key, err)
			continue
		}
		if err := m.deps.Store.SetAgentMetadata(m.deps.AgentID, depthMetaPrefix+r.Key, string(b)); err != nil {
			depthLog.Errorf("persist node %s: %v", r.Key, err)
		}
	}
}

func (m *DepthManager) forget(keys ...string) {
	if m.deps.Store == nil {
		return
	}
	for _, k := range keys {
		if err := m.deps.Store.DeleteAgentMetadata(m.deps.AgentID, depthMetaPrefix+k); err != nil {
			depthLog.Warnf("delete node %s: %v", k, err)
		}
	}
}

// snap copies nodes under m.mu for persist.
func snap(ns ...*depthNode) []depthNode {
	out := make([]depthNode, 0, len(ns))
	for _, n := range ns {
		if n != nil {
			c := *n
			c.Children = append([]string(nil), n.Children...)
			c.Claims = append([]depthClaim(nil), n.Claims...)
			out = append(out, c)
		}
	}
	return out
}

// --- tree queries (m.mu held) ---

func (m *DepthManager) treeNodesLocked(tree string) []*depthNode {
	var out []*depthNode
	for _, n := range m.nodes {
		if n.Tree == tree {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func (m *DepthManager) leafLocked(tree, leafID string) *depthNode {
	for _, n := range m.nodes {
		if n.Tree == tree && n.LeafID == leafID && leafID != "" {
			return n
		}
	}
	return nil
}

// --- feed ---

// pushFeed sends tree's whole state to its root conversation.
func (m *DepthManager) pushFeed(tree string) {
	if m.deps.Surface == nil || tree == "" {
		return
	}
	m.mu.Lock()
	ns := m.treeNodesLocked(tree)
	root := m.nodes[tree]
	type row struct {
		n      depthNode
		parent string
		queued bool
	}
	rows := make([]row, 0, len(ns))
	for _, n := range ns {
		if n.LeafID == "" {
			continue
		}
		var parentLeaf string
		if p := m.nodes[n.Parent]; p != nil {
			parentLeaf = p.LeafID
		}
		rows = append(rows, row{n: *n, parent: parentLeaf, queued: m.queued[n.Key] > 0})
	}
	rootState := depthActive
	if root != nil {
		rootState = root.State
	}
	m.mu.Unlock()

	nodes := make([]fap.DepthNode, 0, len(rows))
	for _, r := range rows {
		n := r.n
		dn := fap.DepthNode{
			LeafID:       n.LeafID,
			ParentLeafID: r.parent,
			Title:        n.Title,
			Level:        n.Level,
			State:        n.State,
			AckPending:   n.AckPending && n.State == depthActive,
			LastLine:     n.LastLine,
			CostText:     m.costText(n.Key),
			UpdatedMs:    n.Updated.UnixMilli(),
		}
		if n.State == depthActive && !n.AckPending && !r.queued && !m.deps.Surface.LeafBusy(n.Key) {
			dn.WaitingForUser = true
		}
		if n.done() {
			dn.ReportPreview = depthPreview(n.reportLine(), 240)
		}
		nodes = append(nodes, dn)
	}
	m.deps.Surface.PushFeed(tree, rootState, nodes)
}

// reportLine is what a done node handed (or failed to hand) its parent.
func (n *depthNode) reportLine() string {
	switch n.State {
	case depthDismissed:
		return "(dismissed by the user)"
	case depthLost:
		return "(lost: " + n.Reason + ")"
	case depthDropped:
		return "(dropped by reset)"
	}
	return n.Report
}

func depthPreview(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return string(r)
}

// costText is the node's spend, e.g. "$0.31", or "" when unknown.
func (m *DepthManager) costText(key string) string {
	if m.deps.CostUSD == nil {
		return ""
	}
	now := m.now()
	m.mu.Lock()
	c, ok := m.costs[key]
	m.mu.Unlock()
	if ok && now.Sub(c.at) < depthCostTTL {
		return c.text
	}
	text := ""
	if usd, ok := m.deps.CostUSD(key); ok {
		text = fmt.Sprintf("$%.2f", usd)
	}
	m.mu.Lock()
	m.costs[key] = depthCost{text: text, at: now}
	m.mu.Unlock()
	return text
}

// --- fan-out ---

// fanout implements depth_fanout for caller.
func (m *DepthManager) fanout(ctx context.Context, caller string, leaves []depthLeafSpec, orientation string) (string, error) {
	if caller == "" {
		return "", fmt.Errorf("depth_fanout: no session in context")
	}
	if len(leaves) == 0 {
		return "", fmt.Errorf("depth_fanout: give at least one leaf")
	}
	for i, l := range leaves {
		if strings.TrimSpace(l.Title) == "" || strings.TrimSpace(l.Prompt) == "" {
			return "", fmt.Errorf("depth_fanout: leaf %d needs a title and a prompt", i+1)
		}
	}
	if m.deps.CanBranch != nil && !m.deps.CanBranch() {
		return "", fmt.Errorf("depth_fanout: this agent's backend cannot fork a conversation, which depth mode needs")
	}

	m.mu.Lock()
	n := m.nodes[caller]
	m.mu.Unlock()
	if n == nil {
		if m.deps.Surface == nil || !m.deps.Surface.RootBound(caller) {
			return "", fmt.Errorf("depth_fanout: depth mode starts only from an app conversation (this session has none); fan-out from Telegram, Discord or a headless session is not supported")
		}
	} else {
		switch n.State {
		case depthActive:
		case depthFanned:
			return "", fmt.Errorf("depth_fanout: you are already waiting on leaves; end your turn and wait for [DEPTH RESULTS]")
		default:
			return "", fmt.Errorf("depth_fanout: this thread is outside the tree (%s); it cannot fan out", n.State)
		}
		if n.Approval != nil {
			return "", fmt.Errorf("depth_fanout: a fan-out of yours is already waiting for the user's approval; end your turn")
		}
	}

	level := 0
	if n != nil {
		level = n.Level
	}
	if n != nil && n.LeafID != "" && level >= depthApprovalLevel {
		return m.requestApproval(caller, leaves, orientation)
	}
	return m.doFanout(ctx, caller, leaves, orientation)
}

// requestApproval parks a fan-out from a level-5+ node until the user answers.
func (m *DepthManager) requestApproval(caller string, leaves []depthLeafSpec, orientation string) (string, error) {
	if m.deps.Approve == nil {
		return "", fmt.Errorf("depth_fanout: the tree is %d levels deep and the user cannot be asked to approve more here", depthApprovalLevel)
	}
	ap := &depthApproval{PromptID: newDepthID("depthok-"), Leaves: leaves, Orientation: orientation}
	m.mu.Lock()
	n := m.nodes[caller]
	if n == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("depth_fanout: node gone")
	}
	n.Approval = ap
	n.Updated = m.now()
	rec := snap(n)
	m.mu.Unlock()
	m.persist(rec...)
	if err := m.presentApproval(caller, ap); err != nil {
		m.mu.Lock()
		if n := m.nodes[caller]; n != nil {
			n.Approval = nil
			rec = snap(n)
		}
		m.mu.Unlock()
		m.persist(rec...)
		return "", fmt.Errorf("depth_fanout: could not ask the user to approve: %w", err)
	}
	return fmt.Sprintf("Your fan-out into %d leaves is pending the user's approval (the tree is past %d levels). End your turn; you will be told the answer.", len(leaves), depthApprovalLevel), nil
}

func (m *DepthManager) presentApproval(caller string, ap *depthApproval) error {
	m.mu.Lock()
	n := m.nodes[caller]
	if n == nil {
		m.mu.Unlock()
		return fmt.Errorf("node gone")
	}
	ns := m.treeNodesLocked(n.Tree)
	leaves, maxLevel := 0, 0
	for _, x := range ns {
		if x.LeafID != "" {
			leaves++
		}
		if x.Level > maxLevel {
			maxLevel = x.Level
		}
	}
	m.mu.Unlock()
	text := fmt.Sprintf("This leaf wants to fan out again into %d leaves. The tree already has %d leaves across %d levels. Approve or deny?", len(ap.Leaves), leaves, maxLevel)
	return m.deps.Approve(caller, ap.PromptID, text, func(ok bool) { m.resolveApproval(caller, ap.PromptID, ok) })
}

// resolveApproval runs or drops a parked fan-out on the user's answer, and
// tells the requester.
func (m *DepthManager) resolveApproval(caller, promptID string, approved bool) {
	m.mu.Lock()
	n := m.nodes[caller]
	if n == nil || n.Approval == nil || n.Approval.PromptID != promptID {
		m.mu.Unlock()
		return
	}
	ap := *n.Approval
	n.Approval = nil
	n.Updated = m.now()
	rec := snap(n)
	m.mu.Unlock()
	m.persist(rec...)
	if approved && !m.Enabled() {
		m.runTurn(caller, "[DEPTH] The user approved your fan-out, but depth mode has since been turned off, so it did not run; you stay a leaf. Carry on with your point.")
		return
	}
	if !approved {
		m.runTurn(caller, "[DEPTH] Fan-out denied by the user; you stay a leaf. Carry on with your point.")
		return
	}
	msg, err := m.doFanout(context.Background(), caller, ap.Leaves, ap.Orientation)
	if err != nil {
		m.runTurn(caller, "[DEPTH] The user approved your fan-out, but it failed: "+err.Error())
		return
	}
	m.runTurn(caller, "[DEPTH] The user approved your fan-out. "+msg)
}

// doFanout forks caller into one branch per leaf and starts each leaf.
func (m *DepthManager) doFanout(ctx context.Context, caller string, leaves []depthLeafSpec, orientation string) (string, error) {
	if m.deps.Sessions == nil {
		return "", fmt.Errorf("depth_fanout: no session brancher")
	}
	// Fork every leaf first, so a failed fork leaves the caller untouched.
	type forked struct {
		spec depthLeafSpec
		key  string
	}
	var made []forked
	for _, l := range leaves {
		orient := l.Orientation
		if orient == "" {
			orient = orientation
		}
		tmpl := strings.ReplaceAll(m.deps.OrientationTemplate, session.ReportRuleVar, orient)
		bk, ok, err := m.deps.Sessions.ForkSession(ctx, caller, BranchOptions{
			NoResetHook:         true,
			BranchType:          "depth",
			OrientationTemplate: tmpl,
		})
		if err != nil {
			return "", fmt.Errorf("depth_fanout: fork for %q: %w", l.Title, err)
		}
		if !ok {
			return "", fmt.Errorf("depth_fanout: the backend could not fork this session (has it started yet?)")
		}
		made = append(made, forked{spec: l, key: bk})
	}
	ctxTokens := 0
	if m.deps.ContextTokens != nil {
		ctxTokens = m.deps.ContextTokens(caller)
	}

	now := m.now()
	m.mu.Lock()
	parent := m.nodes[caller]
	if parent == nil {
		// The caller's first fan-out makes it a tree root.
		parent = &depthNode{Key: caller, Tree: caller, State: depthActive, Created: now}
		m.nodes[caller] = parent
	}
	if parent.State != depthActive {
		m.mu.Unlock()
		return "", fmt.Errorf("depth_fanout: you are no longer able to fan out (%s)", parent.State)
	}
	tree := parent.Tree
	var children []*depthNode
	for i, f := range made {
		c := &depthNode{
			Key: f.key, LeafID: newDepthID("leaf-"), Tree: tree, Parent: caller,
			Level: parent.Level + 1, Title: strings.TrimSpace(f.spec.Title), Prompt: f.spec.Prompt,
			Orientation: f.spec.Orientation, State: depthActive,
			// Distinct creation stamps keep the feed in the order asked for.
			Created: now.Add(time.Duration(i) * time.Microsecond), Updated: now,
		}
		m.nodes[f.key] = c
		m.queued[f.key]++
		children = append(children, c)
	}
	parent.State = depthFanned
	parent.Children = parent.Children[:0]
	for _, c := range children {
		parent.Children = append(parent.Children, c.Key)
	}
	parent.Results = ""
	parent.ContextAtFanout = ctxTokens
	parent.Updated = now
	recs := snap(append([]*depthNode{parent}, children...)...)
	m.mu.Unlock()
	m.persist(recs...)

	for _, c := range children {
		if m.deps.SetNoCompact != nil {
			m.deps.SetNoCompact(c.Key, true)
		}
		if m.deps.Surface != nil {
			if err := m.deps.Surface.RegisterLeaf(tree, c.Key, c.LeafID, c.Title); err != nil {
				depthLog.Warnf("register leaf %s of %s: %v", c.Key, tree, err)
			}
			m.deps.Surface.ShowPrompt(c.Key, c.Prompt, caller)
		}
	}
	m.pushFeed(tree)
	for _, c := range children {
		m.startFirstTurn(c.Key, c.Title, c.Prompt, true)
	}
	depthLog.Infof("fan-out %s → %d leaves (tree %s, level %d)", caller, len(children), tree, children[0].Level)

	var b strings.Builder
	fmt.Fprintf(&b, "You are now waiting on %d leaves; end your turn. You will get one [DEPTH RESULTS] message when all have reported.\n", len(children))
	for _, c := range children {
		fmt.Fprintf(&b, "- %s (%s)\n", c.Title, c.LeafID)
	}
	if ctxTokens > 0 {
		fmt.Fprintf(&b, "Each leaf started from a copy of your context: %d tokens.", ctxTokens)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// startFirstTurn runs a leaf's first turn under the first-turn bound. counted
// says the caller already counted it in m.queued.
func (m *DepthManager) startFirstTurn(key, title, prompt string, counted bool) {
	if !counted {
		m.mu.Lock()
		m.queued[key]++
		m.mu.Unlock()
	}
	text := "[DEPTH LEAF: " + title + "]\n" + prompt
	go func() {
		if m.sem != nil {
			m.sem <- struct{}{}
		}
		release := func() {
			if m.sem != nil {
				<-m.sem
			}
		}
		if m.deps.RunTurn == nil {
			release()
			m.turnEnded(key)
			return
		}
		m.deps.RunTurn(key, text, "depth", func() { m.markFirstTurnStarted(key) }, func(bool) {
			release()
			m.turnEnded(key)
		})
	}()
}

func (m *DepthManager) markFirstTurnStarted(key string) {
	m.mu.Lock()
	n := m.nodes[key]
	if n == nil || n.FirstTurnStarted {
		m.mu.Unlock()
		return
	}
	n.FirstTurnStarted = true
	rec := snap(n)
	m.mu.Unlock()
	m.persist(rec...)
}

// runTurn queues a manager-originated turn on key (trigger "depth"), counted
// as running for the feed until it ends.
func (m *DepthManager) runTurn(key, text string) {
	if m.deps.RunTurn == nil {
		return
	}
	m.mu.Lock()
	m.queued[key]++
	m.mu.Unlock()
	m.deps.RunTurn(key, text, "depth", nil, func(bool) { m.turnEnded(key) })
}

func (m *DepthManager) turnEnded(key string) {
	m.mu.Lock()
	if m.queued[key] > 0 {
		m.queued[key]--
		if m.queued[key] == 0 {
			delete(m.queued, key)
		}
	}
	n := m.nodes[key]
	tree := ""
	if n != nil {
		tree = n.Tree
	}
	m.mu.Unlock()
	m.pushFeed(tree)
}

// --- report and fan-in ---

func (m *DepthManager) report(caller, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("depth_report: the report is empty")
	}
	m.mu.Lock()
	n := m.nodes[caller]
	if n == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("depth_report: this session is not a depth-mode leaf")
	}
	if n.LeafID == "" {
		m.mu.Unlock()
		return "", fmt.Errorf("depth_report: you are the root; answer the user")
	}
	switch n.State {
	case depthActive:
	case depthFanned:
		m.mu.Unlock()
		return "", fmt.Errorf("depth_report: you have open leaves; wait for [DEPTH RESULTS] first")
	default:
		m.mu.Unlock()
		return "", fmt.Errorf("depth_report: already reported; this thread is outside the tree")
	}
	m.finishLocked(n, depthReported, text)
	rec := snap(n)
	parent, tree := n.Parent, n.Tree
	m.mu.Unlock()
	m.persist(rec...)
	m.checkFanIn(parent)
	m.pushFeed(tree)
	return "Reported. This thread is now outside the tree; end your turn.", nil
}

// finishLocked moves n to a done state. Caller holds m.mu.
func (m *DepthManager) finishLocked(n *depthNode, state, report string) {
	now := m.now()
	n.State = state
	if report != "" {
		n.Report = report
	}
	n.AckPending = false
	n.ReportedAt = now
	n.Updated = now
}

// checkFanIn delivers [DEPTH RESULTS] to parent once every child of its
// current fan-out is done.
func (m *DepthManager) checkFanIn(parentKey string) {
	if parentKey == "" {
		return
	}
	m.mu.Lock()
	p := m.nodes[parentKey]
	if p == nil || p.State != depthFanned || len(p.Children) == 0 {
		m.mu.Unlock()
		return
	}
	var kids []depthNode
	for _, k := range p.Children {
		c := m.nodes[k]
		if c == nil {
			continue
		}
		if !c.done() {
			m.mu.Unlock()
			return
		}
		kids = append(kids, *c)
	}
	p.State = depthActive
	p.Updated = m.now()
	m.mu.Unlock()

	msg := m.resultsMessage(kids)
	m.mu.Lock()
	p.Results = msg
	rec := snap(p)
	tree := p.Tree
	m.mu.Unlock()
	m.persist(rec...)
	m.pushFeed(tree)
	m.deliverResults(parentKey, msg)
}

// resultsMessage builds the one [DEPTH RESULTS] turn of a fan-out.
func (m *DepthManager) resultsMessage(kids []depthNode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[DEPTH RESULTS] All %d leaves of your fan-out have reported.\n", len(kids))
	for _, c := range kids {
		b.WriteString("\n## ")
		b.WriteString(c.Title)
		switch c.State {
		case depthDismissed:
			b.WriteString(" — dismissed by the user\n")
			continue
		case depthLost:
			b.WriteString(" — lost: " + c.Reason + "\n")
			continue
		case depthDropped:
			b.WriteString(" — dropped by reset\n")
			continue
		}
		meta := []string{fmt.Sprintf("level %d", c.Level)}
		if !c.ReportedAt.IsZero() {
			meta = append(meta, "reported "+c.ReportedAt.Local().Format("15:04"))
		}
		if cost := m.costText(c.Key); cost != "" {
			meta = append(meta, cost)
		}
		fmt.Fprintf(&b, " (%s)\n%s\n", strings.Join(meta, ", "), c.Report)
	}
	return strings.TrimRight(b.String(), "\n")
}

// deliverResults injects msg into parentKey; the record keeps it until a turn
// took it, so a refused delivery is retried by the next process.
func (m *DepthManager) deliverResults(parentKey, msg string) {
	if m.deps.Notifier == nil {
		return
	}
	m.deps.Notifier.InjectToAgentThen(parentKey, msg, "depth_results", func(delivered bool) {
		if !delivered {
			depthLog.Infof("[DEPTH RESULTS] for %s not taken (shutting down?) — left for the next process", parentKey)
			return
		}
		m.mu.Lock()
		p := m.nodes[parentKey]
		if p == nil || p.Results != msg {
			m.mu.Unlock()
			return
		}
		p.Results = ""
		rec := snap(p)
		m.mu.Unlock()
		m.persist(rec...)
	})
}

// --- hub callbacks ---

// LeafUserMessage: a user message to a done leaf reopens it as a side thread
// (ruling 8): nothing from it reaches the parent.
func (m *DepthManager) LeafUserMessage(key string) {
	m.mu.Lock()
	n := m.nodes[key]
	if n == nil || (n.State != depthReported && n.State != depthDismissed) {
		m.mu.Unlock()
		return
	}
	n.State = depthSide
	n.Updated = m.now()
	rec := snap(n)
	tree := n.Tree
	m.mu.Unlock()
	m.persist(rec...)
	m.pushFeed(tree)
}

// LeafText records the leaf's latest message for its feed item.
func (m *DepthManager) LeafText(key, text string) {
	m.mu.Lock()
	n := m.nodes[key]
	if n == nil {
		m.mu.Unlock()
		return
	}
	now := m.now()
	n.LastLine = text
	n.LastLineAt = now
	n.Updated = now
	rec := snap(n)
	tree := n.Tree
	m.mu.Unlock()
	m.persist(rec...)
	m.pushFeed(tree)
}

// LeafBusy re-sends the feed when a leaf's turn starts or ends.
func (m *DepthManager) LeafBusy(key string, _ bool) {
	m.mu.Lock()
	n := m.nodes[key]
	tree := ""
	if n != nil {
		tree = n.Tree
	}
	m.mu.Unlock()
	m.pushFeed(tree)
}

// Ack is the feed's Ack (NQ1): cancel the leaf's pending ask and run one turn
// telling it to report now. If that turn ends without a report, its final
// text becomes the report, so one tap always closes the leaf.
func (m *DepthManager) Ack(rootKey, leafID string) error {
	m.mu.Lock()
	n := m.leafLocked(rootKey, leafID)
	if n == nil {
		m.mu.Unlock()
		return fmt.Errorf("no leaf %s in this conversation", leafID)
	}
	tree := n.Tree
	switch {
	case n.State == depthFanned:
		m.mu.Unlock()
		return fmt.Errorf("this leaf has open leaves of its own; it reports once they have")
	case n.State != depthActive || n.AckPending:
		m.mu.Unlock()
		m.pushFeed(tree) // idempotent: answer with the current feed
		return nil
	}
	n.AckPending = true
	n.AckAt = m.now()
	n.AckTurnStarted = false
	n.Updated = n.AckAt
	rec := snap(n)
	key := n.Key
	m.mu.Unlock()
	m.persist(rec...)
	if m.deps.CancelAsk != nil {
		m.deps.CancelAsk(key)
	}
	m.pushFeed(tree)
	m.runAck(key)
	return nil
}

func (m *DepthManager) runAck(key string) {
	if m.deps.RunTurn == nil {
		return
	}
	m.mu.Lock()
	m.queued[key]++
	m.mu.Unlock()
	m.deps.RunTurn(key, DepthAckText, "depth_ack", func() {
		m.mu.Lock()
		n := m.nodes[key]
		var rec []depthNode
		if n != nil && n.AckPending {
			n.AckTurnStarted = true
			rec = snap(n)
		}
		m.mu.Unlock()
		m.persist(rec...)
	}, func(ran bool) {
		if ran {
			m.autoReport(key)
		}
		m.turnEnded(key)
	})
}

// autoReport closes an acknowledged leaf whose ack turn ended without a
// report, with its latest message since the ack.
func (m *DepthManager) autoReport(key string) {
	m.mu.Lock()
	n := m.nodes[key]
	if n == nil || n.State != depthActive || !n.AckPending {
		m.mu.Unlock()
		return
	}
	text := "(acknowledged by the user; the leaf gave no summary)"
	if strings.TrimSpace(n.LastLine) != "" && !n.LastLineAt.Before(n.AckAt) {
		text = n.LastLine + "\n\n(auto-reported after the user's acknowledgement)"
	}
	m.finishLocked(n, depthReported, text)
	rec := snap(n)
	parent, tree := n.Parent, n.Tree
	m.mu.Unlock()
	m.persist(rec...)
	m.checkFanIn(parent)
	m.pushFeed(tree)
}

// Dismiss is the feed's Dismiss (Q3): the node ends with no report, after its
// unreported descendants. It counts as reported for its parent.
func (m *DepthManager) Dismiss(rootKey, leafID string) error {
	m.mu.Lock()
	n := m.leafLocked(rootKey, leafID)
	if n == nil {
		m.mu.Unlock()
		return fmt.Errorf("no leaf %s in this conversation", leafID)
	}
	tree, parent := n.Tree, n.Parent
	if n.done() {
		m.mu.Unlock()
		m.pushFeed(tree)
		return nil
	}
	var stop []string
	var recs []*depthNode
	var walk func(x *depthNode)
	walk = func(x *depthNode) {
		for _, k := range x.Children {
			if c := m.nodes[k]; c != nil && !c.done() {
				walk(c)
			}
		}
		m.finishLocked(x, depthDismissed, "")
		x.Approval = nil
		stop = append(stop, x.Key)
		recs = append(recs, x)
	}
	walk(n)
	rec := snap(recs...)
	m.mu.Unlock()
	m.persist(rec...)
	for _, k := range stop {
		if m.deps.StopTurn != nil {
			m.deps.StopTurn(k)
		}
		if m.deps.CancelAsk != nil {
			m.deps.CancelAsk(k)
		}
	}
	m.checkFanIn(parent)
	m.pushFeed(tree)
	return nil
}

// OnReset drops the whole tree of a root being reset (Dick: a reset is
// permitted and ends every node, with no fan-in). A reset of a session that is
// not a tree root does nothing here.
func (m *DepthManager) OnReset(rootKey string) {
	m.mu.Lock()
	root := m.nodes[rootKey]
	if root == nil || root.LeafID != "" {
		m.mu.Unlock()
		return
	}
	var recs []*depthNode
	var open, all []string
	for _, n := range m.treeNodesLocked(rootKey) {
		if n.LeafID == "" {
			continue
		}
		all = append(all, n.Key)
		if !n.done() || n.State == depthSide {
			if n.State != depthSide {
				open = append(open, n.Key)
			}
			m.finishLocked(n, depthDropped, "")
			n.Reason = "the root was reset"
			n.Approval = nil
			recs = append(recs, n)
		}
	}
	root.State = depthActive
	root.Children = nil
	root.Results = ""
	root.Updated = m.now()
	recs = append(recs, root)
	rec := snap(recs...)
	m.mu.Unlock()
	m.persist(rec...)
	for _, k := range open {
		if m.deps.StopTurn != nil {
			m.deps.StopTurn(k)
		}
		if m.deps.CancelAsk != nil {
			m.deps.CancelAsk(k)
		}
	}
	m.pushFeed(rootKey)
	if m.deps.Surface != nil {
		for _, k := range all {
			m.deps.Surface.UnregisterLeaf(k)
		}
	}
	if len(open) > 0 {
		depthLog.Infof("reset of %s dropped %d open node(s) of its tree", rootKey, len(open))
	}
}

// --- restart ---

// Restore loads the agent's trees after a restart: it rebuilds the leaf
// views, marks nodes whose branch cannot resume lost, re-runs first turns and
// acks the last process never started, re-presents pending approvals,
// delivers [DEPTH RESULTS] it could not, and deletes trees finished long ago.
// Call once, after the app platform is up. While depth mode is off it only
// marks the startup point, and OnEnable restores once it is turned on.
func (m *DepthManager) Restore() {
	m.mu.Lock()
	m.restoreDue = true
	m.mu.Unlock()
	if m.Enabled() {
		m.restore()
	}
}

// restore is Restore's work, run once.
func (m *DepthManager) restore() {
	if m.deps.Store == nil {
		return
	}
	rows, err := m.deps.Store.AgentMetadataByPrefix(m.deps.AgentID, depthMetaPrefix)
	if err != nil {
		depthLog.Errorf("load depth nodes: %v", err)
		return
	}
	m.mu.Lock()
	if m.restored {
		m.mu.Unlock()
		return
	}
	m.restored = true
	for _, v := range rows {
		var n depthNode
		if err := json.Unmarshal([]byte(v), &n); err != nil || n.Key == "" {
			depthLog.Warnf("skip unreadable depth node record: %v", err)
			continue
		}
		m.nodes[n.Key] = &n
	}
	trees := map[string]bool{}
	for _, n := range m.nodes {
		trees[n.Tree] = true
	}
	now := m.now()
	var expired []string
	for tree := range trees {
		ns := m.treeNodesLocked(tree)
		allDone, newest := true, time.Time{}
		for _, n := range ns {
			if n.LeafID != "" && !n.done() {
				allDone = false
			}
			if n.LeafID == "" && (n.State != depthActive || n.Results != "") {
				allDone = false
			}
			if n.Updated.After(newest) {
				newest = n.Updated
			}
		}
		if allDone && now.Sub(newest) > depthRetention {
			for _, n := range ns {
				expired = append(expired, n.Key)
				delete(m.nodes, n.Key)
			}
			delete(trees, tree)
		}
	}
	type firstTurn struct{ key, title, prompt string }
	var (
		register     []*depthNode
		firsts       []firstTurn
		acks         []string
		approvals    []string
		results      []string
		fanInParents []string
		recs         []*depthNode
	)
	for _, n := range m.nodes {
		if n.LeafID != "" && n.State != depthDropped {
			register = append(register, n)
		}
		live := n.State == depthActive || n.State == depthFanned
		if n.LeafID != "" && live && m.deps.CanResume != nil && n.FirstTurnStarted && !m.deps.CanResume(n.Key) {
			m.finishLocked(n, depthLost, "")
			n.Reason = "its branch could not be resumed after a restart"
			n.Approval = nil
			recs = append(recs, n)
			fanInParents = append(fanInParents, n.Parent)
			continue
		}
		switch {
		case n.LeafID != "" && n.State == depthActive && !n.FirstTurnStarted:
			firsts = append(firsts, firstTurn{n.Key, n.Title, n.Prompt})
		case n.LeafID != "" && n.State == depthActive && n.AckPending:
			acks = append(acks, n.Key)
		}
		if n.Approval != nil && live {
			approvals = append(approvals, n.Key)
		}
		if n.Results != "" {
			results = append(results, n.Key)
		}
		if n.State == depthFanned {
			fanInParents = append(fanInParents, n.Key)
		}
	}
	rec := snap(recs...)
	m.mu.Unlock()

	m.forget(expired...)
	m.persist(rec...)
	if m.deps.Surface != nil {
		for _, n := range register {
			if err := m.deps.Surface.RegisterLeaf(n.Tree, n.Key, n.LeafID, n.Title); err != nil {
				depthLog.Warnf("restore leaf view %s (tree %s): %v", n.Key, n.Tree, err)
			}
		}
	}
	for _, f := range firsts {
		m.startFirstTurn(f.key, f.title, f.prompt, false)
	}
	for _, k := range acks {
		m.mu.Lock()
		n := m.nodes[k]
		started := n != nil && n.AckTurnStarted
		m.mu.Unlock()
		if started {
			// The ack turn ran (at least partly) in the last process: close the
			// leaf with what it said, as the ack promised.
			m.autoReport(k)
		} else {
			m.runAck(k)
		}
	}
	for _, k := range approvals {
		m.mu.Lock()
		var ap *depthApproval
		if n := m.nodes[k]; n != nil && n.Approval != nil {
			c := *n.Approval
			ap = &c
		}
		m.mu.Unlock()
		if ap != nil && m.deps.Approve != nil {
			if err := m.presentApproval(k, ap); err != nil {
				depthLog.Warnf("re-present fan-out approval for %s: %v", k, err)
			}
		}
	}
	for _, k := range results {
		m.mu.Lock()
		msg := ""
		if n := m.nodes[k]; n != nil {
			msg = n.Results
		}
		m.mu.Unlock()
		if msg != "" {
			m.deliverResults(k, msg)
		}
	}
	for _, k := range fanInParents {
		m.checkFanIn(k)
	}
	for tree := range trees {
		m.pushFeed(tree)
	}
	if len(m.nodes) > 0 || len(expired) > 0 {
		depthLog.Infof("restored %d depth node(s) in %d tree(s); %d expired record(s) deleted", len(register), len(trees), len(expired))
	}
}

// --- claims and status ---

func (m *DepthManager) claims(caller, action, text string) (string, error) {
	out, rec, err := m.claimsLocked(caller, action, text)
	m.persist(rec...)
	return out, err
}

func (m *DepthManager) claimsLocked(caller, action, text string) (string, []depthNode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.nodes[caller]
	if n == nil {
		return "", nil, fmt.Errorf("depth_claims: this session is not part of a depth-mode tree")
	}
	root := m.nodes[n.Tree]
	if root == nil {
		return "", nil, fmt.Errorf("depth_claims: the tree's root record is missing")
	}
	switch action {
	case "", "list":
		if len(root.Claims) == 0 {
			return "No claims yet.", nil, nil
		}
		var b strings.Builder
		for _, c := range root.Claims {
			fmt.Fprintf(&b, "- [%s] %s (%s)\n", c.Node, c.Text, c.At.Local().Format("15:04"))
		}
		return strings.TrimRight(b.String(), "\n"), nil, nil
	case "add":
		text = strings.TrimSpace(text)
		if text == "" {
			return "", nil, fmt.Errorf("depth_claims add: say what you claim")
		}
		var overlap []depthClaim
		words := claimWords(text)
		for _, c := range root.Claims {
			for w := range claimWords(c.Text) {
				if words[w] {
					overlap = append(overlap, c)
					break
				}
			}
		}
		title := n.Title
		if n.LeafID == "" {
			title = "root"
		}
		now := m.now()
		root.Claims = append(root.Claims, depthClaim{Node: title, Text: text, At: now})
		root.Updated = now
		rec := snap(root)
		if len(overlap) == 0 {
			return "Claimed.", rec, nil
		}
		var b strings.Builder
		b.WriteString("Claimed. Existing claims that share a word with yours:\n")
		for _, c := range overlap {
			fmt.Fprintf(&b, "- [%s] %s\n", c.Node, c.Text)
		}
		return strings.TrimRight(b.String(), "\n"), rec, nil
	}
	return "", nil, fmt.Errorf("depth_claims: action must be list or add")
}

// claimWords are the words of a claim that count for overlap: lowercased,
// four letters or more.
func claimWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	}) {
		if len([]rune(w)) >= 4 {
			out[w] = true
		}
	}
	return out
}

func (m *DepthManager) status(caller string) string {
	m.mu.Lock()
	n := m.nodes[caller]
	if n == nil {
		m.mu.Unlock()
		return "This session is not part of a depth-mode tree."
	}
	ns := m.treeNodesLocked(n.Tree)
	cp := make([]depthNode, len(ns))
	queued := make([]bool, len(ns))
	for i, x := range ns {
		cp[i] = *x
		queued[i] = m.queued[x.Key] > 0
	}
	byKey := map[string][]int{}
	rootIdx := -1
	for i, x := range cp {
		if x.LeafID == "" {
			rootIdx = i
			continue
		}
		byKey[x.Parent] = append(byKey[x.Parent], i)
	}
	m.mu.Unlock()

	var b strings.Builder
	if rootIdx >= 0 {
		r := cp[rootIdx]
		fmt.Fprintf(&b, "Tree of %s (root: %s", r.Key, r.State)
		if r.ContextAtFanout > 0 {
			fmt.Fprintf(&b, ", context at last fan-out %d tokens", r.ContextAtFanout)
		}
		b.WriteString(")\n")
	}
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, i := range byKey[parent] {
			x := cp[i]
			ind := strings.Repeat("  ", depth)
			state := x.State
			if x.State == depthActive {
				switch {
				case x.AckPending:
					state = "active, wrapping up after the user's ack"
				case queued[i] || (m.deps.Surface != nil && m.deps.Surface.LeafBusy(x.Key)):
					state = "active, running"
				default:
					state = "active, waiting for the user"
				}
			}
			if x.State == depthFanned {
				open := 0
				for _, k := range x.Children {
					for _, j := range byKey[x.Key] {
						if cp[j].Key == k && !cp[j].done() {
							open++
						}
					}
				}
				state = fmt.Sprintf("fanned, waiting on %d leaves", open)
			}
			if x.Approval != nil {
				state += ", fan-out pending the user's approval"
			}
			fmt.Fprintf(&b, "%s- %s [%s] level %d, %s", ind, x.Title, x.LeafID, x.Level, state)
			if c := m.costText(x.Key); c != "" {
				fmt.Fprintf(&b, ", %s", c)
			}
			if x.Key == caller {
				b.WriteString(" (you)")
			}
			b.WriteString("\n")
			if x.done() {
				fmt.Fprintf(&b, "%s  report: %s\n", ind, depthPreview(x.reportLine(), 160))
			}
			walk(x.Key, depth+1)
		}
	}
	walk(n.Tree, 0)
	return strings.TrimRight(b.String(), "\n")
}

// --- the hub's controller: one per process, routing by agent ---

var (
	depthMgrMu sync.RWMutex
	depthMgrs  = map[string]*DepthManager{}
)

func registerDepthManager(agentID string, m *DepthManager) {
	depthMgrMu.Lock()
	depthMgrs[agentID] = m
	depthMgrMu.Unlock()
}

// DepthManagerFor returns the depth manager of sessionKey's agent, or nil.
func DepthManagerFor(sessionKey string) *DepthManager {
	depthMgrMu.RLock()
	defer depthMgrMu.RUnlock()
	return depthMgrs[session.AgentIDFromAnyKey(sessionKey)]
}

// DepthDispatch implements app.DepthController: it hands each hub callback
// to the depth manager of the session's agent.
type DepthDispatch struct{}

func (DepthDispatch) LeafUserMessage(key string) {
	if m := DepthManagerFor(key); m != nil {
		m.LeafUserMessage(key)
	}
}

func (DepthDispatch) LeafText(key, text string) {
	if m := DepthManagerFor(key); m != nil {
		m.LeafText(key, text)
	}
}

func (DepthDispatch) LeafBusy(key string, busy bool) {
	if m := DepthManagerFor(key); m != nil {
		m.LeafBusy(key, busy)
	}
}

func (DepthDispatch) Ack(rootKey, leafID string) error {
	m := DepthManagerFor(rootKey)
	if m == nil {
		return fmt.Errorf("depth mode is not enabled for this agent")
	}
	return m.Ack(rootKey, leafID)
}

func (DepthDispatch) Dismiss(rootKey, leafID string) error {
	m := DepthManagerFor(rootKey)
	if m == nil {
		return fmt.Errorf("depth mode is not enabled for this agent")
	}
	return m.Dismiss(rootKey, leafID)
}

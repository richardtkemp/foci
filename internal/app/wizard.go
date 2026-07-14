package app

// Out-of-band wizard rendering for the app path (wire §12).
//
// Chat platforms run command wizards through the receive-side intercept
// (dispatch.Interceptor → Registry.HandleMessage): the wizard's prompts are
// ordinary text replies and the user's next message is the answer. The app
// has no such intercept — composer text always goes to the agent — so wizards
// are instead rendered out-of-band, like asks: each step goes out as a
// structured wizard.step frame, the app answers with a wizard.response, and
// the flow terminates with a wizard.end. Only clients that advertised the
// "wizard" capability get this; uncapable clients keep the legacy behavior
// (prompt as a plain system message).
//
// Wizards are scoped by session key (see command/wizard.go), so each
// conversation fronts at most its own wizard and chat-side traffic in other
// sessions can't advance or replace it. The session below tags the one app
// conversation allowed to answer and snapshots the Registry's per-scope
// generation, so a same-scope replacement still expires the stale screen.
//
// Sessions are persisted to the session index (mirroring the Registry's own
// wizard_pending) and restored at agent setup, so a server restart doesn't
// orphan an app mid-wizard: the restored Registry wizard and the restored
// session re-link, and the phone's next wizard.response routes as if nothing
// happened.

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"foci/internal/app/fap"
	"foci/internal/command"
	"foci/internal/question"
)

// wizardSessionsMetaKey is the agent_metadata key holding this agent's live
// app wizard sessions (JSON map wizardId → persistedWizardSession).
const wizardSessionsMetaKey = "wizard_app_sessions"

// wizardSession is one live out-of-band wizard: the conversation that started
// it (the only one allowed to answer), the wizard's scope in the command
// Registry, the last-emitted step (for answer resolution + staleness checks),
// and the Registry generation of the wizard it fronts.
type wizardSession struct {
	id     string // wizardId on the wire
	stepID string // last-emitted step; a response must echo it
	scope  string // Registry wizard scope (the conversation's session key)
	title  string // display title (the invoking command)
	gen    uint64 // Registry.WizardGen(scope) at activation — rejects replaced wizards
	step   question.Question
	b      *convBinding
	conn   *appConn
}

// persistedWizardSession is the durable form of a wizardSession. The step
// question itself isn't persisted — after a restart the structured step is
// re-derived from the restored wizard (WizardPendingStep), with answer
// resolution falling back to verbatim text if it can't be.
type persistedWizardSession struct {
	ConvID  string    `json:"convId"`
	AgentID string    `json:"agentId"`
	Scope   string    `json:"scope"`
	StepID  string    `json:"stepId"`
	Title   string    `json:"title,omitempty"`
	SavedAt time.Time `json:"savedAt"`
}

// maybeStartWizard starts an out-of-band wizard session when the just-run
// command activated one. Called by dispatchCommand with the scope's wizard
// generation snapshotted before Dispatch; a changed generation plus an active
// wizard means this command installed a fresh wizard for the scope (including
// replacing an older one). title is the invoking command line ("/agents new"),
// shown by the app as the wizard's heading. Returns true when the first step
// was sent as a wizard.step frame — the caller must then skip the plain-text
// render of resp, which is that same prompt.
func (h *Hub) maybeStartWizard(conn *appConn, b *convBinding, resp command.Response, title, scope string, genBefore uint64) bool {
	reg := conn.commands
	gen := reg.WizardGen(scope)
	if gen == genBefore || !reg.WizardActive(scope) {
		return false
	}
	if !b.supportsFeature(featureWizard) {
		return false // legacy client: prompt stays a plain system message
	}

	// One wizard per scope: a still-open app session fronting this scope's
	// previous wizard is now stale — end it on its own conversation before
	// registering the replacement.
	h.endScopeWizard(scope, fap.WizardCancelled, "Superseded by a new wizard.")

	s := &wizardSession{
		id:    fap.NewULID(),
		scope: scope,
		title: title,
		gen:   gen,
		b:     b,
		conn:  conn,
	}
	h.wizardMu.Lock()
	h.wizards[s.id] = s
	h.wizardByScope[scope] = s.id
	h.wizardMu.Unlock()

	h.sendWizardStep(s, resp.Text, nil)
	appLog.Debugf("wizard started: id=%s conv=%s scope=%s title=%q", s.id, b.convID, scope, s.title)
	return true
}

// sendWizardStep emits the wizard's current step: the Registry's structured
// step when the wizard provides one (option buttons), else a free-text step
// whose text is the wizard's plain prompt. media (nil for most steps) is a
// staged WizardDocProvider blob the app renders inline above the step. Mints
// and records the stepID the next response must echo, then checkpoints the
// session.
func (h *Hub) sendWizardStep(s *wizardSession, promptText string, media *fap.WizardStepMedia) {
	q := s.conn.commands.WizardPendingStep(s.scope)
	if q == nil {
		q = &question.Question{Question: promptText}
	}
	s.stepID = fap.NewULID()
	s.step = *q
	h.persistWizardSessions(s.b.agentID)
	s.b.send(fap.WizardStep{
		ConversationID: s.b.convID,
		WizardID:       s.id,
		StepID:         s.stepID,
		Title:          s.title,
		Step:           fapWizardQuestion(q),
		Media:          media,
	})
}

// handleWizardResponse routes a wizard.response back into the command
// registry's wizard, then emits the follow-up frame: the next wizard.step
// while the wizard stays active, or a terminal wizard.end.
func (h *Hub) handleWizardResponse(f fap.WizardResponse) {
	h.wizardMu.Lock()
	s := h.wizards[f.WizardID]
	h.wizardMu.Unlock()

	if s == nil {
		// Unknown wizard — a stale row on the app (e.g. its wizard exceeded the
		// persistence TTL across a restart). Resolve it client-side.
		h.mu.RLock()
		b := h.convs[f.ConversationID]
		h.mu.RUnlock()
		if b != nil {
			b.send(fap.WizardEnd{ConversationID: f.ConversationID, WizardID: f.WizardID,
				Status: fap.WizardExpired, Text: "This wizard is no longer active."})
		}
		return
	}
	if f.ConversationID != s.b.convID || f.StepID != s.stepID {
		appLog.Debugf("wizard response dropped as stale (conv=%s): id=%s step=%s (want %s)", s.b.convID, f.WizardID, f.StepID, s.stepID)
		return
	}
	reg := s.conn.commands
	if reg.WizardGen(s.scope) != s.gen || !reg.WizardActive(s.scope) {
		// The scope's wizard was replaced (e.g. restarted via Telegram in the
		// same session) or finished behind our back — this session no longer
		// fronts it.
		h.endWizard(s, fap.WizardExpired, "This wizard is no longer active.")
		return
	}

	text, cancelled := resolveWizardAnswer(&s.step, f.Data)
	resp, docPath, handled := reg.HandleMessage(s.scope, text)
	if !handled {
		h.endWizard(s, fap.WizardExpired, "This wizard is no longer active.")
		return
	}
	stillOurs := reg.WizardActive(s.scope) && reg.WizardGen(s.scope) == s.gen

	// A WizardDocProvider file (e.g. the /android QR) renders INLINE in the
	// wizard screen: stage it as a blob and reference it from the next step.
	// When the wizard just ended (no next step to carry it) — or the blob
	// store balks — fall back to an ordinary in-chat Media frame, mirroring
	// the Telegram path. Consume-once either way: send/stage, then remove.
	var media *fap.WizardStepMedia
	if docPath != "" {
		if stillOurs {
			if meta, err := h.blobs.putFile(docPath, "photo"); err == nil {
				media = &fap.WizardStepMedia{BlobID: meta.id, MIME: meta.mime, Name: meta.name}
			} else {
				appLog.Warnf("wizard doc blob staging failed (conv=%s), sending in-chat: %v", s.b.convID, err)
				_ = s.conn.SendDocumentToChat(s.b.chatID, docPath, "")
			}
		} else {
			_ = s.conn.SendDocumentToChat(s.b.chatID, docPath, "")
		}
		_ = os.Remove(docPath)
	}
	if stillOurs {
		h.sendWizardStep(s, resp, media)
		return
	}
	status := fap.WizardDone
	if cancelled {
		status = fap.WizardCancelled
	}
	h.endWizard(s, status, resp)
}

// endWizard sends the terminal frame for s and deregisters it. It does not
// touch the Registry: the wizard itself already ended (done/cancel) or is no
// longer ours (replaced).
func (h *Hub) endWizard(s *wizardSession, status, text string) {
	h.wizardMu.Lock()
	delete(h.wizards, s.id)
	if h.wizardByScope[s.scope] == s.id {
		delete(h.wizardByScope, s.scope)
	}
	h.wizardMu.Unlock()
	h.persistWizardSessions(s.b.agentID)
	s.b.send(fap.WizardEnd{ConversationID: s.b.convID, WizardID: s.id, Status: status, Text: text})
	appLog.Debugf("wizard ended (conv=%s): id=%s status=%s", s.b.convID, s.id, status)
}

// endScopeWizard ends the live session (if any) fronting the given wizard
// scope — used when a new wizard supersedes it.
func (h *Hub) endScopeWizard(scope, status, text string) {
	h.wizardMu.Lock()
	id := h.wizardByScope[scope]
	s := h.wizards[id]
	h.wizardMu.Unlock()
	if s != nil {
		h.endWizard(s, status, text)
	}
}

// persistWizardSessions checkpoints agentID's live sessions to the session
// index so they survive a server restart alongside the Registry's own
// persisted wizard state. Best-effort; no-op without a session index.
func (h *Hub) persistWizardSessions(agentID string) {
	idx := h.deps.SessionIndex
	if idx == nil {
		return
	}
	h.wizardMu.Lock()
	saved := make(map[string]persistedWizardSession)
	for id, s := range h.wizards {
		if s.b.agentID != agentID {
			continue
		}
		saved[id] = persistedWizardSession{
			ConvID: s.b.convID, AgentID: agentID, Scope: s.scope,
			StepID: s.stepID, Title: s.title, SavedAt: time.Now(),
		}
	}
	h.wizardMu.Unlock()
	if len(saved) == 0 {
		if err := idx.DeleteAgentMetadata(agentID, wizardSessionsMetaKey); err != nil {
			appLog.Warnf("clear persisted wizard sessions: %v", err)
		}
		return
	}
	blob, err := json.Marshal(saved)
	if err != nil {
		appLog.Warnf("marshal wizard sessions: %v", err)
		return
	}
	if err := idx.SetAgentMetadata(agentID, wizardSessionsMetaKey, string(blob)); err != nil {
		appLog.Warnf("persist wizard sessions: %v", err)
	}
}

// restoreWizardSessions rebuilds agentID's persisted app wizard sessions after
// a restart, re-linking each to the Registry wizard its scope restored (the
// Registry restore ran earlier, at command registration). Sessions whose
// wizard did NOT survive (snapshot-incapable, TTL-expired) are dropped; the
// app's next wizard.response for them gets the expired self-heal. Called from
// setupAgent once conn.commands is wired. No frames are emitted — the app
// already shows the step; this only makes future responses route.
func (h *Hub) restoreWizardSessions(conn *appConn, agentID string) {
	idx := h.deps.SessionIndex
	if idx == nil || conn.commands == nil {
		return
	}
	raw, err := idx.GetAgentMetadata(agentID, wizardSessionsMetaKey)
	if err != nil || raw == "" {
		return
	}
	var saved map[string]persistedWizardSession
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		appLog.Warnf("unmarshal wizard sessions: %v — dropping", err)
		_ = idx.DeleteAgentMetadata(agentID, wizardSessionsMetaKey)
		return
	}
	restored := 0
	for id, p := range saved {
		if p.AgentID != agentID || !conn.commands.WizardActive(p.Scope) {
			continue // wizard didn't survive the restart — drop; expired self-heal covers it
		}
		s := &wizardSession{
			id:     id,
			stepID: p.StepID,
			scope:  p.Scope,
			title:  p.Title,
			gen:    conn.commands.WizardGen(p.Scope),
			b:      h.ensureBinding(nil, agentID, p.ConvID),
			conn:   conn,
		}
		if q := conn.commands.WizardPendingStep(p.Scope); q != nil {
			s.step = *q // re-derive option labels for qa:<i> resolution
		}
		h.wizardMu.Lock()
		h.wizards[id] = s
		h.wizardByScope[p.Scope] = id
		h.wizardMu.Unlock()
		restored++
	}
	h.persistWizardSessions(agentID) // re-write the cleaned set
	if restored > 0 {
		appLog.Infof("restored %d in-flight wizard session(s) for agent %s", restored, agentID)
	}
}

// resolveWizardAnswer translates a wizard.response's data into the text the
// wizard's Handle expects: the Cancel sentinel becomes the /cancel command
// (Registry.HandleMessage's existing abort path), a "qa:<i>" button becomes
// that option's label, and anything else is typed text passed verbatim. A
// malformed button token falls back to verbatim (Handle re-asks on invalid
// input, so nothing is lost).
func resolveWizardAnswer(q *question.Question, data string) (text string, cancelled bool) {
	if data == question.CancelData {
		return "/cancel", true
	}
	if strings.HasPrefix(data, "qa:") {
		if answer, _, err := question.ResolveAnswer(q, data); err == nil {
			return answer, false
		}
	}
	return data, false
}

// fapWizardQuestion maps a structured wizard step onto the wire Question
// shape, mirroring batchQuestionsFor in cmd/foci-gw (options get "qa:<index>"
// data and NO Cancel choice — the app's wizard screen has its own Cancel).
func fapWizardQuestion(q *question.Question) fap.Question {
	choices := make([]fap.Choice, len(q.Options))
	for i, opt := range q.Options {
		choices[i] = fap.Choice{
			Label:       opt.Label,
			Data:        question.OptionData(i),
			Description: opt.Description,
		}
	}
	return fap.Question{Text: q.Question, Header: q.Header, Choices: choices}
}

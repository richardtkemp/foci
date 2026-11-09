package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
)

// A user-facing session resumes a turn a restart cut (#2999, Dick 2026-11-09:
// "user-facing sessions should resume too!"; #2589 does the same for spawns).
//
// While a qualifying turn runs on a session (resumableTurn), the session
// carries a cut_turn record (session.MetaKeyCutTurn): written when the first
// turn on it begins, deleted when the last one ends. Two things leave it in
// place:
//   - the process dies (a crash, a power cut): nothing runs to delete it;
//   - a shutdown cuts the turn: runShutdown calls MarkTurnsCut once its
//     graceful wait is over, before it closes the backends, so a turn that
//     ends from then on was cut and keeps its record. A turn that ends inside
//     the graceful wait finished, and deletes it (requirement 4).
//
// At startup RestoreCutTurns (a restore phase, before RestoreUndelivered)
// gives each recorded session ONE turn: a hidden resume line saying the turn
// was cut by a restart, with the cut turn's input and the rule about side
// effects (requirement 6), queued on the session's inbox through
// UsageHeldDriver like any restored message. The user sees a chat notice
// (RestartResumeNotice, requirement 3). The record stays until the resume
// turn ends, so a restart during the resume resumes again, up to
// maxRestartResumes; then the session gets a notice and no further resume.
//
// Spawns keep the #2589 path (their sessions never write a record), batch
// turns have no session the user reads, and keepalive, the memory passes and
// background (todo) work rerun on their own schedules, so none of them
// writes one (requirement 5).

// maxRestartResumes is how many restarts in a row may resume one cut turn.
const maxRestartResumes = 3

// cutTurnInputMax caps the cut turn's input kept in the record.
const cutTurnInputMax = 4000

// cutTurnRecord is one session's running (or cut) turn as stored.
type cutTurnRecord struct {
	Trigger     string    `json:"trigger,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	Input       string    `json:"input,omitempty"`
	Attachments int       `json:"attachments,omitempty"`
	// Resumes counts the restarts that resumed this turn already. A record
	// with Resumes > 0 was written by RestoreCutTurns; the resume turn that
	// begins on it keeps it as it is.
	Resumes int `json:"resumes,omitempty"`
}

// cutTurnState is the agent's in-memory side of the cut_turn records.
type cutTurnState struct {
	mu sync.Mutex
	// running counts the qualifying turns in flight per session (a folded
	// follow-up is a second OrchestrateFullTurn on the same session).
	running map[string]int
	// cut is set by MarkTurnsCut: from then on an ending turn keeps its
	// record.
	cut atomic.Bool
}

// cutTurnExcludedTriggers are the triggers whose turns a restart does not
// resume: they rerun on their own schedule, or have their own restore path.
var cutTurnExcludedTriggers = map[string]bool{
	TriggerKeepalive:     true,
	"reflection":         true,
	"compaction_memory":  true,
	"session_end_memory": true,
	"compaction-resume":  true,
	"background":         true, // background (todo) work: its scheduler runs it again
	"branch":             true, // an external one-shot branch: its caller is gone after a restart
	"depth_ack":          true, // DepthRestore re-runs or closes an interrupted ack (#2588)
	"depth_task":         true, // DepthRestore resumes a task leaf's cut task turn (#2998)
}

// resumableTurn reports whether a restart that cuts ts resumes its session.
func resumableTurn(ts *TurnState) bool {
	if ts.SessionKey == "" || ts.Purpose != "" || cutTurnExcludedTriggers[ts.Trigger] {
		return false
	}
	if _, isSpawn := tools.SpawnOfSession(ts.SessionKey); isSpawn {
		return false
	}
	return true
}

// MarkTurnsCut tells the agent that the shutdown's graceful wait is over:
// every turn still running is being cut, so as each ends it keeps its
// cut_turn record for the next process. Idempotent.
func (a *Agent) MarkTurnsCut() {
	a.cutTurn.cut.Store(true)
	if n := a.runningCutTurnSessions(); n > 0 {
		a.logger().Infof("shutdown: %d session(s) have a turn still running — each resumes after the restart", n)
	}
}

func (a *Agent) runningCutTurnSessions() int {
	a.cutTurn.mu.Lock()
	defer a.cutTurn.mu.Unlock()
	return len(a.cutTurn.running)
}

// trackCutTurn wires ts into the cut_turn records and the queued-input
// store. It wraps ts.Ctx's primary-written callback, which every transport
// fires the moment the turn's input reaches its backend (a delegated write,
// a fold, the API's first answered call): from then the turn's queued input
// is settled (the turn has it), and a qualifying turn holds its session's
// record. A turn cut before that moment never had its input, so its
// messages stay queued and run as messages after the restart, not as a
// resume. A folded follow-up adds its text to the running turn's record. The
// returned call ends the record when the turn ends; call it on every path.
func (a *Agent) trackCutTurn(ts *TurnState) (end func()) {
	prev := OnPrimaryWrittenFromContext(ts.Ctx)
	queued := queuedInputsFromContext(ts.Ctx)
	resumable := a.SessionIndex != nil && resumableTurn(ts)
	sk := ts.SessionKey
	var once sync.Once
	var holds atomic.Bool
	ts.Ctx = WithOnPrimaryWritten(ts.Ctx, func() {
		prev()
		once.Do(func() {
			a.forgetQueuedInput(queued...)
			if !resumable {
				return
			}
			a.cutTurn.mu.Lock()
			defer a.cutTurn.mu.Unlock()
			if ts.foldedFollowUp {
				a.appendCutTurnInputLocked(sk, ts)
				return
			}
			if a.cutTurn.running == nil {
				a.cutTurn.running = map[string]int{}
			}
			a.cutTurn.running[sk]++
			holds.Store(true)
			if a.cutTurn.running[sk] == 1 {
				a.writeCutTurnLocked(sk, ts)
			}
		})
	})
	return func() {
		if !holds.Load() {
			return
		}
		a.cutTurn.mu.Lock()
		defer a.cutTurn.mu.Unlock()
		a.cutTurn.running[sk]--
		if a.cutTurn.running[sk] > 0 {
			return
		}
		delete(a.cutTurn.running, sk)
		if a.cutTurn.cut.Load() {
			a.logger().Infof("session=%s turn cut by the shutdown — it resumes after the restart", sk)
			return
		}
		a.dropCutTurn(sk)
	}
}

// appendCutTurnInputLocked adds a folded follow-up's text to the record of
// the turn it folded into, when that turn holds one (it may be a turn that
// keeps none, such as a depth task's, whose own restore covers it). Under
// cutTurn.mu.
func (a *Agent) appendCutTurnInputLocked(sk string, ts *TurnState) {
	if a.cutTurn.running[sk] == 0 {
		return
	}
	rec, ok := a.loadCutTurn(sk)
	if !ok {
		return
	}
	rec.Input = truncateCutInput(strings.TrimSpace(rec.Input + "\n\n" + strings.Join(ts.Texts, "\n\n")))
	rec.Attachments += len(ts.Attachments)
	a.saveCutTurn(sk, rec)
}

// writeCutTurnLocked stores ts as sk's running turn, unless sk holds a
// record a restart left for its resume turn (Resumes > 0): that one is kept,
// so a restart during the resume still names the original input and counts
// toward maxRestartResumes. Under cutTurn.mu.
func (a *Agent) writeCutTurnLocked(sk string, ts *TurnState) {
	if rec, ok := a.loadCutTurn(sk); ok && rec.Resumes > 0 {
		return
	}
	rec := cutTurnRecord{
		Trigger:     ts.Trigger,
		StartedAt:   ts.StartedAt,
		Input:       truncateCutInput(strings.Join(ts.Texts, "\n\n")),
		Attachments: len(ts.Attachments),
	}
	a.saveCutTurn(sk, rec)
}

func truncateCutInput(s string) string {
	if len(s) <= cutTurnInputMax {
		return s
	}
	cut := cutTurnInputMax
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " […]"
}

// utf8RuneStart reports whether b can begin a UTF-8 encoded rune.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func (a *Agent) loadCutTurn(sk string) (cutTurnRecord, bool) {
	raw, err := a.SessionIndex.GetSessionMetadata(sk, session.MetaKeyCutTurn)
	if err != nil || raw == "" {
		return cutTurnRecord{}, false
	}
	var rec cutTurnRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		a.logger().Warnf("cut turn: record of %s unreadable: %v", sk, err)
		return cutTurnRecord{}, false
	}
	return rec, true
}

func (a *Agent) saveCutTurn(sk string, rec cutTurnRecord) {
	raw, err := json.Marshal(rec)
	if err != nil {
		a.logger().Warnf("cut turn: encode the record of %s: %v", sk, err)
		return
	}
	if err := a.SessionIndex.SetSessionMetadata(sk, session.MetaKeyCutTurn, string(raw)); err != nil {
		a.logger().Warnf("cut turn: persist the record of %s: %v", sk, err)
	}
}

// restartResumeLine is the hidden input of a session's resume turn after a
// restart cut its turn (#2999). quote repeats the cut turn's input: for a
// conversation the cut may have lost it from (the API transport saves a
// turn when it ends; a delegated session without a resume id starts
// afresh). A delegated backend with a kept conversation already has it,
// and repeating it there would hand the agent the same input twice.
func restartResumeLine(rec cutTurnRecord, quote bool) string {
	var b strings.Builder
	b.WriteString("[SYSTEM: FOCI RESTARTED] foci restarted while you were working in this conversation, so your last turn")
	if !rec.StartedAt.IsZero() {
		b.WriteString(" (started " + rec.StartedAt.UTC().Format("2006-01-02 15:04 UTC") + ")")
	}
	b.WriteString(" was cut off before it finished. Carry on with that work from where you left off. " +
		"Tool calls it made may or may not have completed: before you repeat anything with a side effect " +
		"(a message sent, a file written, a commit, a deploy), check whether it already happened.")
	if rec.Resumes > 1 {
		fmt.Fprintf(&b, " This is restart %d in a row during this work.", rec.Resumes)
	}
	if quote && rec.Input != "" {
		b.WriteString("\n\nThe cut may have lost that turn's input from this conversation, so here it is again:\n\n" + rec.Input)
		if rec.Attachments > 0 {
			fmt.Fprintf(&b, "\n\n(It also had %d attachment(s), not repeated here.)", rec.Attachments)
		}
	}
	return b.String()
}

// restartResumeEnvelope is sk's resume turn input, driven by driver: a user
// message (the turn renders in the session's chat) that no client shows,
// never folded into another turn and never taken as an ask answer.
func restartResumeEnvelope(sk string, driver Driver, rec cutTurnRecord, quote bool) Envelope {
	return Envelope{
		SessionKey:    sk,
		Text:          restartResumeLine(rec, quote),
		ChatID:        session.ChatIDFromKey(sk),
		ReceivedAt:    time.Now(),
		Steer:         platform.SteerNever,
		Automated:     true,
		Driver:        driver,
		restartResume: true,
	}
}

// Chat notices of a restart resume (requirement 3).
const (
	restartResumeNotice = "🔄 foci restarted while I was working here; resuming."
	restartGiveUpNotice = "⚠️ foci restarted %d times while I was working here; not resuming it again. Send a message to carry on."
)

// RestoreCutTurns gives each of this agent's sessions whose turn a restart
// cut one resume turn, and runs the user messages the last process accepted
// but never started (queued_input). Call once at startup, after the inboxes
// and platforms are up and BEFORE DelegatedManager.RestoreUndelivered, so the
// resume line is the first thing the session sees. A session whose resume
// cannot be built (no driver hook) keeps its record for the next start.
func (a *Agent) RestoreCutTurns() {
	if a.SessionIndex == nil {
		return
	}
	keys, err := a.SessionIndex.SessionKeysWithMetadata(session.MetaKeyCutTurn)
	if err != nil {
		a.logger().Warnf("cut turn: restore: %v", err)
	}
	for _, sk := range keys {
		if parsed, err := session.ParseSessionKey(sk); err != nil || parsed.AgentID != a.AgentID {
			continue
		}
		a.restoreCutTurn(sk)
	}
	a.restoreQueuedInput()
}

func (a *Agent) restoreCutTurn(sk string) {
	if _, isSpawn := tools.SpawnOfSession(sk); isSpawn {
		// Never written for a spawn; a stale one must not resume a spawn's
		// branch beside #2589's own resume.
		a.dropCutTurn(sk)
		return
	}
	a.cutTurn.mu.Lock()
	if a.cutTurn.running[sk] > 0 {
		// A turn of this process already runs on sk and owns the record.
		a.cutTurn.mu.Unlock()
		return
	}
	rec, ok := a.loadCutTurn(sk)
	resume := ok && rec.Resumes < maxRestartResumes && a.UsageHeldDriver != nil
	if resume {
		// Saved before the resume can run: the resume turn keeps it.
		rec.Resumes++
		a.saveCutTurn(sk, rec)
	}
	a.cutTurn.mu.Unlock()
	switch {
	case !ok:
		a.dropCutTurn(sk)
		return
	case rec.Resumes >= maxRestartResumes && !resume:
		a.logger().Warnf("cut turn: %s was cut by %d restarts in a row — not resuming it again", sk, rec.Resumes+1)
		a.dropCutTurn(sk)
		a.restartResumeNotify(sk, fmt.Sprintf(restartGiveUpNotice, rec.Resumes+1))
		return
	case !resume:
		a.logger().Warnf("cut turn: no driver for the resume of %s — kept for the next start", sk)
		return
	}
	quote := a.DelegatedManager == nil || !a.HasResumableSession(sk)
	env := restartResumeEnvelope(sk, a.UsageHeldDriver(sk), rec, quote)
	a.logger().Infof("cut turn: resuming %s (trigger=%s, started %s, resume %d)", sk, rec.Trigger, rec.StartedAt.Format(time.RFC3339), rec.Resumes)
	if !a.Enqueue(env) {
		a.logger().Warnf("cut turn: inbox refused the resume of %s — kept for the next start", sk)
		return
	}
	a.restartResumeNotify(sk, restartResumeNotice)
}

func (a *Agent) dropCutTurn(sk string) {
	if err := a.SessionIndex.DeleteSessionMetadata(sk, session.MetaKeyCutTurn); err != nil {
		a.logger().Warnf("cut turn: clear the record of %s: %v", sk, err)
	}
}

func (a *Agent) restartResumeNotify(sk, text string) {
	if fn := a.RestartResumeNotice; fn != nil {
		fn(sk, text)
	}
}

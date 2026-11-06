package relogin

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"foci/internal/log"
)

// Advance warning that the Claude Code sign-in is about to end (#2864).
//
// A claude.ai sign-in has a hard end date. Claude Code reports it (2.1.318+) as
// credential_expires_at on results and loginExpiresAt in `claude auth status`.
// Renewing the token does not move it, only a new sign-in does, and that needs a
// human. Claude Code's own wording (2.1.318 binary) is that the sign-in ends in
// the three days up to that date, and it starts warning ten days before it. The
// constants below follow those figures.
const (
	expiryEarlyMargin = 3 * 24 * time.Hour  // the sign-in can end this long before the date
	expiryWarnWindow  = 10 * 24 * time.Hour // first notice, this long before the date
	expiryUrgentAhead = 24 * time.Hour      // second notice, this long before the earliest end
	// expirySameLogin is how far a reported date can move and still be the same
	// sign-in. A new sign-in moves it by weeks.
	expirySameLogin = 24 * time.Hour
)

// The stages of one sign-in's warnings. Each is sent at most once.
const (
	expiryStageNone = iota
	expiryStageWarned
	expiryStageUrgent
)

// expiryState is what has been sent, persisted so that a restart inside the
// window does not repeat it.
type expiryState struct {
	ExpiresAt time.Time `json:"expires_at"`
	Stage     int       `json:"stage"`
}

// ExpiryWarner decides when to tell the user that the sign-in is ending. One
// serves the whole process, as all Claude Code agents share one credential.
type ExpiryWarner struct {
	path string

	mu      sync.Mutex
	state   expiryState
	sending bool
}

// NewExpiryWarner returns a warner that keeps its state in the file at path. A
// missing or unreadable file means nothing was sent yet.
func NewExpiryWarner(path string) *ExpiryWarner {
	w := &ExpiryWarner{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.NewComponentLogger("relogin").Warnf("read sign-in expiry state: %v", err)
		}
		return w
	}
	if err := json.Unmarshal(data, &w.state); err != nil {
		log.NewComponentLogger("relogin").Warnf("parse sign-in expiry state %s: %v", path, err)
		w.state = expiryState{}
	}
	return w
}

// Observe takes a reported end date. If a notice is due it calls send, which
// returns whether the notice reached a chat; one that did not is tried again at
// the next Observe. A zero date does nothing. While one notice is being sent,
// other calls return at once and send nothing.
func (w *ExpiryWarner) Observe(expiresAt, now time.Time, send func(notice string) bool) {
	if expiresAt.IsZero() {
		return
	}
	stage := expiryStage(expiresAt, now)

	w.mu.Lock()
	if w.sending {
		w.mu.Unlock()
		return
	}
	switch delta := expiresAt.Sub(w.state.ExpiresAt); {
	case w.state.ExpiresAt.IsZero() || delta > expirySameLogin:
		// A first date, or a new sign-in: its stages start from nothing.
		w.state = expiryState{ExpiresAt: expiresAt}
	case delta < -expirySameLogin:
		// A long-running process still holding the previous sign-in.
		w.mu.Unlock()
		return
	}
	if stage <= w.state.Stage {
		w.mu.Unlock()
		return
	}
	w.sending = true
	w.mu.Unlock()

	delivered := send(expiryNotice(expiresAt, now))

	w.mu.Lock()
	defer w.mu.Unlock()
	w.sending = false
	if !delivered {
		return
	}
	w.state.Stage = stage
	if err := w.save(); err != nil {
		log.NewComponentLogger("relogin").Warnf("save sign-in expiry state: %v", err)
	}
}

// expiryStage returns the stage due at now: nothing outside the window, the
// urgent stage from a day before the earliest end, also after the date has gone.
func expiryStage(expiresAt, now time.Time) int {
	left := expiresAt.Sub(now)
	switch {
	case left >= expiryWarnWindow:
		return expiryStageNone
	case left > expiryEarlyMargin+expiryUrgentAhead:
		return expiryStageWarned
	}
	return expiryStageUrgent
}

// expiryNotice is the text the user reads. It names what to do, and that signing
// in again is the only thing that helps.
func expiryNotice(expiresAt, now time.Time) string {
	const dateFormat = "2006-01-02"
	earliest := expiresAt.Add(-expiryEarlyMargin)
	from, until := earliest.UTC().Format(dateFormat), expiresAt.UTC().Format(dateFormat)
	const action = "Send /login to sign in again. Until someone does, Claude Code agents stop when it ends."
	switch {
	case !expiresAt.After(now):
		return fmt.Sprintf("⚠️ The Claude Code sign-in may have ended between %s and %s. %s", from, until, action)
	case expiryStage(expiresAt, now) == expiryStageUrgent:
		return fmt.Sprintf("⚠️ The Claude Code sign-in can end at any time from now until %s. %s", until, action)
	}
	days := int(earliest.Sub(now) / (24 * time.Hour))
	unit := "days"
	if days == 1 {
		unit = "day"
	}
	return fmt.Sprintf("⚠️ The Claude Code sign-in ends between %s and %s (in %d %s at the earliest). Renewing it does not move those dates. %s", from, until, days, unit, action)
}

// save writes the state in one step. The caller holds mu.
func (w *ExpiryWarner) save() error {
	data, err := json.Marshal(w.state)
	if err != nil {
		return err
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.path)
}

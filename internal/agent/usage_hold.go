package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"foci/internal/delegator"
	"foci/internal/tools"
	"foci/internal/usagehold"
)

// The account-wide usage hold, agent side (#2491, internal/usagehold).
//
// A delegated agent on Claude Code or codex belongs to its backend's account.
// Its backend reports usage windows (HostHooks.OnUsageWindow → ObserveUsage);
// the registry decides the account's phase; every agent on the account sees
// each phase change (StartUsageHold's subscription) and:
//   - posts one notice in its own default chat (UsageHoldNotifyFunc);
//   - while the account HOLDS, runs only keepalives and foci_remind fires
//     (scheduled_wake): user messages, cron sends, webhooks, agent messages
//     and every other injection wait in the inbox, and the periodic
//     schedulers other than keepalive see the session as rate-limited;
//   - while the account STOPS, runs nothing, keepalives and reminders
//     included;
//   - at lift, releases what waited: each session's held user messages run as
//     ONE turn (they are re-queued together, and the inbox batches them).

// usageHoldState is an agent's view of the hold.
type usageHoldState struct {
	// storeMu serialises read-modify-write of the stored held lists.
	storeMu sync.Mutex
	mu      sync.Mutex
	unsub   func()
	// lifted is closed and replaced on every phase change, so an inbox worker
	// can wait for the next one.
	changed chan struct{}
	// notices wait to be posted, in order; posting is true while a goroutine
	// posts them.
	notices []string
	posting bool
	// turns are the running turns a hold start interrupts through their
	// context (UsageHoldTurn); paused are the sessions it paused, by kind
	// (UsageHoldPause).
	turns  map[*holdTurn]bool
	paused map[string]string
}

// usageRegistry is the registry this agent reports to: UsageHoldRegistry when
// set (tests), else the process's usagehold.Default.
func (a *Agent) usageRegistry() *usagehold.Registry {
	if a.UsageHoldRegistry != nil {
		return a.UsageHoldRegistry
	}
	return usagehold.Default
}

// UsageAccount is the account this agent's usage counts against: its
// delegated backend family (Dick 2026-11-02: the same backend is the same
// account), "" for an API agent. Only a backend that reports usage windows
// (HostHooks.OnUsageWindow: Claude Code and codex) ever holds its account;
// opencode reports none, so its account stays open.
func (a *Agent) UsageAccount() string {
	if a.DelegatedManager == nil {
		return ""
	}
	return a.DelegatedManager.BackendType
}

// ObserveUsage is HostHooks.OnUsageWindow: one usage report of this agent's
// account.
func (a *Agent) ObserveUsage(ev usagehold.Event) {
	acct := a.UsageAccount()
	if acct == "" {
		return
	}
	a.usageRegistry().Observe(acct, ev, time.Now())
}

// UsageHold is the account's state now (Phase Open for an agent with no
// account).
func (a *Agent) UsageHold() usagehold.State {
	acct := a.UsageAccount()
	if acct == "" {
		return usagehold.State{}
	}
	return a.usageRegistry().State(acct, time.Now())
}

// StartUsageHold subscribes the agent to its account's phase changes. Call it
// once the DelegatedManager is set; a no-op for an agent with no account.
func (a *Agent) StartUsageHold() {
	acct := a.UsageAccount()
	if acct == "" {
		return
	}
	a.usageHold.mu.Lock()
	if a.usageHold.unsub != nil {
		a.usageHold.mu.Unlock()
		return
	}
	a.usageHold.unsub = a.usageRegistry().Subscribe(acct, a.onUsageHoldChange)
	a.usageHold.mu.Unlock()
	if st := a.UsageHold(); st.Phase != usagehold.Open {
		a.logger().Infof("usage hold (%s) restored: %s until %s", acct, st.Phase, formatLift(st.LiftAt))
	}
}

// StopUsageHold ends the subscription (agent stop).
func (a *Agent) StopUsageHold() {
	a.usageHold.mu.Lock()
	defer a.usageHold.mu.Unlock()
	if a.usageHold.unsub != nil {
		a.usageHold.unsub()
		a.usageHold.unsub = nil
	}
}

// usageHoldChanged returns a channel closed at the account's next phase
// change.
func (a *Agent) usageHoldChanged() <-chan struct{} {
	a.usageHold.mu.Lock()
	defer a.usageHold.mu.Unlock()
	if a.usageHold.changed == nil {
		a.usageHold.changed = make(chan struct{})
	}
	return a.usageHold.changed
}

// onUsageHoldChange is the agent's reaction to one phase change.
func (a *Agent) onUsageHoldChange(c usagehold.Change) {
	a.usageHold.mu.Lock()
	if a.usageHold.changed != nil {
		close(a.usageHold.changed)
	}
	a.usageHold.changed = make(chan struct{})
	a.usageHold.mu.Unlock()

	a.logger().Infof("usage hold (%s): %s -> %s (lifts %s)", c.Account, c.From, c.To.Phase, formatLift(c.To.LiftAt))
	if fn := a.UsageHoldNotifyFunc; fn != nil {
		a.queueUsageHoldNotice(fn, UsageHoldNotice(c))
	}
	// The work running at a hold start is interrupted or left to finish
	// (usage_hold_interrupt.go), before the reactions below: the hold's
	// compaction reads the paused set it records. Its backend interrupts run
	// off this goroutine, which may be that backend's reader.
	if c.From == usagehold.Open && c.To.Phase != usagehold.Open {
		a.interruptForUsageHold()
	}
	for _, fn := range a.UsageHoldChangeFunc {
		fn(c)
	}
}

// queueUsageHoldNotice posts text through fn off the caller's goroutine, in
// the order the changes happened: a start and its end must not swap.
func (a *Agent) queueUsageHoldNotice(fn func(string), text string) {
	a.usageHold.mu.Lock()
	a.usageHold.notices = append(a.usageHold.notices, text)
	start := !a.usageHold.posting
	a.usageHold.posting = true
	a.usageHold.mu.Unlock()
	if !start {
		return
	}
	go func() {
		for {
			a.usageHold.mu.Lock()
			if len(a.usageHold.notices) == 0 {
				a.usageHold.posting = false
				a.usageHold.mu.Unlock()
				return
			}
			next := a.usageHold.notices[0]
			a.usageHold.notices = a.usageHold.notices[1:]
			a.usageHold.mu.Unlock()
			fn(next)
		}
	}()
}

// UsageHoldNotice is the chat notice for one phase change.
func UsageHoldNotice(c usagehold.Change) string {
	w := c.To.Primary()
	switch c.To.Phase {
	case usagehold.Hold:
		return fmt.Sprintf("⏸️ Usage hold: the %s window is at %.0f%%. Only keepalives and reminders run until %s; your messages wait and run then.",
			windowLabel(w.Name), w.Util*100, formatLift(c.To.LiftAt))
	case usagehold.Stop:
		return fmt.Sprintf("🛑 Usage limit reached on the %s window. Nothing runs until %s; your messages wait and run then.",
			windowLabel(w.Name), formatLift(c.To.LiftAt))
	default:
		return "▶️ Usage hold ended: work resumes, and waiting messages run now."
	}
}

func windowLabel(name string) string {
	switch name {
	case "five_hour":
		return "5-hour"
	case "seven_day":
		return "7-day"
	}
	return strings.TrimSpace(name)
}

func formatLift(t time.Time) string {
	if t.IsZero() {
		return "the window resets"
	}
	return t.Local().Format("Mon 2 Jan 15:04")
}

// usageHoldPhase is the account phase now (Open with no account).
func (a *Agent) usageHoldPhase() usagehold.Phase {
	return a.UsageHold().Phase
}

// usageHoldsTrigger reports whether a turn with trigger must wait now:
// during a hold everything but keepalives, foci_remind fires and the turns
// the hold itself starts (holdPermit); during a stop everything.
func (a *Agent) usageHoldsTrigger(trigger string, holdPermit bool) bool {
	switch a.usageHoldPhase() {
	case usagehold.Hold:
		return !(isKeepaliveTrigger(trigger) || trigger == "scheduled_wake" || holdPermit)
	case usagehold.Stop:
		return true
	default:
		return false
	}
}

// UsageHoldBlocks is the periodic schedulers' view (BackgroundAgent): a
// non-keepalive scheduler may not fire during a hold or a stop; keepalive may
// fire during a hold, not during a stop. reason names the hold.
func (a *Agent) UsageHoldBlocks(keepalive bool) (blocked bool, reason string) {
	st := a.UsageHold()
	switch {
	case st.Phase == usagehold.Stop:
		return true, "usage limit reached (lifts " + formatLift(st.LiftAt) + ")"
	case st.Phase == usagehold.Hold && !keepalive:
		return true, "usage hold (lifts " + formatLift(st.LiftAt) + ")"
	}
	return false, ""
}

// usageHoldPassesPast reports whether env runs past same-class siblings
// parked on the held list: during a usage hold a foci_remind fire runs while
// the cron sends and webhooks parked ahead of it wait (#2491 requirement 6).
func (a *Agent) usageHoldPassesPast(env Envelope) bool {
	return env.Inject != nil && env.Inject.Trigger == "scheduled_wake" && a.usageHoldPhase() == usagehold.Hold
}

// parkUsageHeld parks a user message the usage hold must hold, stores it so
// it survives a restart, and tells its sender it waits and when it runs. A
// resume line goes to the head (it is the first input of the folded turn);
// it, and a copy restored after a restart, notify nobody. Worker-confined.
func (a *Agent) parkUsageHeld(inb *sessionInbox, env Envelope) {
	restored := env.usageHeldID != ""
	// It leaves the queued-input store for the held one (#2999). A restart's
	// resume line is not stored at all: its cut_turn record brings it back.
	a.forgetQueuedInput(env)
	if !env.restartResume {
		a.storeUsageHeld(&env)
	}
	if env.usageResume {
		for _, h := range inb.usageHeld {
			if h.usageResume {
				return // one resume line per session
			}
		}
		inb.usageHeld = append([]Envelope{env}, inb.usageHeld...)
	} else {
		inb.usageHeld = append(inb.usageHeld, env)
	}
	st := a.UsageHold()
	a.logger().Infof("inbox: usage hold (%s) holds a message sk=%s (%dB, resume=%v, %d held)", st.Phase, env.SessionKey, len(env.Text), env.usageResume, len(inb.usageHeld))
	if fn := a.TurnQueuedNotifyFunc; fn != nil && !env.Automated && !env.usageResume && !restored {
		text := "⏸️ Held: usage is at the hold limit, so this message waits until " + formatLift(st.LiftAt) + ", then runs."
		go fn(env.SessionKey, text)
	}
}

// releaseUsageHeld runs the session's held input once the usage hold no
// longer holds it: all of it as ONE turn, in arrival order behind the hidden
// resume line when the hold interrupted a turn (the first starts the batch,
// the rest join it through takeUsageRelease). A resume line at the head of
// real messages takes their driver and sender, so the turn renders where they
// came from. Returns false when the worker must stop.
func (a *Agent) releaseUsageHeld(ctx context.Context, inb *sessionInbox) bool {
	if len(inb.usageHeld) == 0 || a.usageHoldsTrigger("user", false) {
		return true
	}
	held := inb.usageHeld
	inb.usageHeld = nil
	if held[0].usageResume && len(held) > 1 {
		head, src := &held[0], held[1]
		head.Driver, head.UserID, head.Username, head.SenderName = src.Driver, src.UserID, src.Username, src.SenderName
		head.ChatID, head.Original, head.Human = src.ChatID, src.Original, src.Human
	}
	a.logger().Infof("inbox: usage hold lifted, running %d held input(s) as one turn sk=%s (resume=%v)", len(held), inb.sk, held[0].usageResume)
	inb.usageRelease = held[1:]
	return a.runUserBatch(ctx, inb, held[0])
}

// takeUsageRelease hands the tail of a released set to the batch its head
// starts. Worker-confined.
func (inb *sessionInbox) takeUsageRelease() []Envelope {
	out := inb.usageRelease
	inb.usageRelease = nil
	return out
}

// UsageHoldRefusal is tools.UsageHoldRefuser: why no spawn may start (and
// spawn_send may not reach one) now, or "" when the account is open.
func (a *Agent) UsageHoldRefusal() string {
	st := a.UsageHold()
	switch st.Phase {
	case usagehold.Hold:
		return "the usage hold is on (only keepalives and reminders run) until " + formatLift(st.LiftAt) + "."
	case usagehold.Stop:
		return "the usage limit is reached; nothing runs until " + formatLift(st.LiftAt) + "."
	}
	return ""
}

// UsageResumeLine is the hidden input a turn the usage hold interrupted gets
// at lift (Dick 2026-11-02: "simulate a user message saying \"resume what you
// were doing\" - do not show this in the clients").
const UsageResumeLine = tools.UsageResumeLine

// leadingUsageResume counts the hidden resume lines heading batch: a usage
// hold's, or a restart's (#2999, restart_resume.go). Both reach the model but
// no client.
func leadingUsageResume(batch []Envelope) int {
	n := 0
	for _, env := range batch {
		if !env.usageResume && !env.restartResume {
			break
		}
		n++
	}
	return n
}

type hiddenLeadingKey struct{}

// withHiddenLeadingTexts marks the first n texts of ctx's turn as hidden from
// clients (the usage hold's resume line).
func withHiddenLeadingTexts(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, hiddenLeadingKey{}, n)
}

func hiddenLeadingTexts(ctx context.Context) int {
	n, _ := ctx.Value(hiddenLeadingKey{}).(int)
	return n
}

// ErrUsageHeld is returned for a periodic turn the usage hold does not let
// start (usageHoldRefusesTurn).
var ErrUsageHeld = errors.New("held by the usage hold")

// usageHoldPeriodic are the periodic triggers that reach a backend outside
// the inbox in fork mode, so the orchestrator holds them itself.
var usageHoldPeriodic = map[string]bool{
	"reflection":                        true,
	delegator.BatchPurposeConsolidation: true,
	"background":                        true,
}

// usageHoldRefusesTurn is the orchestrator's backstop of the hold for turns
// that bypass the inbox (fork-mode keepalive, reflection, consolidation,
// background work): during a hold such a turn starts only with the hold's
// permit (usagehold.WithPermit), a resume of an interrupted background turn
// never meets it (it runs after the lift); during a stop no keepalive starts.
func (a *Agent) usageHoldRefusesTurn(ts *TurnState) error {
	phase := a.usageHoldPhase()
	switch {
	case phase == usagehold.Open:
		return nil
	case isKeepaliveTrigger(ts.Trigger):
		if phase == usagehold.Stop {
			return ErrUsageHeld
		}
		return nil
	case usageHoldPeriodic[ts.Trigger]:
		if phase == usagehold.Hold && usagehold.Permitted(ts.Ctx) {
			return nil
		}
		return ErrUsageHeld
	}
	return nil
}

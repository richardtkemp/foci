package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/account"
	"foci/internal/delegator"
	"foci/internal/tools"
	"foci/internal/usagehold"
)

// The account-wide usage hold, agent side (#2491, internal/usagehold).
//
// Every session of a delegated agent is on one account (#3186,
// Agent.AccountFor): a Claude Code login, or the one login of codex or
// opencode. The session's backend reports usage windows to that account
// (HostHooks.OnUsageWindow → Account.ObserveUsage); the account's hold
// decides its phase; every agent with the account in its pool sees each phase
// change (StartUsageHold's subscription) and:
//   - posts one notice in each of its chats whose session is on that account
//     (UsageHoldNotifyFunc over ChatSessionsFunc; Dick 2026-11-19, RULINGS 4
//     Q6: "Only chats whose session is on that account"): its default chat
//     hears nothing of a hold of an account its session is not on;
//   - while the account HOLDS, runs only keepalives and foci_remind fires
//     (scheduled_wake) on its sessions on that account: user messages, cron
//     sends, webhooks, agent messages and every other injection wait in the
//     inbox, and the periodic schedulers other than keepalive see the session
//     as rate-limited;
//   - while the account STOPS, runs nothing on them, keepalives and reminders
//     included;
//   - at lift, releases what waited: each session's held user messages run as
//     ONE turn (they are re-queued together, and the inbox batches them).

// usageHoldState is an agent's view of its accounts' holds.
type usageHoldState struct {
	// storeMu serialises read-modify-write of the stored held lists.
	storeMu sync.Mutex
	// minted names the held records this process stored before
	// RestoreUsageHeld read the store; heldRestored is set once it has
	// (both under storeMu). Such a record is already parked in memory, or
	// has run, so the startup restore must skip it (#3197: a queued message
	// the startup re-queue parked was restored a second time).
	minted       map[string]bool
	heldRestored bool

	mu sync.Mutex
	// unsubs end the subscriptions StartUsageHold made, one pair per account
	// of the pool (phase changes, limit resets); started is set once it ran.
	unsubs  []func()
	started bool
	// resetting is set while this agent's own ResetLimits clears an account,
	// so its reset subscriber leaves the work to ResetLimits (#3044).
	resetting atomic.Bool
	// changed is closed and replaced on every phase change of any account of
	// the pool, so an inbox worker can wait for the next one and then ask
	// about its own session again.
	changed chan struct{}
	// notices wait to be posted, in order; posting is true while a goroutine
	// posts them.
	notices []holdNotice
	posting bool
	// turns are the running turns a hold start interrupts through their
	// context (UsageHoldTurn); paused are the sessions it paused, by kind
	// (UsageHoldPause).
	turns  map[*holdTurn]bool
	paused map[string]string
	// heldBySession mirrors, per session, how much input the hold holds
	// (inb.usageHeld plus the injects it parked), for UsageHeldInput (#3112).
	// The lists are worker-confined; the mirror is what other goroutines read.
	mirrorMu      sync.Mutex
	heldBySession map[string]int
}

// Accounts is the agent's pool of accounts (nil for an API agent, or no
// agent).
func (a *Agent) Accounts() *account.Pool {
	if a == nil || a.DelegatedManager == nil {
		return nil
	}
	return a.DelegatedManager.Accounts
}

// AccountFor is the account sessionKey is on (#3186): the one its backend
// runs on, whose hold, rate-limit gate, login and files are the session's.
// Nil for a session of an agent with no account (an API agent); a nil
// account is always open and owes no login.
func (a *Agent) AccountFor(sessionKey string) *account.Account {
	return a.Accounts().For(sessionKey)
}

// UsageHold is the hold state of sessionKey's account now (Phase Open with
// no account).
func (a *Agent) UsageHold(sessionKey string) usagehold.State {
	return a.AccountFor(sessionKey).Hold()
}

// StartUsageHold subscribes the agent to the phase changes and limit resets
// of every account in its pool. Call it once the DelegatedManager is set; a
// no-op for an agent with no account.
func (a *Agent) StartUsageHold() {
	a.usageHold.mu.Lock()
	if a.usageHold.started {
		a.usageHold.mu.Unlock()
		return
	}
	a.usageHold.started = true
	for _, acct := range a.Accounts().Accounts() {
		a.usageHold.unsubs = append(a.usageHold.unsubs,
			acct.OnHoldChange(func(c usagehold.Change) { a.onUsageHoldChange(acct, c) }),
			acct.OnLimitReset(func() { a.onAccountLimitReset(acct) }))
	}
	a.usageHold.mu.Unlock()
	for _, acct := range a.Accounts().Accounts() {
		if st := acct.Hold(); st.Phase != usagehold.Open {
			a.logger().Infof("usage hold (%s) restored: %s until %s", acct.Key(), st.Phase, formatLift(st.LiftAt))
		}
	}
}

// StopUsageHold ends the subscriptions (agent stop).
func (a *Agent) StopUsageHold() {
	a.usageHold.mu.Lock()
	defer a.usageHold.mu.Unlock()
	for _, unsub := range a.usageHold.unsubs {
		unsub()
	}
	a.usageHold.unsubs = nil
	a.usageHold.started = false
}

// usageHoldChanged returns a channel closed at the next phase change of any
// account of the pool. A waiter then asks about its own session again.
func (a *Agent) usageHoldChanged() <-chan struct{} {
	a.usageHold.mu.Lock()
	defer a.usageHold.mu.Unlock()
	if a.usageHold.changed == nil {
		a.usageHold.changed = make(chan struct{})
	}
	return a.usageHold.changed
}

// onUsageHoldChange is the agent's reaction to one phase change of acct.
func (a *Agent) onUsageHoldChange(acct *account.Account, c usagehold.Change) {
	a.usageHold.mu.Lock()
	if a.usageHold.changed != nil {
		close(a.usageHold.changed)
	}
	a.usageHold.changed = make(chan struct{})
	a.usageHold.mu.Unlock()

	a.logger().Infof("usage hold (%s): %s -> %s (lifts %s)", c.Account, c.From, c.To.Phase, formatLift(c.To.LiftAt))
	if a.UsageHoldNotifyFunc != nil {
		a.queueUsageHoldNotice(holdNotice{acct: acct, text: acct.WithName(UsageHoldNotice(c))})
	}
	// The work running on the account at a hold start is interrupted or left
	// to finish (usage_hold_interrupt.go), before the reactions below: the
	// hold's compaction reads the paused set it records. Its backend
	// interrupts run off this goroutine, which may be that backend's reader.
	if c.From == usagehold.Open && c.To.Phase != usagehold.Open {
		a.interruptForUsageHold(acct)
	}
	for _, fn := range a.UsageHoldChangeFunc {
		fn(acct, c)
	}
}

// holdNotice is one hold notice waiting to be posted: its text, for the chats
// on acct.
type holdNotice struct {
	acct *account.Account
	text string
}

// postUsageHoldNotice posts n in every chat of the agent whose session is on
// n's account, once each.
func (a *Agent) postUsageHoldNotice(n holdNotice) {
	if a.ChatSessionsFunc == nil {
		return
	}
	seen := map[string]bool{}
	for _, sk := range a.ChatSessionsFunc() {
		if sk == "" || seen[sk] {
			continue
		}
		seen[sk] = true
		if a.AccountFor(sk) == n.acct {
			a.UsageHoldNotifyFunc(sk, n.text)
		}
	}
}

// queueUsageHoldNotice posts n off the caller's goroutine, in the order the
// changes happened: a start and its end must not swap. The chats are chosen
// there too, as asking a session's account may bind it (account.Pool), and
// binding asks the accounts' holds, whose change is being reported.
func (a *Agent) queueUsageHoldNotice(n holdNotice) {
	a.usageHold.mu.Lock()
	a.usageHold.notices = append(a.usageHold.notices, n)
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
			a.postUsageHoldNotice(next)
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

// usageHoldPhase is the phase of sessionKey's account now (Open with no
// account).
func (a *Agent) usageHoldPhase(sessionKey string) usagehold.Phase {
	return a.UsageHold(sessionKey).Phase
}

// usageHoldsTrigger reports whether a turn with trigger on sessionKey must
// wait now: during a hold everything but keepalives, foci_remind fires and
// the turns the hold itself starts (holdPermit); during a stop everything.
func (a *Agent) usageHoldsTrigger(sessionKey, trigger string, holdPermit bool) bool {
	return usageHoldsTriggerIn(a.usageHoldPhase(sessionKey), trigger, holdPermit)
}

// usageHoldsTriggerIn is usageHoldsTrigger for a given phase (a snapshot a
// caller reads once, #3121).
func usageHoldsTriggerIn(phase usagehold.Phase, trigger string, holdPermit bool) bool {
	switch phase {
	case usagehold.Hold:
		return !(isKeepaliveTrigger(trigger) || trigger == "scheduled_wake" || holdPermit)
	case usagehold.Stop:
		return true
	default:
		return false
	}
}

// UsageHoldBlocks is the periodic schedulers' view of sessionKey
// (BackgroundAgent): a non-keepalive scheduler may not fire on it during a
// hold or a stop of its account; keepalive may fire during a hold, not
// during a stop. reason names the hold.
func (a *Agent) UsageHoldBlocks(sessionKey string, keepalive bool) (blocked bool, reason string) {
	st := a.UsageHold(sessionKey)
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
	return env.Inject != nil && env.Inject.Trigger == "scheduled_wake" && a.usageHoldPhase(env.SessionKey) == usagehold.Hold
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
	inb.usageHeldMu.Lock()
	if restored {
		// A stored message is parked once (#3197): a second restored copy
		// of a parked id is dropped.
		for _, h := range inb.usageHeld {
			if h.usageHeldID == env.usageHeldID {
				inb.usageHeldMu.Unlock()
				a.logger().Infof("inbox: usage hold already holds %s sk=%s — duplicate dropped", env.usageHeldID, env.SessionKey)
				return
			}
		}
	}
	if env.usageResume {
		for _, h := range inb.usageHeld {
			if h.usageResume {
				inb.usageHeldMu.Unlock()
				return // one resume line per session
			}
		}
		inb.usageHeld = append([]Envelope{env}, inb.usageHeld...)
	} else {
		inb.usageHeld = append(inb.usageHeld, env)
	}
	n := len(inb.usageHeld)
	inb.usageHeldMu.Unlock()
	a.syncUsageHeldMirror(inb)
	st := a.UsageHold(inb.sk)
	a.logger().Infof("inbox: usage hold (%s) holds a message sk=%s (%dB, resume=%v, %d held)", st.Phase, env.SessionKey, len(env.Text), env.usageResume, n)
	if !env.Automated && !env.usageResume && !restored {
		a.notifyUsageHeld(env.SessionKey, st)
	}
}

// notifyUsageHeld tells sk's chat that the input it just sent waits for the
// usage hold's lift (a user message, parkUsageHeld; an ask answer or a
// redelivery, dispatchUserInject).
func (a *Agent) notifyUsageHeld(sk string, st usagehold.State) {
	if fn := a.TurnQueuedNotifyFunc; fn != nil {
		text := a.AccountFor(sk).WithName("⏸️ Held: usage is at the hold limit, so this message waits until " + formatLift(st.LiftAt) + ", then runs.")
		go fn(sk, text)
	}
}

// releaseUsageHeld runs the session's held input once the usage hold no
// longer holds it: all of it as ONE turn, in arrival order behind the hidden
// resume line when the hold interrupted a turn (the first starts the batch,
// the rest join it through takeUsageRelease). A resume line at the head of
// real messages takes their driver and sender, so the turn renders where they
// came from. Returns false when the worker must stop.
func (a *Agent) releaseUsageHeld(ctx context.Context, inb *sessionInbox) bool {
	if inb.usageHeldLen() == 0 || a.usageHoldsTrigger(inb.sk, "user", false) {
		return true
	}
	inb.usageHeldMu.Lock()
	held := inb.usageHeld
	inb.usageHeld = nil
	inb.usageHeldMu.Unlock()
	if len(held) == 0 {
		return true // a bare /lb took the last one meanwhile
	}
	a.syncUsageHeldMirror(inb)
	if held[0].usageResume && len(held) > 1 {
		head, src := &held[0], held[1]
		head.Driver, head.UserID, head.Username, head.SenderName = src.Driver, src.UserID, src.Username, src.SenderName
		head.ChatID, head.Original, head.Human = src.ChatID, src.Original, src.Human
	}
	if held[0].usageResume {
		// A session idle too long asks instead of resuming (#3155).
		if limit := a.AccountFor(inb.sk).HoldSettings().ResumeMaxIdle(); limit > 0 {
			if idle := usageResumeIdle(held, time.Now()); idle > limit {
				a.logger().Infof("inbox: usage hold lifted after %s idle on %s (limit %s): the agent asks the user instead of resuming", idle.Round(time.Second), inb.sk, limit)
				held[0].Text = usageResumeAskLine(idle)
			}
		}
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

// UsageHoldRefusal is tools.UsageHoldRefuser: why sessionKey may start no
// spawn that runs on its account (a clone, which copies its transcript), and
// spawn_send may not reach one of its spawns, now; "" when its account is
// open. sessionKey is one of this agent's sessions; a spawn as this agent
// from another agent asks about this agent's cross-agent root (#3220). Any
// other spawn of the agent's own runs on the pick and asks SpawnAccount
// (#3292). The reason names the account when it has a name (#3220 problem 4).
func (a *Agent) UsageHoldRefusal(sessionKey string) string {
	return accountHoldRefusal(a.AccountFor(sessionKey))
}

// accountHoldRefusal is why nothing but keepalives may start on acct now,
// "" when it is open (a nil account is).
func accountHoldRefusal(acct *account.Account) string {
	st := acct.Hold()
	switch st.Phase {
	case usagehold.Hold:
		return acct.WithName("the usage hold is on (only keepalives and reminders run) until "+formatLift(st.LiftAt)) + "."
	case usagehold.Stop:
		return acct.WithName("the usage limit is reached; nothing runs until "+formatLift(st.LiftAt)) + "."
	}
	return ""
}

var _ tools.SpawnAccountPicker = (*Agent)(nil)

// SpawnAccount is tools.SpawnAccountPicker (#3292): the account a new spawn
// of the agent's own that copies no transcript would run on now (the pool's
// pick, #3246, which passes over a held account while another is open), and
// why it may not start there ("" when the account is open). The hold of the
// spawner's own account plays no part (Dick 2026-11-27 16:14). When the pick
// holds, no account the spawn could run on is open: the reason then names
// every account of a pool of two or more, with why each cannot take it.
func (a *Agent) SpawnAccount() (*account.Account, string) {
	pool := a.Accounts()
	acct := pool.Next()
	msg := accountHoldRefusal(acct)
	if msg == "" {
		return acct, ""
	}
	accts := pool.Accounts()
	if len(accts) < 2 {
		return nil, msg
	}
	parts := make([]string, 0, len(accts))
	for _, x := range accts {
		why := accountHoldRefusal(x)
		if why == "" && x.Login().Pending() {
			why = x.WithName("it owes a login") + "."
		}
		if why == "" {
			continue // open now: a pick a moment later would take it
		}
		parts = append(parts, why)
	}
	if len(parts) == 0 {
		return nil, msg
	}
	return nil, "no account this spawn could run on is open: " + strings.Join(parts, " ")
}

// BindSpawnAccount is tools.SpawnAccountPicker: it puts sessionKey, a new
// spawn's own session, on acct (account.Pool.Assign).
func (a *Agent) BindSpawnAccount(sessionKey string, acct *account.Account) error {
	return a.Accounts().Assign(sessionKey, acct)
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
	phase := a.usageHoldPhase(ts.SessionKey)
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

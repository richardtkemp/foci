package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/delegator"
)

// awaitingHeldAgent is the standard gate-test fixture: a started agent whose
// mock backend reports pending background work, so every gated system inject
// parks. Release flips the gate open.
func awaitingHeldAgent(t *testing.T) (*Agent, *mockBackendDT, func()) {
	t.Helper()
	a, cancel := startedAgent(t)
	be := &mockBackendDT{sessionFile: "/tmp/s.jsonl"}
	be.setAwaiting(true)
	a.DelegatedManager = newMockDelegatedManager(t, be)
	return a, be, func() {
		be.setAwaiting(false)
		cancel()
	}
}

// gateBrancherBackend satisfies delegator.BackendBrancher (so a manager with
// branchSpec reports BackendCanBranch) on top of mockBackendDT's awaiting
// tracking, for gate tests that need both at once.
type gateBrancherBackend struct{ mockBackendDT }

func (b *gateBrancherBackend) ForkSession(context.Context, delegator.ForkRequest) (delegator.ForkResult, error) {
	return delegator.ForkResult{SessionID: "forked"}, nil
}

func (b *gateBrancherBackend) CleanupSession(context.Context, delegator.CleanupRequest) error {
	return nil
}

// newBranchCapableManager pre-registers be under "test/s" on a manager whose
// Spec declares CapBranch.
func newBranchCapableManager(t *testing.T, be delegator.Delegator) *DelegatedManager {
	t.Helper()
	mgr := &DelegatedManager{
		NewBackend: func() (delegator.Delegator, error) { return be, nil },
		Spec:       branchSpec,
	}
	if _, err := mgr.Get(context.Background(), "test/s"); err != nil {
		t.Fatalf("pre-register backend: %v", err)
	}
	return mgr
}

// TestInjectGate_KeepaliveNotHeld pins requirement 4's keepalive ruling: a
// keepalive is never held by background work — its whole purpose is keeping
// the prompt cache warm, so a two-hour hold defeats it.
func TestInjectGate_KeepaliveNotHeld(t *testing.T) {
	a, _, release := awaitingHeldAgent(t)
	defer release()

	sk := "test/s"
	var ran atomic.Int32
	a.Enqueue(Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: TriggerKeepalive,
		Run:     func() { ran.Add(1) },
	}})
	if !waitFor(500*time.Millisecond, func() bool { return ran.Load() == 1 }) {
		t.Fatal("keepalive was held behind background work — it must never be (#2233)")
	}
}

// TestInjectGate_InPlaceStaysHeld is the characterisation pin for the
// in-place side of the branch ruling: every inject shape that runs in the
// parent conversation keeps the hold — the plain in-place pass (what
// handleDelegatedBranch's injectInPlace enqueues for RunInPlace and the
// fork-unavailable fallback, trigger only) and a foci send on a
// branch-capable backend (the exemption is structural, not a property of the
// backend: only the branch machinery routes around the parent inbox).
// The branch side — a pass that runs on a branch is never held because it
// never reaches the parent inbox — is pinned at the real seam by
// cmd/foci-gw's TestBranchTurn_*UnderParentGate tests.
func TestInjectGate_InPlaceStaysHeld(t *testing.T) {
	t.Run("in-place reflection on non-forking backend", func(t *testing.T) {
		a, _, release := awaitingHeldAgent(t)
		defer release()

		var ran atomic.Int32
		a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{
			Trigger: "reflection", // what injectInPlace enqueues: the plain trigger
			Run:     func() { ran.Add(1) },
		}})
		if waitFor(300*time.Millisecond, func() bool { return ran.Load() == 1 }) {
			t.Fatal("an in-place inject ran during pending work — it touches the parent sink and keeps the hold")
		}
	})

	t.Run("force_in_session reflection on a branch-capable backend", func(t *testing.T) {
		a, cancel := startedAgent(t)
		defer cancel()
		be := &gateBrancherBackend{}
		be.sessionFile = "/tmp/s.jsonl"
		be.setAwaiting(true)
		a.DelegatedManager = newBranchCapableManager(t, be)
		// The operator override forces reflection in place even on a backend
		// that can fork: BranchStrategyFor is RunInPlace, so injectInPlace
		// runs on the parent and the gate applies.
		a.LiveConfigFn = func() *config.ResolvedAgentConfig {
			return &config.ResolvedAgentConfig{Reflection: config.ResolvedReflection{ForceInSession: true}}
		}

		var ran atomic.Int32
		a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{
			Trigger: "reflection",
			Run:     func() { ran.Add(1) },
		}})
		if waitFor(300*time.Millisecond, func() bool { return ran.Load() == 1 }) {
			t.Fatal("a force_in_session (RunInPlace) inject ran during pending work — it touches the parent sink and keeps the hold")
		}
	})

	t.Run("foci send on a branch-capable backend", func(t *testing.T) {
		a, cancel := startedAgent(t)
		defer cancel()
		be := &gateBrancherBackend{}
		be.sessionFile = "/tmp/s.jsonl"
		be.setAwaiting(true)
		a.DelegatedManager = newBranchCapableManager(t, be)

		var ran atomic.Int32
		a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{
			Trigger: "user", // a foci send must not become exempt just because the backend can branch
			Run:     func() { ran.Add(1) },
		}})
		if waitFor(300*time.Millisecond, func() bool { return ran.Load() == 1 }) {
			t.Fatal("a foci send on a branch-capable backend ran during pending work — only the branch machinery routes around the parent inbox")
		}
	})
}

// TestInjectGate_HeldInjectsCharacterisation is spec case (d): the real
// system inputs stay held across the background-work window — foci send
// (user/external), wakes, ask expiry (ask_grader without the user-input
// marker), send_to_session, webhooks.
func TestInjectGate_HeldInjectsCharacterisation(t *testing.T) {
	for _, trigger := range []string{"user", "external", "scheduled_wake", "ask_grader", "session_notify", "webhook"} {
		t.Run(trigger, func(t *testing.T) {
			a, _, release := awaitingHeldAgent(t)
			defer release()

			var ran atomic.Int32
			a.Enqueue(Envelope{SessionKey: "test/s", Inject: &InjectMeta{
				Trigger: trigger,
				Run:     func() { ran.Add(1) },
			}})
			// Held: it parks in the gate (a synchronous observable, no fixed
			// wait) and has not run.
			if !waitFor(2*time.Second, func() bool { return gateWatcherParked(a, "test/s") }) {
				t.Fatalf("%s inject never parked in the gate during pending work", trigger)
			}
			if ran.Load() != 0 {
				t.Fatalf("%s inject ran during pending work — system inputs keep the hold (#2233 ruling)", trigger)
			}
		})
	}
}

// TestSystemInputHeld covers the shared visibility predicate (#2233): the
// pending window, an adopted delivering run with an idle worker, the busy
// exclusion for a turn the worker itself runs, and the API-agent negative.
func TestSystemInputHeld(t *testing.T) {
	t.Run("pending work", func(t *testing.T) {
		a, _, release := awaitingHeldAgent(t)
		defer release()
		if !a.SystemInputHeld("test/s") {
			t.Fatal("SystemInputHeld = false while the backend reports pending work")
		}
	})

	t.Run("adopted run, idle worker", func(t *testing.T) {
		a, cancel := startedAgent(t)
		defer cancel()
		release := a.markInFlight("test/s", true)
		defer release()
		if !a.SystemInputHeld("test/s") {
			t.Fatal("SystemInputHeld = false with an adopted delivering run in flight")
		}
	})

	t.Run("worker busy excludes its own turn", func(t *testing.T) {
		a, cancel := startedAgent(t)
		defer cancel()
		release := a.markInFlight("test/s", true)
		defer release()
		inb := a.getOrCreateInbox("test/s")
		inb.busy.Add(1)
		if a.SystemInputHeld("test/s") {
			t.Fatal("SystemInputHeld = true while the worker runs its own turn — a user turn is never a hold (#2233)")
		}
		inb.busy.Add(-1)
	})

	t.Run("API agent idle", func(t *testing.T) {
		a, cancel := startedAgent(t)
		defer cancel()
		if a.SystemInputHeld("test/s") {
			t.Fatal("SystemInputHeld = true for an idle API agent")
		}
	})
}

// TestInjectGateApplies unit-pins the classification itself, including the
// pieces the driven tests above cannot reach (nil meta, user input).
func TestInjectGateApplies(t *testing.T) {
	if injectGateApplies(nil) {
		t.Error("nil meta must not be gated")
	}
	if injectGateApplies(&InjectMeta{UserInput: true}) {
		t.Error("a user-input inject must never be gated")
	}
	if !injectGateApplies(&InjectMeta{Trigger: "user"}) {
		t.Error("a foci send inject keeps the gate")
	}
	if !injectGateApplies(&InjectMeta{Trigger: TriggerKeepalive}) {
		t.Error("the gate must be able to park a keepalive (behind an adopted run or parked siblings) — its exemption is from the pending-work half only, and lives in injectHeld (#2233 review F1)")
	}
}

// TestInjectHeld_KeepaliveHalfSplit pins the keepalive half-split at the
// decision level (#2233 review F1): pending work alone never holds a
// keepalive; an adopted delivering run DOES (parking there is what keeps the
// worker free where the backend's turnActive refusal would otherwise block it
// in RunInference's retry loop); the busy refinement hides a turn the worker
// drives itself; a plain system inject keeps the full hold; and a user-input
// injection is never held at all.
func TestInjectHeld_KeepaliveHalfSplit(t *testing.T) {
	ka := Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: TriggerKeepalive}}

	t.Run("pending work alone never holds a keepalive", func(t *testing.T) {
		a, _, release := awaitingHeldAgent(t)
		defer release()
		if a.injectHeld(ka) {
			t.Fatal("injectHeld(keepalive) = true under pending work alone — background work must never hold a keepalive (#2233)")
		}
	})

	t.Run("an adopted delivering run holds it", func(t *testing.T) {
		a, cancel := startedAgent(t)
		defer cancel()
		release := a.markInFlight("test/s", true)
		defer release()
		if !a.injectHeld(ka) {
			t.Fatal("injectHeld(keepalive) = false during an adopted delivering run — it must park there, not block the worker in the turnActive retry loop (review F1)")
		}
	})

	t.Run("busy refinement hides a turn the worker drives", func(t *testing.T) {
		a, cancel := startedAgent(t)
		defer cancel()
		release := a.markInFlight("test/s", true)
		defer release()
		inb := a.getOrCreateInbox("test/s")
		inb.busy.Add(1)
		defer inb.busy.Add(-1)
		if a.injectHeld(ka) {
			t.Fatal("injectHeld(keepalive) = true while the worker drives its own turn — the busy refinement must hide the adopted run")
		}
	})

	t.Run("a plain system inject keeps the full hold", func(t *testing.T) {
		a, _, release := awaitingHeldAgent(t)
		defer release()
		if !a.injectHeld(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "reflection"}}) {
			t.Fatal("injectHeld(reflection) = false under pending work — the hold on real system inputs is unchanged")
		}
	})

	t.Run("a user-input injection is never held", func(t *testing.T) {
		a, _, release := awaitingHeldAgent(t)
		defer release()
		releaseRun := a.markInFlight("test/s", true)
		defer releaseRun()
		if a.injectHeld(Envelope{SessionKey: "test/s", Inject: &InjectMeta{Trigger: "ask_grader", UserInput: true}}) {
			t.Fatal("injectHeld(user-input) = true under both hold halves — an ask answer takes the user path and is never held (#2233)")
		}
	})
}

// TestRunInject_BusyBracketsTheInjectedTurn drives the busy counter through a
// real injected turn: while the worker runs an injection that carries its own
// delivering registration (a scheduled wake's system turn), the session must
// not read as held — a turn the worker itself drives is "waiting to run",
// never "held" — and once the turn ends the worker reads idle again, so a
// delivering run still registered does read as held. A missing bracket fails
// both halves: the counter reads idle mid-turn (the wake's own turn would
// report itself held) and drifts negative after it (the worker would never
// read idle again, masking every later adopted run).
func TestRunInject_BusyBracketsTheInjectedTurn(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	sk := "test/s"
	inb := a.getOrCreateInbox(sk)

	// The delivering registration the injected turn itself holds while it
	// runs, kept open past the turn's end so both halves of the refinement
	// are observable.
	release := a.markInFlight(sk, true)
	defer release()

	var duringHeld atomic.Bool
	a.runInject(inb, Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "scheduled_wake",
		Run: func() {
			duringHeld.Store(a.SystemInputHeld(sk))
		},
	}})

	if duringHeld.Load() {
		t.Fatal("SystemInputHeld = true while the worker runs the injected turn itself — a turn the worker drives is never a hold (a fired wake's own turn reads waiting, not held)")
	}
	if !a.SystemInputHeld(sk) {
		t.Fatal("SystemInputHeld = false after the injected turn ended with the delivering run still registered — the worker must read idle between turns")
	}
}

// TestDriveOnce_BusyBracketsTheUserTurn is the platform-turn twin of
// TestRunInject_BusyBracketsTheInjectedTurn: while the worker drives a user
// turn under an adopted delivering run's registration, the session must not
// read as held — a user turn is never a pending-work hold, so a wake listed
// or an async send receipted at that moment reads waiting/queued, not held —
// and once the turn ends the worker reads idle again, so the still-registered
// run does read as held. A missing bracket fails the mid-turn half (the user
// turn would report itself a hold).
func TestDriveOnce_BusyBracketsTheUserTurn(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	sk := "test/s"
	inb := a.getOrCreateInbox(sk)

	// The delivering registration an adopted run holds open across the user
	// turn, kept open past the turn's end so both halves of the refinement
	// are observable.
	release := a.markInFlight(sk, true)
	defer release()
	if !a.SystemInputHeld(sk) {
		t.Fatal("fixture: the delivering run must read as held before the turn")
	}

	// The turn observer fires inside driveOnce's busy bracket — the exact
	// window whose reporting the bracket exists to keep honest.
	var during atomic.Bool
	a.SetTurnObserver(func(sessionKey string, _ []Envelope) {
		if sessionKey == sk {
			during.Store(a.SystemInputHeld(sk))
		}
	})
	d := &recordingDriver{}
	a.driveOnce(context.Background(), inb, []Envelope{{SessionKey: sk, Text: "hello", Driver: d}}, nil, a.sessionRouter(sk), d)

	if during.Load() {
		t.Fatal("SystemInputHeld = true while the worker drives the user turn itself — a user turn is never a hold (a wake listed or a send receipted mid-turn reads waiting, not held)")
	}
	if !a.SystemInputHeld(sk) {
		t.Fatal("SystemInputHeld = false after the user turn ended with the delivering run still registered — the worker must read idle between turns")
	}
}

// TestRunInject_PanicInRunKeepsBusyBracket pins the panic half of the busy
// bracket: runInject recovers a panicking Run, and the deferred decrement
// must still run — a counter stuck at 1 makes the worker read busy forever,
// so holdActive's busy refinement never sees an adopted delivering run as a
// hold again and system injects would run during it (#1068, #2233 F4).
func TestRunInject_PanicInRunKeepsBusyBracket(t *testing.T) {
	a, cancel := startedAgent(t)
	defer cancel()
	sk := "test/s"
	inb := a.getOrCreateInbox(sk)

	a.runInject(inb, Envelope{SessionKey: sk, Inject: &InjectMeta{
		Trigger: "reflection",
		Run:     func() { panic("boom") },
	}})
	if got := inb.busy.Load(); got != 0 {
		t.Fatalf("busy = %d after a recovered inject panic, want 0 — the bracket leaked and the worker never reads idle again", got)
	}

	// The gate consequence: with the counter back at zero, an adopted
	// delivering run still holds a later system inject.
	release := a.markInFlight(sk, true)
	defer release()
	if !a.SystemInputHeld(sk) {
		t.Fatal("SystemInputHeld = false with an adopted delivering run after a recovered panic — the leaked busy bracket hid the hold")
	}
}

package agent

import (
	"context"
	"testing"

	"foci/internal/delegator"
	"foci/internal/platform"
)

// noSteerAgent is an agent (inbox not started, so the session channel stays
// observable) on a delegated backend whose Spec declares mid_turn_steer as
// given, with a turn in flight on "test/s".
func noSteerAgent(t *testing.T, steer delegator.Support) (*Agent, *recordingBackend, *sessionInbox) {
	t.Helper()
	be := &recordingBackend{}
	be.turnInFlight = true
	mgr := newMockDelegatedManager(t, be)
	mgr.Spec.Caps[delegator.CapMidTurnSteer] = steer
	a := newTestAgent(t)
	a.DelegatedManager = mgr
	a.SetInboxSteerMode(true)
	a.SetInboxBackend(func(context.Context, string) (delegator.Delegator, error) { return be, nil })
	inb := a.getOrCreateInbox("test/s")
	inb.turnActive.Store(true)
	return a, be, inb
}

// #3115: on a backend with no mid-turn steer (opencode), a message that
// arrives mid-turn is never steered, whatever steer_mode or the message's own
// preference says. It waits in the session queue and runs as the next turn,
// in arrival order.
func TestInbox_Enqueue_NoMidTurnSteer_QueuesInOrder(t *testing.T) {
	t.Parallel()
	a, be, inb := noSteerAgent(t, delegator.No("no mid-turn input"))

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "first"})
	a.Enqueue(Envelope{SessionKey: "test/s", Text: "second", Steer: platform.SteerAlways})

	if injects := be.Injects(); len(injects) != 0 {
		t.Errorf("mid-turn messages reached the backend: %+v", injects)
	}
	if entries := inb.drainSteer(); len(entries) != 0 {
		t.Errorf("mid-turn messages landed in the steer buffer (%d entries)", len(entries))
	}
	for _, want := range []string{"first", "second"} {
		select {
		case env := <-inb.ch:
			if env.Text != want {
				t.Errorf("queued text = %q, want %q", env.Text, want)
			}
		default:
			t.Fatalf("%q was not queued to the session channel", want)
		}
	}
}

// Control for the test above: the same agent on a backend that declares
// mid_turn_steer Yes steers the message.
func TestInbox_Enqueue_MidTurnSteer_Steers(t *testing.T) {
	t.Parallel()
	a, be, _ := noSteerAgent(t, delegator.Yes())

	a.Enqueue(Envelope{SessionKey: "test/s", Text: "fold me"})

	injects := be.Injects()
	if len(injects) != 1 || injects[0].Source != delegator.SourceSteer {
		t.Fatalf("injects = %+v, want one SourceSteer", injects)
	}
}

// spawn_send into a running spawn turn (SteerTurn) is refused on a backend
// with no mid-turn steer, so the caller queues the text for the spawn's next
// turn; with the capability it folds.
func TestSteerTurn_FollowsMidTurnSteerCapability(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		steer delegator.Support
		want  bool
	}{
		{"no", delegator.No("no mid-turn input"), false},
		{"yes", delegator.Yes(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, be, _ := noSteerAgent(t, tc.steer)
			if got := a.SteerTurn(context.Background(), "test/s", "hi"); got != tc.want {
				t.Errorf("SteerTurn = %v, want %v", got, tc.want)
			}
			if n := len(be.Injects()); (n == 1) != tc.want {
				t.Errorf("backend injects = %d, want steered=%v", n, tc.want)
			}
		})
	}
}

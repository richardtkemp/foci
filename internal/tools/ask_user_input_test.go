package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/log"
	"foci/internal/question"
)

// userRouteFixture is the ask fixture with BOTH delivery routes wired the way
// production wires them (#2233): deliver (system route) and deliverUser (the
// user-input route), each recorded separately.
type userRouteFixture struct {
	present *fakePresenter
	system  *fakeDeliver
	user    *fakeDeliver
	tool    *Tool
	router  *AskRouter
}

func newUserRouteFixture(opts ...AskOption) *userRouteFixture {
	f := &userRouteFixture{
		present: &fakePresenter{},
		system:  &fakeDeliver{},
		user:    &fakeDeliver{},
	}
	f.tool, f.router = NewAskTool(f.present.present, nil, f.system.deliver, nil, nil, "test",
		append([]AskOption{WithUserInputDeliver(f.user.deliver)}, opts...)...)
	return f
}

// singleQuestionAsk starts a one-option ask and returns its request id.
func singleQuestionAsk(t *testing.T, f *userRouteFixture) string {
	t.Helper()
	res := execAsk(t, f.tool, `{"questions":[{"question":"pick","header":"Pick","options":[{"label":"A"},{"label":"B"}]}]}`)
	var out struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("parse tool result: %v", err)
	}
	return out.RequestID
}

// TestAskDelivery_UserInputKinds pins requirement 2's user half: every ask
// message that carries the user's own action routes through the user-input
// delivery, not the system one.
func TestAskDelivery_UserInputKinds(t *testing.T) {
	t.Run("full batch raw", func(t *testing.T) {
		f := newUserRouteFixture()
		reqID := singleQuestionAsk(t, f)
		f.present.answer("qa:0")

		if msg, ok := f.user.last(); !ok || !strings.Contains(msg, "A") {
			t.Fatalf("user route = %v, want the raw answer batch", f.user.messages)
		}
		if f.user.lastReqID() != reqID {
			t.Errorf("user route reqID = %q, want %q", f.user.lastReqID(), reqID)
		}
		if len(f.system.messages) != 0 {
			t.Errorf("answer batch also hit the system route: %v", f.system.messages)
		}
	})

	t.Run("full batch graded", func(t *testing.T) {
		// A grader that fails at run time still delivers (fallback) — through
		// the user route, because the message carries the user's answers.
		grader := filepath.Join(t.TempDir(), "grader.sh")
		os.WriteFile(grader, []byte("#!/bin/sh\nexit 3\n"), 0o755)
		f := newUserRouteFixture()
		res := execAsk(t, f.tool, `{"questions":[{"question":"pick","options":[{"label":"A"}]}],"grader":"`+grader+`"}`)
		var out struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("parse tool result: %v", err)
		}
		f.present.answer("qa:0")

		if !eventuallyDelivered(f.user, func(msg string) bool {
			return strings.Contains(msg, "grader could not run")
		}) {
			t.Fatalf("user route = %v, want the graded(fallback) batch", f.user.messages)
		}
		if len(f.system.messages) != 0 {
			t.Errorf("graded batch also hit the system route: %v", f.system.messages)
		}
	})

	t.Run("partial /complete batch", func(t *testing.T) {
		f := newUserRouteFixture()
		execAsk(t, f.tool, `{"questions":[{"question":"one","options":[{"label":"A"}]},{"question":"two","options":[{"label":"B"}]}]}`)
		f.present.answer("qa:0")
		if _, _, ok := f.router.CompleteSession(askSession); !ok {
			t.Fatal("CompleteSession failed")
		}

		if msg, ok := f.user.last(); !ok || !strings.Contains(msg, "completed your `ask` request early") {
			t.Fatalf("user route = %v, want the partial batch", f.user.messages)
		}
		if len(f.system.messages) != 0 {
			t.Errorf("partial batch also hit the system route: %v", f.system.messages)
		}
	})

	t.Run("partial /complete batch graded", func(t *testing.T) {
		// /complete with a grader configured: the graded partial set (here
		// the grader's run-time fallback) is still the user's own answers,
		// so it takes the user route too.
		grader := filepath.Join(t.TempDir(), "grader.sh")
		os.WriteFile(grader, []byte("#!/bin/sh\nexit 3\n"), 0o755)
		f := newUserRouteFixture()
		execAsk(t, f.tool, `{"questions":[{"question":"one","options":[{"label":"A"}]},{"question":"two","options":[{"label":"B"}]}],"grader":"`+grader+`"}`)
		f.present.answer("qa:0")
		if _, _, ok := f.router.CompleteSession(askSession); !ok {
			t.Fatal("CompleteSession failed")
		}

		if !eventuallyDelivered(f.user, func(msg string) bool {
			return strings.Contains(msg, "grader could not run") && strings.Contains(msg, "completed your `ask` request early")
		}) {
			t.Fatalf("user route = %v, want the graded(fallback) partial batch", f.user.messages)
		}
		if len(f.system.messages) != 0 {
			t.Errorf("graded partial batch also hit the system route: %v", f.system.messages)
		}
	})

	t.Run("late answer", func(t *testing.T) {
		f := newUserRouteFixture()
		reqID := singleQuestionAsk(t, f)
		f.present.answer("qa:0") // resolves it; the ask is gone

		ok := f.router.DeliverLateAnswer(LateAnswer{
			PromptID:   reqID + "-q0",
			SessionKey: askSession,
			AskedAt:    time.Now().Add(-time.Hour),
			Questions:  []question.Question{{Question: "pick", Options: []question.Option{{Label: "A"}}}},
			Answers:    []string{"late choice"},
		})
		if !ok {
			t.Fatal("late answer not delivered")
		}
		if msg, ok := f.user.last(); !ok || !strings.Contains(msg, "LATE ANSWER") {
			t.Fatalf("user route = %v, want the late answer", f.user.messages)
		}
		if len(f.system.messages) != 0 {
			t.Errorf("late answer also hit the system route: %v", f.system.messages)
		}
	})

	t.Run("user cancel notice", func(t *testing.T) {
		f := newUserRouteFixture()
		singleQuestionAsk(t, f)
		f.present.answer("qa:cancel")

		if msg, ok := f.user.last(); !ok || !strings.Contains(msg, "CANCELLED") {
			t.Fatalf("user route = %v, want the cancel notice", f.user.messages)
		}
		if len(f.system.messages) != 0 {
			t.Errorf("cancel notice also hit the system route: %v", f.system.messages)
		}
	})
}

// TestAskDelivery_SystemKinds pins requirement 2's system half (the
// characterisation side): expiry and restore notices — messages the user did
// not cause — keep the system route.
func TestAskDelivery_SystemKinds(t *testing.T) {
	t.Run("live expiry", func(t *testing.T) {
		f := newUserRouteFixture()
		singleQuestionAsk(t, f)
		f.present.answer(question.ExpiredData)

		if msg, ok := f.system.last(); !ok || !strings.Contains(msg, "expired unanswered") {
			t.Fatalf("system route = %v, want the expiry notice", f.system.messages)
		}
		if len(f.user.messages) != 0 {
			t.Errorf("expiry notice hit the user route: %v", f.user.messages)
		}
	})

	t.Run("queued ask expiring at promotion", func(t *testing.T) {
		f := newUserRouteFixture()
		s := f.state()
		qs := []question.Question{{Question: "pick", Options: []question.Option{{Label: "A"}}}}
		primary, _ := s.start(askSession, qs, nil, graderConfig{})
		s.start(askSession, qs, nil, graderConfig{}) // queued behind the primary

		// Age the queued ask past the TTL, then resolve the primary: the
		// promotion drops the stale ask with a SYSTEM notice.
		stale := time.Now().Add(-25 * time.Hour)
		s.mu.Lock()
		for _, p := range s.byReqID {
			if p.queued {
				p.createdAt = stale
			}
		}
		s.mu.Unlock()
		s.handleResponse(primary, "qa:0")

		// The primary's own ANSWER is user input (correct); only the stale
		// queue entry's expiry notice must stay system.
		if !eventuallyDelivered(f.system, func(msg string) bool {
			return strings.Contains(msg, "NEVER shown to the user")
		}) {
			t.Fatalf("system route = %v, want the queued-expired notice", f.system.messages)
		}
		// Wait for the primary's answer batch on the user route, then check
		// the notice is not there too. Promotion (and so the notice) runs
		// before the answer batch is delivered (resolveLocked, then
		// deliverBatch), so once the batch is in, the user route has had
		// every delivery this response makes. Polling for the notice's
		// ABSENCE instead cost the full 2s poll bound on every pass (#2539).
		if !eventuallyDelivered(f.user, func(msg string) bool {
			return strings.Contains(msg, "the user answered")
		}) {
			t.Fatalf("user route = %v, want the primary's answer batch", f.user.messages)
		}
		if delivered(f.user, func(msg string) bool {
			return strings.Contains(msg, "NEVER shown to the user")
		}) {
			t.Errorf("queued-expired notice hit the user route: %v", f.user.messages)
		}
	})

	t.Run("restore notices replay as system", func(t *testing.T) {
		// A restore-time expiry parks its notice during restoring; the replay
		// keeps the system route.
		f := newUserRouteFixture()
		s := f.state()
		s.mu.Lock()
		s.restoring = true
		s.mu.Unlock()
		s.deliverMsg(deliverSystem, askSession, "req-x", "[SYSTEM: expired]")
		s.mu.Lock()
		s.restoring = false
		s.mu.Unlock()
		s.deliverRestoreNotices()

		if msg, ok := f.system.last(); !ok || !strings.Contains(msg, "expired") {
			t.Fatalf("system route = %v, want the restored notice", f.system.messages)
		}
		if len(f.user.messages) != 0 {
			t.Errorf("restored notice hit the user route: %v", f.user.messages)
		}
	})
}

// TestAskDelivery_RestoreHeldNoticeKeepsKind pins that a message parked during
// restoring replays with the kind it was parked with: a user-kind notice
// (cancel) replays through the user route, not the system one.
func TestAskDelivery_RestoreHeldNoticeKeepsKind(t *testing.T) {
	f := newUserRouteFixture()
	s := f.state()
	s.mu.Lock()
	s.restoring = true
	s.mu.Unlock()
	s.deliverMsg(deliverUserInput, askSession, "req-x", "CANCELLED by the user")
	s.mu.Lock()
	s.restoring = false
	s.mu.Unlock()
	s.deliverRestoreNotices()

	if msg, ok := f.user.last(); !ok || !strings.Contains(msg, "CANCELLED") {
		t.Fatalf("user route = %v, want the parked user-kind notice replayed as user input", f.user.messages)
	}
	if len(f.system.messages) != 0 {
		t.Errorf("user-kind notice replayed through the system route: %v", f.system.messages)
	}
}

// TestAskDelivery_FallsBackToDeliverWhenNoUserRoute is the characterisation
// of the compatibility route every existing NewAskTool caller relies on: with
// no WithUserInputDeliver wired, user-kind messages fall back to `deliver` —
// and the degradation must be LOUD (a WARN on the ask component), not a
// silent route change: a gateway that wired only `deliver` would otherwise
// quietly hold the user's answers behind background work again.
func TestAskDelivery_FallsBackToDeliverWhenNoUserRoute(t *testing.T) {
	var mu sync.Mutex
	var warned []string
	remove := log.AddTap(log.WARN, func(_ log.Level, component, msg string) {
		if component != "ask" {
			return
		}
		mu.Lock()
		warned = append(warned, msg)
		mu.Unlock()
	})
	defer remove()

	p := &fakePresenter{}
	d := &fakeDeliver{}
	tool, _ := NewAskTool(p.present, nil, d.deliver, nil, nil, "test")
	res := execAsk(t, tool, `{"questions":[{"question":"pick","options":[{"label":"A"}]}]}`)
	var out struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("parse tool result: %v", err)
	}
	p.answer("qa:0")

	if _, ok := d.last(); !ok {
		t.Fatal("answer batch was not delivered through the fallback system route")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warned) == 0 {
		t.Fatal("the fallback to the system route passed silently — a mis-wired gateway would quietly hold the user's answers behind background work")
	}
}

// eventuallyDelivered waits (bounded by asyncDeliveryWait, see ask_test.go)
// for a delivery matching want — the graded goroutine delivers asynchronously.
func eventuallyDelivered(d *fakeDeliver, want func(string) bool) bool {
	deadline := time.Now().Add(asyncDeliveryWait)
	for time.Now().Before(deadline) {
		if delivered(d, want) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// delivered reports whether d has already received a delivery matching want.
func delivered(d *fakeDeliver, want func(string) bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range d.messages {
		if want(m) {
			return true
		}
	}
	return false
}

// state builds the fixture's ask state directly (bypassing NewAskTool's tool
// wrapper) for the subtests that manipulate internal state.
func (f *userRouteFixture) state() *askState {
	s := newAskState(f.present.present, nil, f.system.deliver, nil, nil, "test")
	s.deliverUser = f.user.deliver
	return s
}

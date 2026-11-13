package secretsapprove

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"foci/internal/log"
)

var logger = log.NewComponentLogger("secretsapprove")

// DefaultTimeout is how long a request waits for the user's answer. Then the
// dialog closes and the agent hears timed_out.
const DefaultTimeout = 30 * time.Minute

// The dialog's answers (Answer.Decision). ChoiceApprove carries the typed
// value for a set; ChoiceCancel is the value step's Cancel.
const (
	ChoiceApprove = "approve"
	ChoiceDeny    = "deny"
	ChoiceCancel  = "cancel"
)

// Outcome is how a request ended.
type Outcome string

// Outcomes. Every submitted request ends in exactly one.
const (
	Applied   Outcome = "applied"
	Denied    Outcome = "denied"
	Cancelled Outcome = "cancelled"
	TimedOut  Outcome = "timed_out"
	Failed    Outcome = "failed"
)

// Result is a request's end, as the agent hears it. Message never holds a
// secret value.
type Result struct {
	ID      string
	Request Request
	Outcome Outcome
	Message string
}

// Answer is the user's reply from the dialog. Value is set only with
// ChoiceApprove on a set.
type Answer struct {
	Decision string
	Value    string
}

// Prompt is what Present shows. OnAnswer may be called at most once that
// counts; later calls are ignored.
type Prompt struct {
	ID         string
	Header     string
	Text       string
	NeedsValue bool   // a set: Approve leads to a masked value field
	ValueLabel string // the field's label, e.g. the secret's name
	OnAnswer   func(Answer)
}

// Deps wires the service to the gateway.
type Deps struct {
	Store Store
	// KnownAgent reports whether id is a configured agent; an agent rule
	// naming any other id is refused before the user is asked.
	KnownAgent func(id string) bool
	// Present shows the prompt in the foci app. An error means no app device
	// can show it: the request fails at once (Dick 2026-11-13: app only).
	Present func(Prompt) error
	// Close withdraws a prompt that timed out from every device.
	Close func(id string)
	// Timeout overrides DefaultTimeout (tests).
	Timeout time.Duration
}

// Service holds the pending requests.
type Service struct {
	d       Deps
	mu      sync.Mutex
	pending map[string]*pending
}

type pending struct {
	agentID string
	req     Request
	done    func(Result)
	timer   *time.Timer
}

// New builds the service.
func New(d Deps) *Service {
	if d.Timeout <= 0 {
		d.Timeout = DefaultTimeout
	}
	return &Service{d: d, pending: make(map[string]*pending)}
}

// Submit asks the user about req on agentID's behalf. An error means nothing
// was asked (a bad request, or no app device). Otherwise the request is
// pending, and done is called exactly once with its outcome, from another
// goroutine (the dialog's answer, or the timeout).
func (s *Service) Submit(agentID string, req Request, done func(Result)) (string, error) {
	if s == nil || s.d.Store == nil {
		return "", errors.New("secret requests are not available on this gateway")
	}
	if s.d.KnownAgent != nil {
		for _, a := range req.agentIDs() {
			if !s.d.KnownAgent(a) {
				return "", fmt.Errorf("unknown agent %q: an agent rule may name only configured agents", a)
			}
		}
	}
	if err := precheck(s.d.Store, req); err != nil {
		return "", err
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	header, text := describe(s.d.Store, agentID, req)
	p := &pending{agentID: agentID, req: req, done: done}
	s.mu.Lock()
	s.pending[id] = p
	s.mu.Unlock()

	prompt := Prompt{ID: id, Header: header, Text: text, NeedsValue: req.NeedsValue(), ValueLabel: req.Name,
		OnAnswer: func(a Answer) { s.answer(id, a) }}
	if err := s.d.Present(prompt); err != nil {
		s.take(id)
		logger.Warnf("secret request %s from %s (%s): not shown: %v", id, agentID, req, err)
		return "", err
	}
	s.mu.Lock()
	if _, ok := s.pending[id]; ok { // not answered synchronously
		p.timer = time.AfterFunc(s.d.Timeout, func() { s.expire(id) })
	}
	s.mu.Unlock()
	logger.Infof("secret request %s from %s: %s (waiting for the user)", id, agentID, req)
	return id, nil
}

// take removes id and returns it, or nil when it already ended. Only the
// caller that gets it non-nil may finish it.
func (s *Service) take(id string) *pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[id]
	delete(s.pending, id)
	if p != nil && p.timer != nil {
		p.timer.Stop()
	}
	return p
}

// answer applies the user's answer. The value is checked here and never
// logged; a bad value fails the request (the agent may ask again).
func (s *Service) answer(id string, a Answer) {
	p := s.take(id)
	if p == nil {
		return
	}
	res := Result{ID: id, Request: p.req}
	switch {
	case a.Decision == ChoiceDeny:
		res.Outcome, res.Message = Denied, "The user denied the request."
	case a.Decision == ChoiceCancel:
		res.Outcome, res.Message = Cancelled, "The user cancelled the request at the value step: nothing changed."
	case a.Decision == ChoiceApprove:
		value := ""
		if p.req.NeedsValue() {
			v, err := CheckValue(a.Value)
			if err != nil {
				res.Outcome, res.Message = Failed, "The user approved, but "+err.Error()+": nothing changed. Ask again if it is still needed."
				break
			}
			value = v
		}
		msg, err := apply(s.d.Store, p.req, value)
		if err != nil {
			res.Outcome, res.Message = Failed, "The user approved, but the change failed: "+err.Error()
		} else {
			res.Outcome, res.Message = Applied, msg
		}
	default:
		res.Outcome, res.Message = Failed, fmt.Sprintf("unknown answer %q", a.Decision)
	}
	s.finish(p, res)
}

// expire ends id as timed_out and closes its dialog.
func (s *Service) expire(id string) {
	p := s.take(id)
	if p == nil {
		return
	}
	if s.d.Close != nil {
		s.d.Close(id)
	}
	s.finish(p, Result{ID: id, Request: p.req, Outcome: TimedOut,
		Message: fmt.Sprintf("The user did not answer within %s: nothing changed.", s.d.Timeout)})
}

func (s *Service) finish(p *pending, res Result) {
	if res.Outcome == Failed {
		logger.Warnf("secret request %s from %s (%s): %s: %s", res.ID, p.agentID, p.req, res.Outcome, res.Message)
	} else {
		logger.Infof("secret request %s from %s (%s): %s", res.ID, p.agentID, p.req, res.Outcome)
	}
	if p.done != nil {
		p.done(res)
	}
}

// Pending reports how many requests wait for an answer (tests, diagnostics).
func (s *Service) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "secrets-" + hex.EncodeToString(b[:]), nil
}

package askgw

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type connWriter interface {
	WriteFrame(v any) error
	Close() error
}

type entry struct {
	connID     uint64
	askID      string
	agentID    string
	sessionKey string
	w          connWriter
	questions  []AskQuestion
	current    int
	answers    map[string]json.RawMessage
	msgID      string
	timer      *time.Timer
	cancelFn   func()

	// platformMsgID is the platform-native message ID of the most recently
	// presented question (overwritten as a multi-question ask advances), set
	// via SetPresentedMsgID once PresentFn returns it. It is the message a
	// later `notify` frame edits in place — see recordAnswered.
	platformMsgID string
}

func (e *entry) currentQuestion() *AskQuestion {
	if e.current < 0 || e.current >= len(e.questions) {
		return nil
	}
	return &e.questions[e.current]
}

func (e *entry) isDone() bool { return e.current >= len(e.questions) }

type Registry struct {
	mu      sync.Mutex
	connSeq uint64
	conns   map[uint64]map[string]*entry

	// answered tracks recently-answered asks (see answeredInfo/recordAnswered)
	// so a later `notify` frame — arriving after the entry above has already
	// been removed by sendAnswer — can still find where to render it.
	answered map[string]*answeredInfo
}

func NewRegistry() *Registry {
	return &Registry{
		conns:    make(map[uint64]map[string]*entry),
		answered: make(map[string]*answeredInfo),
	}
}

// answeredInfo is what a `notify` frame needs to render against an already-
// answered ask: where to deliver it (agentID/sessionKey) and, if known, the
// platform-native message ID of the last-presented question to edit in
// place. msgID is "" when PresentFn couldn't report one (e.g. the
// plain-text fallback in SendInteractiveMessageWithID) — renderNotify then
// falls back to a standalone message.
type answeredInfo struct {
	agentID    string
	sessionKey string
	msgID      string
	resolvedAt time.Time
}

// answeredTTL bounds how long an answered-but-not-yet-notified ask is kept
// around for a `notify` to find — mirrors HTTPTransport's own 15-minute
// abandoned-answer eviction window (internal/askgw/http.go), so an
// unnotified ask doesn't linger in memory forever.
const answeredTTL = 15 * time.Minute

// recordAnswered stashes (agentID, sessionKey, platformMsgID) for askID,
// keyed only by askID (a notify frame carries no connID) so a subsequent
// `notify` — over the same connection or, for the HTTP transport, any
// caller — can find it. Also opportunistically sweeps entries past
// answeredTTL; there is no background goroutine for this, so eviction is
// piggybacked onto the next write instead.
func (r *Registry) recordAnswered(askID, agentID, sessionKey, msgID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answered[askID] = &answeredInfo{
		agentID:    agentID,
		sessionKey: sessionKey,
		msgID:      msgID,
		resolvedAt: time.Now(),
	}
	cutoff := time.Now().Add(-answeredTTL)
	for id, info := range r.answered {
		if info.resolvedAt.Before(cutoff) {
			delete(r.answered, id)
		}
	}
}

// getAnswered looks up a previously-answered ask by ID for notify rendering.
// Returns nil if askID was never answered, or its entry aged out past
// answeredTTL.
func (r *Registry) getAnswered(askID string) *answeredInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.answered[askID]
}

func (r *Registry) RegisterConn() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connSeq++
	r.conns[r.connSeq] = make(map[string]*entry)
	return r.connSeq
}

func (r *Registry) UnregisterConn(connID uint64) []*entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.conns[connID]
	delete(r.conns, connID)
	entries := make([]*entry, 0, len(m))
	for _, e := range m {
		if e.timer != nil {
			e.timer.Stop()
		}
		if e.cancelFn != nil {
			e.cancelFn()
		}
		entries = append(entries, e)
	}
	return entries
}

func (r *Registry) Add(connID uint64, askID, agentID, sessionKey string, w connWriter, questions []AskQuestion, msgID string, timeout time.Duration, cancelFn func()) (*entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.conns[connID]
	if m == nil {
		return nil, fmt.Errorf("conn %d not registered", connID)
	}
	if _, exists := m[askID]; exists {
		return nil, fmt.Errorf("ask id %q already pending on conn %d", askID, connID)
	}
	e := &entry{
		connID:     connID,
		askID:      askID,
		agentID:    agentID,
		sessionKey: sessionKey,
		w:          w,
		questions:  questions,
		answers:    make(map[string]json.RawMessage),
		msgID:      msgID,
		cancelFn:   cancelFn,
	}
	if timeout > 0 {
		e.timer = time.AfterFunc(timeout, func() {
			r.ResolveTimeout(connID, askID)
		})
	}
	m[askID] = e
	return e, nil
}

func (r *Registry) Get(connID uint64, askID string) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.conns[connID]; m != nil {
		return m[askID]
	}
	return nil
}

// SetPresentedMsgID records the platform-native message ID of the question
// currently presented for (connID, askID), overwriting any prior value (a
// multi-question ask presents several messages in sequence — only the last
// one is live when the ask is finally answered). No-op if the entry is gone
// (already resolved/cancelled by the time PresentFn returned).
func (r *Registry) SetPresentedMsgID(connID uint64, askID, platformMsgID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.conns[connID]; m != nil {
		if e := m[askID]; e != nil {
			e.platformMsgID = platformMsgID
		}
	}
}

func (r *Registry) Remove(connID uint64, askID string) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.conns[connID]
	if m == nil {
		return nil
	}
	e := m[askID]
	delete(m, askID)
	if e != nil && e.timer != nil {
		e.timer.Stop()
	}
	return e
}

func (r *Registry) Cancel(connID uint64, askID string) bool {
	e := r.Remove(connID, askID)
	if e == nil {
		return false
	}
	if e.cancelFn != nil {
		e.cancelFn()
	}
	return true
}

func (r *Registry) ResolveTimeout(connID uint64, askID string) {
	e := r.Remove(connID, askID)
	if e == nil {
		return
	}
	_ = e.w.WriteFrame(AnswerFrame{
		Protocol: ProtocolVersion,
		Type:     TypeAnswer,
		ID:       askID,
		Status:   StatusTimeout,
	})
	askgwLog.Debugf("timeout conn=%d id=%s", connID, askID)
}

func (r *Registry) resolveDismissed(connID uint64, askID string) {
	e := r.Remove(connID, askID)
	if e == nil {
		return
	}
	_ = e.w.WriteFrame(AnswerFrame{
		Protocol: ProtocolVersion,
		Type:     TypeAnswer,
		ID:       askID,
		Status:   StatusDismissed,
	})
	askgwLog.Debugf("dismissed conn=%d id=%s", connID, askID)
}

func (r *Registry) ResolveUnavailable(w connWriter, askID string) {
	_ = w.WriteFrame(AnswerFrame{
		Protocol: ProtocolVersion,
		Type:     TypeAnswer,
		ID:       askID,
		Status:   StatusUnavailable,
	})
}

func (r *Registry) recordAnswer(connID uint64, askID, key string, answer json.RawMessage) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.conns[connID]
	if m == nil {
		return false
	}
	e := m[askID]
	if e == nil {
		return false
	}
	e.answers[key] = answer
	e.current++
	return e.isDone()
}

func (r *Registry) sendAnswer(connID uint64, askID string) {
	e := r.Remove(connID, askID)
	if e == nil {
		return
	}
	// Only an actually-answered ask gets a notify anchor: a client only
	// sends a completion notify after acting on a real human decision
	// (timeout/dismissed/unavailable asks have nothing to report against).
	r.recordAnswered(askID, e.agentID, e.sessionKey, e.platformMsgID)
	_ = e.w.WriteFrame(AnswerFrame{
		Protocol: ProtocolVersion,
		Type:     TypeAnswer,
		ID:       askID,
		Status:   StatusAnswered,
		Answers:  e.answers,
	})
	askgwLog.Debugf("answered conn=%d id=%s answers=%d", connID, askID, len(e.answers))
}

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
}

func NewRegistry() *Registry {
	return &Registry{conns: make(map[uint64]map[string]*entry)}
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
	_ = e.w.WriteFrame(AnswerFrame{
		Protocol: ProtocolVersion,
		Type:     TypeAnswer,
		ID:       askID,
		Status:   StatusAnswered,
		Answers:  e.answers,
	})
	askgwLog.Debugf("answered conn=%d id=%s answers=%d", connID, askID, len(e.answers))
}

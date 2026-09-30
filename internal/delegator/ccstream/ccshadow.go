// ccshadow.go — runs the Claude Code ledger adapter (ccbook.go) in SHADOW
// (#2111 §12): beside the live, turn-level cost path, booking into the scratch
// ledger (logging.api_shadow_db, accounting.Shadow) so a 1-2 day run can be
// compared with the live rows before the switch. Nothing here touches the live
// path or its accounting: every hook only enqueues, the adapter runs on its own
// goroutine, and a full queue drops (and counts) rather than blocks the stream.

package ccstream

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"foci/internal/delegator/accounting"
)

// ccEventKind names what a shadow event carries.
type ccEventKind int

const (
	ccNamed ccEventKind = iota
	ccMainLine
	ccSubLine
	ccTailOpened
	ccTailClosed
	ccBoundaryEv
	ccResultEv
	ccCostState
	ccSessionFile
	ccExit
)

// ccEvent is one input to the adapter, stamped when it happened.
type ccEvent struct {
	kind    ccEventKind
	at      time.Time
	turn    string // the foci turn open when it happened
	id      string
	agent   string
	parent  string // ccTailOpened: the agent whose turn this one books on
	line    *ccLine
	mu      map[string]ModelUsage
	running int
}

// ccShadow owns one CC process's shadow adapter.
type ccShadow struct {
	book    *ccBook
	events  chan ccEvent
	dropped atomic.Int64
	done    chan struct{}

	tailOnce  sync.Once
	tailStop  chan struct{}
	tailDone  chan struct{}
	closeOnce sync.Once
}

// shadowTick is how often the adapter settles results whose bound passed.
var shadowTick = 50 * time.Millisecond

// newCCShadow starts the adapter for a process launched now, or returns nil
// when no shadow ledger is configured.
func newCCShadow(b *Backend, session, agentID string, baseline map[string]ModelUsage) *ccShadow {
	l := accounting.Shadow()
	if l == nil {
		return nil
	}
	s := &ccShadow{
		book:     newCCBook(l, b.logger(), session, agentID, time.Now(), baseline),
		events:   make(chan ccEvent, 8192),
		done:     make(chan struct{}),
		tailStop: make(chan struct{}),
		tailDone: make(chan struct{}),
	}
	go s.run()
	return s
}

// enqueue hands an event to the adapter without ever blocking the caller.
func (s *ccShadow) enqueue(e ccEvent) {
	if s == nil {
		return
	}
	if e.at.IsZero() {
		e.at = time.Now()
	}
	select {
	case s.events <- e:
	default:
		if s.dropped.Add(1) == 1 {
			s.book.lg.Warnf("ledger shadow: event queue full; dropping (counted at close)")
		}
	}
}

func (s *ccShadow) run() {
	defer close(s.done)
	tick := time.NewTicker(shadowTick)
	defer tick.Stop()
	c := s.book
	for {
		select {
		case e := <-s.events:
			switch e.kind {
			case ccNamed:
				c.streamNamed(e.id, e.turn, e.at)
			case ccMainLine:
				c.mainLine(e.line)
			case ccSubLine:
				c.subLine(e.agent, e.turn, e.line)
			case ccTailOpened:
				c.tailOpenedUnder(e.agent, e.parent, e.turn)
			case ccTailClosed:
				c.tailClosed(e.agent)
			case ccBoundaryEv:
				c.compactBoundary(e.turn, e.at)
			case ccResultEv:
				c.result(e.mu, e.running, e.at)
			case ccCostState:
				c.costStateSeen(e.mu, e.at)
			case ccSessionFile:
				c.sessionFile = e.id
			case ccExit:
				c.exit()
				if n := s.dropped.Load(); n > 0 {
					c.lg.Warnf("ledger shadow: %d event(s) dropped on a full queue; this process's shadow rows are incomplete", n)
				}
				return
			}
			c.settle(false)
		case <-tick.C:
			c.settle(false)
		}
	}
}

// startMainTail tails the process's main transcript from offset (the file's
// size at launch: everything before it is an earlier process's) and feeds
// every assistant line and this process's cost-state record to the adapter.
// Idempotent.
func (s *ccShadow) startMainTail(path string, offset int64) {
	if s == nil || path == "" {
		return
	}
	s.tailOnce.Do(func() {
		s.enqueue(ccEvent{kind: ccSessionFile, id: path})
		go s.tailMain(path, offset)
	})
}

func (s *ccShadow) tailMain(path string, offset int64) {
	defer close(s.tailDone)
	var f *os.File
	for f == nil {
		var err error
		if f, err = os.Open(path); err == nil {
			break
		}
		select {
		case <-s.tailStop:
			return
		case <-time.After(subagentTailPoll):
		}
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		s.book.lg.Warnf("ledger shadow: seek %s to %d: %v", path, offset, err)
		return
	}
	r := bufio.NewReaderSize(f, 1<<20)
	var partial []byte
	drain := func() {
		for {
			chunk, err := r.ReadBytes('\n')
			partial = append(partial, chunk...)
			if err != nil {
				return // EOF: keep the torn line for the next read
			}
			line := bytes.TrimRight(partial, "\r\n")
			partial = partial[:0]
			// Several lines can land in one read — CC re-appends history in
			// batch writes (F1b) — so every complete line is handled here.
			if l, cs := parseCCRecord(line); l != nil {
				s.enqueue(ccEvent{kind: ccMainLine, line: l})
			} else if cs != nil {
				s.enqueue(ccEvent{kind: ccCostState, mu: cs.ModelUsage})
			}
		}
	}
	for {
		drain()
		select {
		case <-s.tailStop:
			drain() // the process has exited: whatever it wrote is there now
			return
		case <-time.After(subagentTailPoll):
		}
	}
}

// close drains the main tail and flushes the process into the shadow ledger:
// called once the CC process has exited or is being closed. Bounded, so a
// stuck adapter never holds up teardown.
func (s *ccShadow) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.tailStop)
		// A tail that never started never will: mark it done so the wait
		// below passes at once. A running one ends after a final drain.
		s.tailOnce.Do(func() { close(s.tailDone) })
		select {
		case <-s.tailDone:
		case <-time.After(5 * time.Second):
			s.book.lg.Warnf("ledger shadow: main-transcript tail did not stop within 5s")
		}
		s.enqueue(ccEvent{kind: ccExit})
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			s.book.lg.Warnf("ledger shadow: adapter did not finish its exit flush within 10s")
		}
	})
}

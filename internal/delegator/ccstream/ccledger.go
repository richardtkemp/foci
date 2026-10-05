// ccledger.go — runs the Claude Code ledger adapter (ccbook.go) for one CC
// process (#2111 P2): it is this backend's only cost path. The hooks only
// enqueue, the adapter runs on its own goroutine, and a full queue drops (and
// says so loudly) rather than blocks the stream.
//
// The adapter books into accounting.Live(); with no live ledger (a unit test,
// or no api.db) a call is still observed — written to api.jsonl and handed to
// BookedHook — as every backend's is.

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

// ccEventKind names what an adapter event carries.
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
	ccMainRead
	ccMainTailEnded
	ccBarrier
	ccTurnEnded
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
	reply   chan struct{} // ccBarrier: closed once the barrier is passed
}

// ccLedger owns one CC process's ledger adapter.
type ccLedger struct {
	book    *ccBook
	events  chan ccEvent
	dropped atomic.Int64
	done    chan struct{}

	tailOnce  sync.Once
	tailStop  chan struct{}
	tailDone  chan struct{}
	tailPoke  chan struct{}
	closeOnce sync.Once

	// exitMu guards exited: once the adapter has flushed at exit, a turn
	// ending later (the agent layer completes a turn after its process died)
	// is applied to the book directly, as no worker reads the queue.
	exitMu sync.Mutex
	exited bool
}

// ccLedgerTick is how often the adapter settles results whose bound passed.
var ccLedgerTick = 50 * time.Millisecond

// ccBarrierBound caps how long a turn's completion waits for the main-thread
// calls the stream named to be booked (flush). The lines land within ~2ms of
// the result (P0-b) and the tail is poked to read at once, so the bound is
// only reached when a line never comes.
var ccBarrierBound = time.Second

// newCCLedger starts the adapter for a process launched now.
func newCCLedger(b *Backend, session, agentID string, baseline map[string]ModelUsage) *ccLedger {
	s := &ccLedger{
		book:     newCCBook(accounting.Live(), b.logger(), session, agentID, time.Now(), baseline),
		events:   make(chan ccEvent, 8192),
		done:     make(chan struct{}),
		tailStop: make(chan struct{}),
		tailDone: make(chan struct{}),
		tailPoke: make(chan struct{}, 1),
	}
	go s.run()
	return s
}

// enqueue hands an event to the adapter without ever blocking the caller.
func (s *ccLedger) enqueue(e ccEvent) {
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
			s.book.lg.Errorf("ledger: adapter event queue full; dropping events, so this process's spend will be under-booked (counted at close)")
		}
		if e.reply != nil {
			close(e.reply)
		}
	}
}

// flush waits until every main-thread call the stream has named so far is
// booked, or ccBarrierBound passes: called as a turn completes, so the agent
// layer's turn total (closeLedgerTurn) holds the turn's own calls. What books
// at a later quiet point — a remainder, a background subagent's later calls —
// is not waited for.
func (s *ccLedger) flush() {
	if s == nil {
		return
	}
	reply := make(chan struct{})
	s.enqueue(ccEvent{kind: ccBarrier, reply: reply})
	select {
	case <-reply:
	case <-s.done:
	case <-time.After(ccBarrierBound + time.Second):
		s.book.lg.Warnf("ledger: adapter did not pass a turn's barrier in time")
	}
}

// barrier is one flush waiting on the adapter.
type barrier struct {
	reply    chan struct{}
	deadline time.Time
}

func (s *ccLedger) run() {
	defer close(s.done)
	tick := time.NewTicker(ccLedgerTick)
	defer tick.Stop()
	c := s.book
	var waiting []barrier
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
				c.mainTailRunning(true)
			case ccMainRead:
				c.mainRead(e.at)
			case ccMainTailEnded:
				c.mainTailRunning(false)
			case ccBarrier:
				waiting = append(waiting, barrier{reply: e.reply, deadline: e.at.Add(ccBarrierBound)})
				s.poke()
			case ccTurnEnded:
				c.turnEnded(e.turn, e.at)
			case ccExit:
				c.exit()
				s.markExited()
				for _, w := range waiting {
					close(w.reply)
				}
				if n := s.dropped.Load(); n > 0 {
					c.lg.Errorf("ledger: %d adapter event(s) dropped on a full queue; this process's ledger rows are incomplete", n)
				}
				return
			}
			c.settle(false)
		case <-tick.C:
			c.settle(false)
			if len(waiting) > 0 {
				s.poke()
			}
		}
		waiting = s.release(waiting)
	}
}

// turnEnded tells the adapter a foci turn has ended (its idle): the turn's
// activity closes once its subagents' tails have closed (R8).
func (s *ccLedger) turnEnded(turn string) {
	if s == nil || turn == "" {
		return
	}
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	if s.exited {
		s.book.turnEnded(turn, time.Now())
		return
	}
	s.enqueue(ccEvent{kind: ccTurnEnded, turn: turn})
}

// markExited hands the book to turnEnded's direct path, first applying any
// turn end queued behind the exit.
func (s *ccLedger) markExited() {
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	s.exited = true
	for {
		select {
		case e := <-s.events:
			if e.kind == ccTurnEnded {
				s.book.turnEnded(e.turn, e.at)
			}
		default:
			return
		}
	}
}

// release passes every barrier whose named calls are all booked (or that has
// no main tail to wait on, or whose bound has passed), and returns the rest.
func (s *ccLedger) release(waiting []barrier) []barrier {
	if len(waiting) == 0 {
		return waiting
	}
	now := s.book.now()
	keep := waiting[:0]
	for _, w := range waiting {
		if !s.book.tailing || s.book.unseenNamed() == 0 || !now.Before(w.deadline) {
			close(w.reply)
			continue
		}
		keep = append(keep, w)
	}
	return keep
}

// poke asks the main tail to read now rather than at its next poll.
func (s *ccLedger) poke() {
	select {
	case s.tailPoke <- struct{}{}:
	default:
	}
}

// startMainTail tails the process's main transcript from offset (the file's
// size at launch: everything before it is an earlier process's) and feeds
// every assistant line and this process's cost-state record to the adapter.
// Idempotent.
func (s *ccLedger) startMainTail(path string, offset int64) {
	if s == nil || path == "" {
		return
	}
	s.tailOnce.Do(func() {
		s.enqueue(ccEvent{kind: ccSessionFile, id: path})
		go s.tailMain(path, offset)
	})
}

// Each finished read is reported with the time it began, after the lines it
// found (the queue keeps order), so the adapter can tell a line that never
// came from one not read yet (#2134).
func (s *ccLedger) tailMain(path string, offset int64) {
	defer close(s.tailDone)
	defer s.enqueue(ccEvent{kind: ccMainTailEnded})
	var f *os.File
	for f == nil {
		var err error
		start := time.Now()
		if f, err = os.Open(path); err == nil {
			break
		}
		s.enqueue(ccEvent{kind: ccMainRead, at: start}) // no file yet: no line either
		select {
		case <-s.tailStop:
			return
		case <-s.tailPoke:
		case <-time.After(subagentTailPoll):
		}
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		s.book.lg.Errorf("ledger: seek %s to %d: %v — this process's main-thread calls will not be booked", path, offset, err)
		return
	}
	r := bufio.NewReaderSize(f, 1<<20)
	var partial []byte
	drain := func() {
		start := time.Now()
		for {
			chunk, err := r.ReadBytes('\n')
			partial = append(partial, chunk...)
			if err != nil {
				s.enqueue(ccEvent{kind: ccMainRead, at: start})
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
		case <-s.tailPoke:
		case <-time.After(subagentTailPoll):
		}
	}
}

// close drains the main tail and flushes the process into the ledger: called
// once the CC process has exited or is being closed. Bounded, so a stuck
// adapter never holds up teardown.
func (s *ccLedger) close() {
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
			s.book.lg.Warnf("ledger: main-transcript tail did not stop within 5s")
		}
		s.enqueue(ccEvent{kind: ccExit})
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			s.book.lg.Warnf("ledger: adapter did not finish its exit flush within 10s")
		}
	})
}

package main

import (
	"bufio"
	"encoding/json"
	"time"
)

// stdinPump reads stdin on its own goroutine so the stub can see what foci
// writes while a run is still open, as real CC does (#3056). Without it a
// message foci writes mid-run sat in the pipe until the run ended and became
// a run of its own; real CC folds it into the open run.
//
// While a run is open the stub only waits (a scripted sleep, a hang, a
// pending permission). Each wait goes through absorb, which sorts what
// arrives:
//   - a user message is folded: it is answered inside the open run, after
//     the current answer, with no idle between (probed on CC 2.1.314:
//     result, init, assistant, result, then ONE idle);
//   - a control_response is recorded and marks its request answered;
//   - anything else (an interrupt, get_context_usage) is held and handled
//     after the run's idle, as the stub always did.
type stdinPump struct {
	lines <-chan []byte
	eof   bool

	// pending is replayed by next before stdin: the folded user messages
	// of the open run first, then the held envelopes once the run ends.
	pending  [][]byte
	folded   [][]byte
	held     [][]byte
	answered map[string]bool
}

func newStdinPump(in *bufio.Scanner) *stdinPump {
	ch := make(chan []byte, 64)
	go func() {
		for in.Scan() {
			ch <- append([]byte(nil), in.Bytes()...)
		}
		close(ch)
	}()
	return &stdinPump{lines: ch, answered: map[string]bool{}}
}

// next returns the next envelope to process between turns, or false at EOF.
func (p *stdinPump) next() ([]byte, bool) {
	if len(p.pending) > 0 {
		b := p.pending[0]
		p.pending = p.pending[1:]
		return b, true
	}
	if p.eof {
		return nil, false
	}
	b, ok := <-p.lines
	if !ok {
		p.eof = true
	}
	return b, ok
}

// raw reads the next stdin line directly, bypassing absorb. For the crash
// modes, which read stdin themselves while a run is stuck.
func (p *stdinPump) raw() ([]byte, bool) {
	if p.eof {
		return nil, false
	}
	b, ok := <-p.lines
	if !ok {
		p.eof = true
	}
	return b, ok
}

func (p *stdinPump) absorb(b []byte) {
	var env map[string]any
	if err := json.Unmarshal(b, &env); err != nil {
		return
	}
	switch env["type"] {
	case "user":
		p.folded = append(p.folded, b)
	case "control_response":
		recordControlResponse(env)
		if resp, ok := env["response"].(map[string]any); ok {
			if id, _ := resp["request_id"].(string); id != "" {
				p.answered[id] = true
			}
		}
	default:
		p.held = append(p.held, b)
	}
}

// sleep waits d inside an open run, absorbing what arrives. EOF does not
// cut the wait short: the old stub slept through it too.
func (p *stdinPump) sleep(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		if p.eof {
			<-timer.C
			return
		}
		select {
		case <-timer.C:
			return
		case b, ok := <-p.lines:
			if !ok {
				p.eof = true
				continue
			}
			p.absorb(b)
		}
	}
}

// waitAnswered blocks inside an open run until foci has answered every
// request in ids, absorbing what arrives. It returns false at EOF: foci
// closed stdin, so no answer will come.
func (p *stdinPump) waitAnswered(ids []string) bool {
	for {
		all := true
		for _, id := range ids {
			if !p.answered[id] {
				all = false
				break
			}
		}
		if all {
			return true
		}
		if p.eof {
			return false
		}
		b, ok := <-p.lines
		if !ok {
			p.eof = true
			continue
		}
		p.absorb(b)
	}
}

// takeFolded moves the open run's folded user messages to the front of the
// replay queue and reports whether there were any. The caller keeps the
// run open when it returns true.
func (p *stdinPump) takeFolded() bool {
	if len(p.folded) == 0 {
		return false
	}
	p.pending = append(append([][]byte{}, p.folded...), p.pending...)
	p.folded = nil
	return true
}

// endRun queues the envelopes held during the run, to be handled after
// its idle.
func (p *stdinPump) endRun() {
	p.pending = append(p.pending, p.held...)
	p.held = nil
}

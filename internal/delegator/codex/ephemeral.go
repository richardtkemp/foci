package codex

import "fmt"

type ephemeralFork struct {
	result threadResult
	hold   *Backend
}

// retainEphemeralFork keeps the existing subscription and one pooled-server reference.
// Unsubscribing or cold-resuming would discard its history or parent cache affinity.
func (b *Backend) retainEphemeralFork(result threadResult) {
	p := b.process()
	sharedPool.Lock()
	sharedPool.refs[b.agentID]++
	sharedPool.Unlock()
	hold := &Backend{shared: p, agentID: b.agentID}
	p.threadMapMu.Lock()
	if p.ephemeralForks == nil {
		p.ephemeralForks = make(map[string]ephemeralFork)
	}
	p.ephemeralForks[result.Thread.ID] = ephemeralFork{result: result, hold: hold}
	p.threadMapMu.Unlock()
}

// adoptEphemeralFork binds the live thread without thread/resume or prompt rewriting.
func (b *Backend) adoptEphemeralFork(id string) error {
	p := b.process()
	p.threadMapMu.Lock()
	fork, ok := p.ephemeralForks[id]
	delete(p.ephemeralForks, id)
	p.threadMapMu.Unlock()
	if !ok {
		return fmt.Errorf("ephemeral fork %s is no longer live; cannot resume it from storage", id)
	}
	defer func() { _ = fork.hold.Close() }()
	if err := b.verifyReviewer("thread/fork", fork.result); err != nil {
		b.unsubscribeThread(id)
		return err
	}
	b.mu.Lock()
	b.threadID = id
	b.model = fork.result.Model
	b.threadStatus = fork.result.Thread.Status
	b.mu.Unlock()
	b.registerThread(b.startOpts.SessionKey, id)
	b.readyOnce.Do(func() { close(b.readyCh) })
	if b.onSessionReady != nil {
		b.onSessionReady(id)
	}
	return nil
}

// discardEphemeralFork releases an unadopted child when branch setup fails.
func (b *Backend) discardEphemeralFork(id string) bool {
	p := b.process()
	p.threadMapMu.Lock()
	fork, ok := p.ephemeralForks[id]
	delete(p.ephemeralForks, id)
	p.threadMapMu.Unlock()
	if ok {
		b.unsubscribeThread(id)
		b.forgetFork(id)
		_ = fork.hold.Close()
	}
	return ok
}

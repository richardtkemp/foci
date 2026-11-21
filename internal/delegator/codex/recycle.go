package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"foci/internal/procx"
)

// Recycling the pooled app-server after the codex binary changes (#3233).
//
// One app-server serves an agent for as long as any of its sessions lives,
// which on a busy agent is days. An upgrade of codex underneath it leaves the
// running process on the old version, and some upgrades break it outright:
// Homebrew's cask upgrade deletes the old version's directory
// (Caskroom/codex/<old>/), and the old app-server then fails every new
// thread's "failed to spawn code-mode host .../Caskroom/codex/0.164.0/bin/
// codex-code-mode-host: host executable was not found" (work Mac, 30 Oct,
// 0.164.0 -> 0.166.0). Before this, only a foci restart cured it.
//
// Dick's ruling (2026-11-21): restart on change. So the owner records which
// executable it was launched from (binaryStamp) and compares it with what the
// configured binary resolves to now. When they differ and nothing is using the
// app-server, foci retires it: it leaves the pool, IsRunning reports false for
// it and for every facade on it, and its process is stopped. Each session then
// takes the ordinary dead-backend path on its next use
// (DelegatedManager.getOrCreate): the backend is respawned on a fresh
// app-server (the new binary) and the session's thread is resumed from its
// rollout, as after any app-server restart.
//
// The check runs at two moments:
//   - Start, before it would attach to the pooled owner. That is where a new
//     thread is created, which is what an upgrade breaks first.
//   - Every binaryCheckEvery on the owner (watchBinary), so an agent whose
//     sessions all stay open still moves to the new binary once it is idle.
//
// "Nothing is using it" (busyReason) means: no facade has a turn running or a
// compaction armed, no Start is between attaching and its thread being ready,
// no fork is waiting to be adopted (an ephemeral fork lives only in this
// process) and no request is waiting for its reply. A busy app-server is left
// alone and checked again next time; the ruling is restart when idle, never
// mid-turn.
//
// A turn that tries to begin on an app-server that has just been retired is
// refused with delegator.ErrBackendClosed before anything is sent, and the
// agent begins it again on the respawned backend (turn_delegated.go).

// binaryCheckEvery is how often a pooled owner compares its launch binary with
// the current one. A variable so tests can shorten it.
var binaryCheckEvery = time.Minute

// binaryStamp identifies the codex executable an app-server was launched from.
type binaryStamp struct {
	// path is the binary as resolved on the operator PATH (procx.LookPath),
	// e.g. /opt/homebrew/bin/codex.
	path string
	// real is path with every symlink resolved: Homebrew's versioned
	// Caskroom/codex/<version>/ path, which is what an upgrade changes.
	real string
	// info is the real file's stat: an in-place replacement keeps the path
	// but changes the inode, size or modification time.
	info os.FileInfo
}

// stampBinary resolves bin the way launchAppServer does and stats the file it
// ends at.
func stampBinary(bin string) (binaryStamp, error) {
	path, err := procx.LookPath(procx.Operator, bin)
	if err != nil {
		return binaryStamp{}, err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return binaryStamp{}, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return binaryStamp{}, err
	}
	return binaryStamp{path: path, real: real, info: info}, nil
}

// changedTo reports whether cur names a different executable from s, and how.
// A zero s (the launch stamp could not be taken) never reports a change: there
// is nothing to compare with.
func (s binaryStamp) changedTo(cur binaryStamp) (string, bool) {
	if s.info == nil || cur.info == nil {
		return "", false
	}
	switch {
	case s.path != cur.path:
		return fmt.Sprintf("the binary now resolves to %s (was %s)", cur.path, s.path), true
	case s.real != cur.real:
		return fmt.Sprintf("%s now points to %s (was %s)", s.path, cur.real, s.real), true
	case !os.SameFile(s.info, cur.info):
		return fmt.Sprintf("%s was replaced (new file)", s.real), true
	case s.info.Size() != cur.info.Size() || !s.info.ModTime().Equal(cur.info.ModTime()):
		return fmt.Sprintf("%s was rewritten (size %d -> %d, mtime %s -> %s)", s.real,
			s.info.Size(), cur.info.Size(),
			s.info.ModTime().UTC().Format(time.RFC3339), cur.info.ModTime().UTC().Format(time.RFC3339)), true
	}
	return "", false
}

// recordLaunchBinary stores the stamp of the binary this owner launches.
// Called by launchAppServer just before the spawn.
func (b *Backend) recordLaunchBinary(bin string) {
	s, err := stampBinary(bin)
	if err != nil {
		b.logWarnf("cannot identify codex binary %q (%v): an upgrade will not recycle this app-server", bin, err)
	}
	b.mu.Lock()
	b.binStamp = s
	b.mu.Unlock()
}

// watchBinary runs on the owner for the life of its process and retires it
// once its binary has changed and it is idle. done is the owner's reader-stop
// channel.
func (b *Backend) watchBinary(done <-chan struct{}) {
	t := time.NewTicker(binaryCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			unlock := acquireLocks.Lock(b.agentID)
			retired := b.recycleIfStaleLocked()
			unlock()
			if retired {
				return
			}
		}
	}
}

// recycleIfStaleLocked retires the owner b when the codex binary it was
// launched from has changed and nothing is using it, and reports whether it
// did. On retiring it waits for the process to exit, so a Start that called it
// launches the new app-server only once the old one is gone.
//
// The caller holds acquireLocks for b.agentID, so no Start can attach to b
// while this decides.
func (b *Backend) recycleIfStaleLocked() bool {
	if b.retired.Load() {
		return false
	}
	b.mu.Lock()
	launched := b.binStamp
	alive := b.running && !b.closing
	b.mu.Unlock()
	if !alive || launched.info == nil {
		return false
	}
	cur, err := stampBinary(b.codexBinary())
	if err != nil {
		// Mid-upgrade or uninstalled: there is nothing to relaunch with yet.
		// The old process may still work; check again next time.
		b.logDebugf("codex binary check: %v", err)
		return false
	}
	why, changed := launched.changedTo(cur)
	if !changed {
		return false
	}

	sharedPool.Lock()
	if sharedPool.servers[b.agentID] != b {
		sharedPool.Unlock()
		return false
	}
	busy := ""
	if b.starting > 0 {
		busy = "a session is starting on it"
	}
	if busy == "" {
		// recycleMu makes the idle decision and the retirement one step
		// against beginTurn, which claims a turn under the same lock.
		b.recycleMu.Lock()
		busy = b.busyReason()
		if busy == "" {
			b.retired.Store(true)
		}
		b.recycleMu.Unlock()
	}
	if busy != "" {
		sharedPool.Unlock()
		b.mu.Lock()
		first := !b.recycleWaitLogged
		b.recycleWaitLogged = true
		b.mu.Unlock()
		if first {
			b.logInfof("codex binary changed (%s); the app-server restarts once it is idle (now: %s)", why, busy)
		}
		return false
	}
	delete(sharedPool.servers, b.agentID)
	delete(sharedPool.refs, b.agentID)
	sharedPool.Unlock()

	pid := 0
	b.mu.Lock()
	if b.cmd != nil && b.cmd.Process != nil {
		pid = b.cmd.Process.Pid
	}
	b.mu.Unlock()
	b.logInfof("codex binary changed (%s): restarting the idle app-server (pid %d); each session resumes its thread on the new one at its next turn", why, pid)
	b.stopProcess()
	return true
}

// busyReason says what is using the app-server owned by b, or "" when it is
// idle. Called with sharedPool and b.recycleMu held.
func (b *Backend) busyReason() string {
	b.threadMapMu.RLock()
	facades := make(map[*Backend]string, len(b.threadBackends)+1)
	for tid, f := range b.threadBackends {
		facades[f] = tid
	}
	forks := len(b.ephemeralForks)
	b.threadMapMu.RUnlock()
	if _, ok := facades[b]; !ok {
		facades[b] = "(the owner's)"
	}
	for f, tid := range facades {
		f.turnMu.Lock()
		active := f.turnActive
		f.turnMu.Unlock()
		if active {
			return "a turn is running on thread " + tid
		}
		f.compactMu.Lock()
		compacting := f.compactDoneCh != nil
		f.compactMu.Unlock()
		if compacting {
			return "a compaction is running on thread " + tid
		}
	}
	if forks > 0 {
		return fmt.Sprintf("%d fork(s) wait to be adopted", forks)
	}
	b.rpcMu.Lock()
	pending := len(b.pendingRPC)
	b.rpcMu.Unlock()
	if pending > 0 {
		return fmt.Sprintf("%d request(s) wait for a reply", pending)
	}
	return ""
}

package tools

import (
	"context"
	"time"

	"foci/internal/log"
)

// ClearAll stops all watches, clears the owned sessions map, and stops the
// TTL reaper. Called by the tmux memory monitor after kill-server to reset
// tool instance state.
func (inst *tmuxInstance) ClearAll() {
	if inst.reaperCancel != nil {
		inst.reaperCancel()
	}

	inst.mu.Lock()
	// Cancel all watches
	for key, ws := range inst.watched {
		ws.cancel()
		delete(inst.watched, key)
	}
	inst.persistWatches()
	// Clear owned set
	inst.owned = make(map[string]string)
	inst.lastAccess = make(map[string]time.Time)
	inst.persistOwned()
	inst.mu.Unlock()

	inst.sendMu.Lock()
	inst.lastSend = make(map[string]time.Time)
	inst.sendMu.Unlock()

	log.Debugf("tmux", "ClearAll: cleared all watches and owned sessions")
}

// ttlReaper periodically checks for idle tmux sessions and kills them.
func (inst *tmuxInstance) ttlReaper(ctx context.Context) {
	// Tick at sessionTTL/4, minimum 1 minute
	interval := inst.sessionTTL / 4
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			inst.reapExpiredSessions()
		case <-ctx.Done():
			return
		}
	}
}

// reapExpiredSessions kills tmux sessions that haven't been accessed within the TTL.
func (inst *tmuxInstance) reapExpiredSessions() {
	inst.mu.Lock()
	var expired []string
	now := time.Now()
	for name, lastAccess := range inst.lastAccess {
		if now.Sub(lastAccess) > inst.sessionTTL {
			expired = append(expired, name)
		}
	}
	if len(expired) == 0 {
		inst.mu.Unlock()
		return
	}
	for _, name := range expired {
		delete(inst.owned, name)
		delete(inst.lastAccess, name)
	}
	inst.persistOwned()
	inst.mu.Unlock()

	for _, name := range expired {
		inst.cancelWatchesForSession(name)

		killed, err := killSessionWithChildren(context.Background(), name)
		if err != nil {
			log.Debugf("tmux", "ttl reaper: %s: %v", name, err)
			continue
		}
		log.Infof("tmux", "ttl reaper: killed idle session %s (TTL %v exceeded)", name, inst.sessionTTL)
		if killed > 0 {
			log.Debugf("tmux", "ttl reaper: terminated %d child process(es) for %s", killed, name)
		}
	}

	inst.sendMu.Lock()
	for _, name := range expired {
		delete(inst.lastSend, name)
	}
	inst.sendMu.Unlock()

	maybeKillTmuxServer(context.Background())
}

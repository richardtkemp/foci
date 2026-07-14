package session

import "foci/internal/log"

// sessionLog returns a logger scoped to a single session key, so per-session
// store/index/branch lines name their session ([session:<key>]) instead of a
// bare [session] shared across every session. Registry-wide batch operations
// (sweeps, migrations, rebuilds) keep the plain "session" component.
func sessionLog(key string) *log.ComponentLogger {
	return log.NewComponentLogger("session:" + key)
}

package main

import (
	"sync/atomic"
	"time"

	"foci/internal/config"
	"foci/internal/usercmd"
)

// userCommandTimeoutNs is the process-wide limit for a chat "!" command and a
// code block's run button (#3045), from the global [tools]
// user_command_timeout. Every agent's UserRunner reads it at each run, so the
// live applier's store reaches the next command without a restart.
var userCommandTimeoutNs atomic.Int64

func init() { userCommandTimeoutNs.Store(int64(usercmd.DefaultTimeout)) }

// userCommandTimeout is UserRunner.Timeout for every agent.
func userCommandTimeout() time.Duration {
	return time.Duration(userCommandTimeoutNs.Load())
}

// applyUserCommandTimeout sets the limit from cfg: at startup, and from the
// live applier on an edit of tools.user_command_timeout.
func applyUserCommandTimeout(cfg *config.Config) {
	userCommandTimeoutNs.Store(int64(usercmd.Timeout(cfg.Tools.UserCommandTimeout)))
}

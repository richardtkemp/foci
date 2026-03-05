package agent

import (
	"fmt"
	"time"
)

// logTurnLockWait logs a warning when the turn lock was held longer than the
// configured threshold, including details about the current holder if found.
func (a *Agent) logTurnLockWait(sessionKey string, lockDur time.Duration, waiterTrigger string) {
	warnThreshold := a.TurnLockWarnThreshold
	if warnThreshold <= 0 {
		warnThreshold = 3 * time.Minute
	}
	if lockDur > warnThreshold && waiterTrigger != "proactive_warning" {
		holder := ""
		for _, td := range a.ProcessingDetails() {
			if td.SessionKey == sessionKey {
				holder = fmt.Sprintf(" holder_trigger=%s holder_tool=%s holder_elapsed=%s",
					td.Trigger, td.ToolName, time.Since(td.StartTime).Truncate(time.Millisecond))
				break
			}
		}
		a.logger().Warnf("turn_lock_held session=%s waited=%s waiter_trigger=%s%s", sessionKey, lockDur, waiterTrigger, holder)
	} else {
		a.logger().Debugf("turn_lock_acquired session=%s waited=%s", sessionKey, lockDur)
	}
}

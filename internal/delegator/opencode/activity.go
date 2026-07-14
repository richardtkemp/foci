// activity.go — delegator.ActivityChecker implementation.
// Backend.LastActivity() proxies to the Server's shared atomic stamp,
// which the SSE subscriber updates on every inbound frame (event or
// heartbeat). Since all Backends on a Server share one subscriber, they
// all see the same activity time — an activity-aware timeout in the
// agent layer (turn_delegated.go) uses this to distinguish "alive, just
// slow" from "dead, no events arriving."

package opencode

import "time"
import

// LastActivity returns the time of the most recent SSE frame from the
// opencode server. Implements delegator.ActivityChecker.
//
// Returns the zero time if the Server has never received an event
// (fresh Server, or subscriber not yet connected). All Backends sharing
// a Server see the same value because the stamp lives on the Server,
// not the Backend.
"foci/internal/log"

var (
	opencodeLog = log.NewComponentLogger("opencode")
)

func (b *Backend) LastActivity() time.Time {
	if b.server == nil {
		return time.Time{}
	}
	ns := b.server.lastActivity.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

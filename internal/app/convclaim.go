package app

import "time"

// convClaims is the per-chat claim a background model check of a chat takes
// before it runs: the automatic chat name (#2962) and the chat sections
// (#2993). One check runs per chat at a time; a message stored while one runs
// asks for another when it ends; a failed check leaves the chat alone for a
// gap, so a broken provider is not called on every message. Not safe on its
// own: the owner holds its mutex around claim and release.
type convClaims struct {
	busy    map[string]bool      // convID → a check is running
	again   map[string]bool      // convID → a message was stored while its check ran
	retryAt map[string]time.Time // convID → not before this, after a failure
	now     func() time.Time     // nil = time.Now; a test sets it
}

func newConvClaims() convClaims {
	return convClaims{busy: make(map[string]bool), again: make(map[string]bool), retryAt: make(map[string]time.Time)}
}

func (c *convClaims) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// claim takes convID for one check. false when a check of convID is already
// running (convID is then marked to be checked again when it ends) or convID
// is waiting out a failure.
func (c *convClaims) claim(convID string) bool {
	if c.busy[convID] {
		c.again[convID] = true
		return false
	}
	if t, ok := c.retryAt[convID]; ok {
		if c.clock().Before(t) {
			return false
		}
		delete(c.retryAt, convID)
	}
	c.busy[convID] = true
	return true
}

// release frees convID; failed starts a retry gap of gap. It returns true when
// a message was stored during the check, so the caller checks again.
func (c *convClaims) release(convID string, failed bool, gap time.Duration) (again bool) {
	delete(c.busy, convID)
	again = c.again[convID]
	delete(c.again, convID)
	if failed {
		c.retryAt[convID] = c.clock().Add(gap)
	}
	return again
}

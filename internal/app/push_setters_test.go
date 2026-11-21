package app

import "time"

// Single-feature setters for tests: production records a whole hello at once
// (pushTokens.hello), and these keep the older tests' one-flag-at-a-time setup.

func (p *pushTokens) setAlarmPush(deviceID string, ok bool, at time.Time) {
	if deviceID == "" {
		return
	}
	p.mu.Lock()
	p.setAlarmPushLocked(deviceID, ok, at)
	p.saveLocked()
	p.mu.Unlock()
}

func (p *pushTokens) setAskgw(deviceID string, ok bool) {
	if deviceID == "" {
		return
	}
	p.mu.Lock()
	p.setAskgwLocked(deviceID, ok)
	p.saveLocked()
	p.mu.Unlock()
}

func (p *pushTokens) setAskgwValue(deviceID string, ok bool) {
	if deviceID == "" {
		return
	}
	p.mu.Lock()
	p.setAskgwValueLocked(deviceID, ok)
	p.saveLocked()
	p.mu.Unlock()
}

func (p *pushTokens) setReadWake(deviceID string, ok bool) {
	if deviceID == "" {
		return
	}
	p.mu.Lock()
	p.setReadWakeLocked(deviceID, ok)
	p.saveLocked()
	p.mu.Unlock()
}

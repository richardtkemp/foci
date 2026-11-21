package app

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"time"
)

// defaultPushTokensFile is the saved push registry, relative to data_dir (#3196).
const defaultPushTokensFile = "app-push-tokens.json"

// savedPushDevice is one device's entry in the saved push registry: its FCM
// token and the push features of its latest hello.
type savedPushDevice struct {
	DeviceID   string    `json:"deviceId"`
	Token      string    `json:"token,omitempty"`
	ReadWake   bool      `json:"readWake,omitempty"`
	Askgw      bool      `json:"askgw,omitempty"`
	AskgwValue bool      `json:"askgwValue,omitempty"`
	AlarmPush  time.Time `json:"alarmPush,omitzero"`
}

// loadPushTokens returns a registry saved to path, loaded with what the file
// holds. keep reports whether a device id is still paired: an entry for any
// other device (one revoked while the gateway was down, or before #3196 made
// revoke forget its push entry) is dropped, so it gets no wake push. A nil keep
// keeps every entry. path == "" returns an in-memory registry.
func loadPushTokens(path string, keep func(deviceID string) bool) *pushTokens {
	p := newPushTokens()
	p.path = path
	if path == "" {
		return p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			appLog.Warnf("push token store %s: %v (starting empty: each phone registers again at its next connect)", path, err)
		}
		return p
	}
	var list []savedPushDevice
	if err := json.Unmarshal(data, &list); err != nil {
		appLog.Warnf("push token store %s: %v (starting empty: each phone registers again at its next connect)", path, err)
		return p
	}
	dropped := 0
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range list {
		if d.DeviceID == "" {
			continue
		}
		if keep != nil && !keep(d.DeviceID) {
			dropped++
			continue
		}
		if d.Token != "" {
			p.tokens[d.DeviceID] = d.Token
		}
		if d.ReadWake {
			p.readWake[d.DeviceID] = true
		}
		if d.Askgw {
			p.askgw[d.DeviceID] = true
			p.askgwSeen = true
		}
		if d.AskgwValue {
			p.askgwValue[d.DeviceID] = true
		}
		if !d.AlarmPush.IsZero() {
			p.alarmPush[d.DeviceID] = d.AlarmPush
		}
	}
	if dropped > 0 {
		appLog.Infof("push token store: dropped %d entr(ies) for devices no longer paired", dropped)
		p.saveLocked()
	}
	appLog.Infof("loaded push registrations for %d device(s)", len(p.tokens))
	return p
}

// hello records one ClientHello under one lock and one save: the token (an
// empty token keeps the one held) and whether each push feature was advertised.
// at is the hello time, which picks the phone for an alarm push.
func (p *pushTokens) hello(deviceID, token string, features []string, at time.Time) {
	if deviceID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if token != "" {
		p.setLocked(deviceID, token)
	}
	p.setReadWakeLocked(deviceID, slices.Contains(features, featureReadWake))
	p.setAskgwLocked(deviceID, slices.Contains(features, featureAskgwPrompts))
	p.setAskgwValueLocked(deviceID, slices.Contains(features, featureAskgwValueInput))
	p.setAlarmPushLocked(deviceID, slices.Contains(features, featureAlarmPush), at)
	p.saveLocked()
}

// forget drops everything held for deviceID. Called when the device is
// revoked: a revoked device must get no more wake pushes, which carry message
// previews.
func (p *pushTokens) forget(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tokens, deviceID)
	delete(p.readWake, deviceID)
	delete(p.askgw, deviceID)
	delete(p.askgwValue, deviceID)
	delete(p.alarmPush, deviceID)
	p.saveLocked()
}

// saveLocked writes the registry to p.path atomically (temp + rename), 0600:
// FCM tokens address the user's phones. Caller holds p.mu for writing, which also
// serialises the temp file.
// A failed write is logged and the in-memory registry carries on.
func (p *pushTokens) saveLocked() {
	if p.path == "" {
		return
	}
	ids := make(map[string]struct{}, len(p.tokens))
	for id := range p.tokens {
		ids[id] = struct{}{}
	}
	for _, m := range []map[string]bool{p.readWake, p.askgw, p.askgwValue} {
		for id := range m {
			ids[id] = struct{}{}
		}
	}
	for id := range p.alarmPush {
		ids[id] = struct{}{}
	}
	list := make([]savedPushDevice, 0, len(ids))
	for id := range ids {
		list = append(list, savedPushDevice{
			DeviceID: id, Token: p.tokens[id], ReadWake: p.readWake[id], Askgw: p.askgw[id],
			AskgwValue: p.askgwValue[id], AlarmPush: p.alarmPush[id],
		})
	}
	slices.SortFunc(list, func(a, b savedPushDevice) int { return cmp.Compare(a.DeviceID, b.DeviceID) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		appLog.Errorf("push token store marshal: %v", err)
		return
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		appLog.Errorf("push token store write: %v", err)
		return
	}
	if err := os.Rename(tmp, p.path); err != nil {
		appLog.Errorf("push token store rename: %v", err)
	}
}

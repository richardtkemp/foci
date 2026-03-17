package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"foci/internal/log"
	"foci/internal/session"
)

type ManaWatcher struct {
	name             string
	thresholds       []int
	restoreThreshold int // fire restore notice when mana was below this then hits 100% (0=disabled)
	firedToday       map[int]bool
	seenBelow        bool // mana was seen below restoreThreshold today
	firedRestore     bool // restore notice already fired today
	mu               sync.Mutex
	lastReset        time.Time
	idx              *session.SessionIndex
	agentID          string
}

type manaWatcherState struct {
	FiredToday   map[int]bool `json:"fired_today"`
	LastReset    time.Time    `json:"last_reset"`
	SeenBelow    bool         `json:"seen_below"`
	FiredRestore bool         `json:"fired_restore"`
}

func NewManaWatcher(name string, thresholds []int) *ManaWatcher {
	if len(thresholds) == 0 {
		return nil
	}
	if name == "" {
		name = "mana"
	}
	sorted := make([]int, len(thresholds))
	copy(sorted, thresholds)
	sort.Sort(sort.Reverse(sort.IntSlice(sorted)))

	return &ManaWatcher{
		name:       name,
		thresholds: sorted,
		firedToday: make(map[int]bool),
		lastReset:  time.Now().Truncate(24 * time.Hour),
	}
}

func (m *ManaWatcher) SetSessionIndex(idx *session.SessionIndex, agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idx = idx
	m.agentID = agentID
}

// SetRestoreThreshold enables restore notification when mana reaches 100%
// after being seen below the given threshold. Set to 0 to disable.
func (m *ManaWatcher) SetRestoreThreshold(pct int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoreThreshold = pct
}

func (m *ManaWatcher) Restore() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.idx == nil {
		return
	}

	raw, err := m.idx.GetAgentMetadata(m.agentID, "mana:"+m.name)
	if err != nil || raw == "" {
		return
	}

	var state manaWatcherState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		log.Warnf("mana", "unmarshal fired state: %v", err)
		return
	}

	today := time.Now().Truncate(24 * time.Hour)
	if state.LastReset.Truncate(24 * time.Hour).Equal(today) {
		m.firedToday = state.FiredToday
		if m.firedToday == nil {
			m.firedToday = make(map[int]bool)
		}
		m.seenBelow = state.SeenBelow
		m.firedRestore = state.FiredRestore
	}
}

func (m *ManaWatcher) saveFiredState() {
	if m.idx == nil {
		return
	}
	state := manaWatcherState{
		FiredToday:   m.firedToday,
		LastReset:    m.lastReset,
		SeenBelow:    m.seenBelow,
		FiredRestore: m.firedRestore,
	}
	data, err := json.Marshal(state)
	if err != nil {
		log.Errorf("mana", "marshal fired state: %v", err)
		return
	}
	if err := m.idx.SetAgentMetadata(m.agentID, "mana:"+m.name, string(data)); err != nil {
		log.Errorf("mana", "persist fired state: %v", err)
	}
}

func (m *ManaWatcher) CheckAndWarn(manaStr, resetTime string, warnFunc func(string)) {
	if m == nil || warnFunc == nil || manaStr == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	today := now.Truncate(24 * time.Hour)

	if today.After(m.lastReset) {
		m.firedToday = make(map[int]bool)
		m.seenBelow = false
		m.firedRestore = false
		m.lastReset = today
	}

	mana := m.parseManaPercentage(manaStr)
	if mana < 0 {
		return
	}

	// Track if mana drops below restore threshold
	if m.restoreThreshold > 0 && mana <= m.restoreThreshold && !m.seenBelow {
		m.seenBelow = true
		m.saveFiredState()
	}

	for _, threshold := range m.thresholds {
		if mana <= threshold && !m.firedToday[threshold] {
			m.firedToday[threshold] = true
			m.saveFiredState()
			warnFunc(m.formatWarning(mana, resetTime))
			return
		}
	}
}

// CheckRestore checks if mana has restored to 100% after being below the
// restore threshold. Returns a notification message if so, empty string otherwise.
// This is separate from CheckAndWarn so it can be injected into the session
// rather than sent to the user.
func (m *ManaWatcher) CheckRestore(manaStr string) string {
	if m == nil || manaStr == "" || m.restoreThreshold == 0 {
		return ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.seenBelow || m.firedRestore {
		return ""
	}

	mana := m.parseManaPercentage(manaStr)
	if mana < 100 {
		return ""
	}

	m.firedRestore = true
	m.saveFiredState()
	return fmt.Sprintf("%s restored to 100%% (was below %d%% earlier)", m.name, m.restoreThreshold)
}

func (m *ManaWatcher) parseManaPercentage(manaStr string) int {
	manaStr = manaStr[:len(manaStr)-1]
	pct, err := strconv.Atoi(manaStr)
	if err != nil {
		return -1
	}
	return pct
}

func (m *ManaWatcher) formatWarning(mana int, resetTime string) string {
	if resetTime != "" {
		return fmt.Sprintf("low %s: %d%% remaining (resets %s)", m.name, mana, resetTime)
	}
	return fmt.Sprintf("low %s: %d%% remaining", m.name, mana)
}

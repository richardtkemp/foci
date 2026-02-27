package agent

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"foci/log"
	"foci/state"
)

type ManaWatcher struct {
	name       string
	thresholds []int
	firedToday map[int]bool
	mu         sync.Mutex
	lastReset  time.Time
	store      *state.Store
}

type manaWatcherState struct {
	FiredToday map[int]bool
	LastReset  time.Time
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

func (m *ManaWatcher) SetStore(store *state.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

func (m *ManaWatcher) Restore() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.store == nil {
		return
	}

	key := "mana:" + m.name
	var state manaWatcherState
	if !m.store.Get(key, &state) {
		return
	}

	today := time.Now().Truncate(24 * time.Hour)
	if state.LastReset.Truncate(24 * time.Hour).Equal(today) {
		m.firedToday = state.FiredToday
		if m.firedToday == nil {
			m.firedToday = make(map[int]bool)
		}
	}
}

func (m *ManaWatcher) saveFiredState() {
	if m.store == nil {
		return
	}
	key := "mana:" + m.name
	state := manaWatcherState{
		FiredToday: m.firedToday,
		LastReset:  m.lastReset,
	}
	if err := m.store.Set(key, state); err != nil {
		log.Errorf("mana", "persist fired state: %v", err)
	}
}

func (m *ManaWatcher) CheckAndWarn(manaStr string, warnFunc func(string)) {
	if m == nil || warnFunc == nil || manaStr == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	today := now.Truncate(24 * time.Hour)

	if today.After(m.lastReset) {
		m.firedToday = make(map[int]bool)
		m.lastReset = today
	}

	mana := m.parseManaPercentage(manaStr)
	if mana < 0 {
		return
	}

	for _, threshold := range m.thresholds {
		if mana <= threshold && !m.firedToday[threshold] {
			m.firedToday[threshold] = true
			m.saveFiredState()
			warnFunc(m.formatWarning(mana, threshold))
			return
		}
	}
}

func (m *ManaWatcher) parseManaPercentage(manaStr string) int {
	manaStr = manaStr[:len(manaStr)-1]
	pct, err := strconv.Atoi(manaStr)
	if err != nil {
		return -1
	}
	return pct
}

func (m *ManaWatcher) formatWarning(mana, threshold int) string {
	return fmt.Sprintf("low %s: %d%% remaining (threshold: %d%%)", m.name, mana, threshold)
}

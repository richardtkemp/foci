package agent

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

type ManaWatcher struct {
	thresholds []int
	firedToday map[int]bool
	mu         sync.Mutex
	lastReset  time.Time
}

func NewManaWatcher(thresholds []int) *ManaWatcher {
	if len(thresholds) == 0 {
		return nil
	}
	sorted := make([]int, len(thresholds))
	copy(sorted, thresholds)
	sort.Sort(sort.Reverse(sort.IntSlice(sorted)))

	return &ManaWatcher{
		thresholds: sorted,
		firedToday: make(map[int]bool),
		lastReset:  time.Now().Truncate(24 * time.Hour),
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
	return fmt.Sprintf("low mana: %d%% remaining (threshold: %d%%)", mana, threshold)
}

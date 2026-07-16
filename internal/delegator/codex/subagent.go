package codex

import (
	"encoding/json"
	"sync"
	"time"
)

// subagentTracker manages polling of subagent threads to deliver their text
// output via OnSubagentText. Multiple subagents can run simultaneously;
// each is tracked by its agentThreadId and polled independently.
type subagentTracker struct {
	mu     sync.Mutex
	active map[string]*subagentPoll // agentThreadId → poll state
}

type subagentPoll struct {
	groupKey  string        // the item ID used as the OnSubagentStart groupKey
	seenItems int           // items already delivered (avoids re-delivery)
	done      chan struct{} // closed to stop the polling goroutine
}

func newSubagentTracker() *subagentTracker {
	return &subagentTracker{active: make(map[string]*subagentPoll)}
}

// start begins polling a subagent thread for agentMessage items.
// Called when subAgentActivity kind=started fires.
func (st *subagentTracker) start(b *Backend, agentThreadID, groupKey string) {
	st.mu.Lock()
	if _, exists := st.active[agentThreadID]; exists {
		st.mu.Unlock()
		return // already tracking
	}
	poll := &subagentPoll{groupKey: groupKey, done: make(chan struct{})}
	st.active[agentThreadID] = poll
	st.mu.Unlock()

	go st.pollLoop(b, agentThreadID, poll)
}

// stop signals the polling goroutine to do a final read then exit.
// Called when subAgentActivity kind=interrupted or kind=interacted fires.
func (st *subagentTracker) stop(_ *Backend, agentThreadID string) {
	st.mu.Lock()
	poll, ok := st.active[agentThreadID]
	if ok {
		delete(st.active, agentThreadID)
	}
	st.mu.Unlock()
	if !ok {
		return
	}
	close(poll.done)
}

// stopAll stops all active subagent polls (used on backend shutdown / turn reset).
func (st *subagentTracker) stopAll() {
	st.mu.Lock()
	for _, poll := range st.active {
		select {
		case <-poll.done:
		default:
			close(poll.done)
		}
	}
	st.active = make(map[string]*subagentPoll)
	st.mu.Unlock()
}

func (st *subagentTracker) pollLoop(b *Backend, agentThreadID string, poll *subagentPoll) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-poll.done:
			b.readSubagentThread(agentThreadID, poll)
			return
		case <-ticker.C:
			b.readSubagentThread(agentThreadID, poll)
		case <-b.done:
			return
		}
	}
}

// readSubagentThread calls thread/read on a subagent's thread, extracts
// new agentMessage items, and delivers them via OnSubagentText.
func (b *Backend) readSubagentThread(agentThreadID string, poll *subagentPoll) {
	result, err := b.sendAndWait("thread/read", map[string]interface{}{
		"threadId":     agentThreadID,
		"includeTurns": true,
	})
	if err != nil {
		return
	}

	var resp struct {
		Thread struct {
			Turns []struct {
				Items []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return
	}

	// Flatten all turns' items into a single list
	var items []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	for _, turn := range resp.Thread.Turns {
		items = append(items, turn.Items...)
	}

	// Deliver only new agentMessage items
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == "agentMessage" && items[i].Text != "" {
			// Check if this is a new item (beyond what we've seen)
			if i >= poll.seenItems {
				se := b.sessionEvents.Load()
				if se != nil && se.OnSubagentText != nil {
					se.OnSubagentText(poll.groupKey, items[i].Text)
				}
			}
		}
	}
	poll.seenItems = len(items)
}

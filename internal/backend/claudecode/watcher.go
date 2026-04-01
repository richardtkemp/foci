package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"foci/internal/backend"

	"github.com/fsnotify/fsnotify"
)

// agentCall tracks a pending Agent tool_use call.
type agentCall struct {
	id          string // tool_use ID
	description string // short description from input
}

// sessionWatcher tails a Claude Code session JSONL file and emits events
// as new entries are appended. Uses fsnotify for immediate event delivery.
// isSyntheticNoResponse returns true for CC's synthetic "no response" messages
// that should be silently dropped. CC generates these locally (model: "<synthetic>")
// when it has nothing to say — e.g. after "Continue from where you left off."
func isSyntheticNoResponse(text string) bool {
	t := strings.TrimSpace(text)
	return t == "No response requested." || t == "[[NO_RESPONSE]]"
}

type sessionWatcher struct {
	path    string
	fsnot   *fsnotify.Watcher
	mu      sync.Mutex
	offset  int64                // current read position in the file
	handler *backend.EventHandler // current turn's handler (nil between turns)

	// onPermissionCheck is called periodically to detect permission prompts
	// in the tmux pane. Decoupled from session events because prompts can
	// appear at any time (after sub-agent completion, slow tools, etc.).
	onPermissionCheck func()

	// onAgentStatus is called when agent spawn/completion status changes.
	// Receives a formatted status string to send to the user.
	onAgentStatus func(text string)

	// turnState tracks the current turn's accumulated text, tool calls, and usage.
	turnText  string
	turnTools int
	turnUsage *backend.TurnUsage // usage from the last assistant message
	turnModel string             // model from the last assistant message

	// agentTracking tracks pending Agent tool_use calls within a turn.
	pendingAgents []agentCall
	agentStart    time.Time // when the first agent in a batch was spawned
}

// close shuts down the fsnotify watcher.
func (w *sessionWatcher) close() {
	w.fsnot.Close()
}

// newSessionWatcher creates a watcher for the given JSONL file path.
// It seeks to the end of the file so only new entries are processed.
// newSessionWatcher creates a watcher for the given JSONL file, starting
// from startOffset. Pass -1 to start from the current end of file (tail mode).
// Pass 0 to read from the beginning. Pass a recorded offset to resume from
// a known position (e.g. pre-send offset to catch responses written before
// the watcher started).
func newSessionWatcher(path string, startOffset int64) (*sessionWatcher, error) {
	fsnot, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	if err := fsnot.Add(path); err != nil {
		fsnot.Close()
		return nil, fmt.Errorf("watch session file: %w", err)
	}

	offset := startOffset
	if offset < 0 {
		info, err := os.Stat(path)
		if err != nil {
			fsnot.Close()
			return nil, fmt.Errorf("stat session file: %w", err)
		}
		offset = info.Size()
	}

	return &sessionWatcher{
		path:   path,
		fsnot:  fsnot,
		offset: offset,
	}, nil
}

// watchLoop blocks until the context is cancelled, reading new JSONL entries
// as they are appended to the session file. Also periodically fires
// onPermissionCheck (if set) to detect permission prompts in the tmux pane.
func (w *sessionWatcher) watchLoop(ctx context.Context) {
	// Periodic permission check — catches prompts that appear after tool
	// execution starts (sub-agents, slow tools, etc.).
	permTicker := time.NewTicker(3 * time.Second)
	defer permTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.fsnot.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) {
				w.mu.Lock()
				h := w.handler
				w.mu.Unlock()
				if h != nil {
					w.readNew(h)
				}
			}
		case <-permTicker.C:
			if w.onPermissionCheck != nil {
				w.onPermissionCheck()
			}
		case _, ok := <-w.fsnot.Errors:
			if !ok {
				return
			}
		}
	}
}

// setHandler sets the event handler for the current turn.
func (w *sessionWatcher) setHandler(h *backend.EventHandler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handler = h
}

// readNew reads any new lines appended since the last read and processes them.
func (w *sessionWatcher) readNew(handler *backend.EventHandler) {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.Open(w.path)
	if err != nil {
		return
	}
	defer f.Close()

	if _, err := f.Seek(w.offset, io.SeekStart); err != nil {
		return
	}

	scanner := bufio.NewScanner(f)
	// Session entries can be large (tool results with full file contents).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		w.processLine(line, handler)
	}

	pos, err := f.Seek(0, io.SeekCurrent)
	if err == nil {
		w.offset = pos
	}
}

// processLine parses a single JSONL entry and dispatches events.
func (w *sessionWatcher) processLine(line []byte, handler *backend.EventHandler) {
	var entry sessionEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return // skip unparseable lines
	}

	switch entry.Type {
	case "assistant":
		w.handleAssistant(&entry, handler)
	case "user":
		w.handleUser(&entry)
	case "system":
		w.handleSystem(&entry, handler)
	// progress, file-history-snapshot, queue-operation: ignored for now
	}
}

// handleAssistant processes an assistant entry, extracting text deltas
// and tool call events.
func (w *sessionWatcher) handleAssistant(entry *sessionEntry, handler *backend.EventHandler) {
	if entry.Message == nil {
		return
	}

	blocks := parseContentBlocks(entry.Message.Content)
	var newAgents []agentCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" && !isSyntheticNoResponse(b.Text) {
				w.turnText = b.Text
				if handler.OnText != nil {
					handler.OnText(b.Text)
				}
			}
		case "tool_use":
			w.turnTools++
			if handler.OnToolStart != nil {
				input := string(b.Input)
				handler.OnToolStart(b.Name, input)
			}
			// Track Agent tool calls for status reporting.
			if b.Name == "Agent" {
				desc := extractAgentDescription(b.Input)
				newAgents = append(newAgents, agentCall{id: b.ID, description: desc})
			}
		}
	}
	if len(newAgents) > 0 {
		w.pendingAgents = append(w.pendingAgents, newAgents...)
		if w.agentStart.IsZero() {
			w.agentStart = time.Now()
		}
		w.notifyAgentStatus()
	}

	// Extract usage and model from the assistant message (last one wins per turn).
	if entry.Message.Usage != nil {
		w.turnUsage = &backend.TurnUsage{
			InputTokens:              entry.Message.Usage.InputTokens,
			OutputTokens:             entry.Message.Usage.OutputTokens,
			CacheCreationInputTokens: entry.Message.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     entry.Message.Usage.CacheReadInputTokens,
		}
	}
	if entry.Message.Model != "" {
		w.turnModel = entry.Message.Model
	}

	stopReason := ""
	if entry.Message.StopReason != nil {
		stopReason = *entry.Message.StopReason
	}
	if stopReason == "end_turn" {
		w.fireTurnResult(handler)
	}
}

// handleSystem processes system entries.
//
// turn_duration: written after completed turns. Fallback turn completion
// signal for turns that end with stop_sequence or other non-end_turn stop
// reasons (which handleAssistant doesn't catch).
//
// compact_boundary: written when CC completes /compact. The command doesn't
// produce an assistant message (no end_turn) or turn_duration, so without
// this check WaitForTurn blocks until the next real turn — causing the
// "context compacted" notification to arrive one turn late.
func (w *sessionWatcher) handleSystem(entry *sessionEntry, handler *backend.EventHandler) {
	switch entry.Subtype {
	case "turn_duration", "compact_boundary":
		w.fireTurnResult(handler)
	}
}

// fireTurnResult builds a TurnResult from accumulated state and fires
// the handler's OnTurnComplete. Called from both handleAssistant (end_turn)
// and handleSystem (turn_duration). If both fire for the same turn, the
// second is a safe no-op — fireTurnComplete is one-shot (nils the callback
// after first call) and turn state is already reset.
func (w *sessionWatcher) fireTurnResult(handler *backend.EventHandler) {
	result := &backend.TurnResult{
		Text:      w.turnText,
		ToolCalls: w.turnTools,
		Usage:     w.turnUsage,
		Model:     w.turnModel,
	}
	w.turnText = ""
	w.turnTools = 0
	w.turnUsage = nil
	w.turnModel = ""
	if handler.OnTurnComplete != nil {
		handler.OnTurnComplete(result)
	}
}

// handleUser processes user entries, tracking Agent tool_result completions.
func (w *sessionWatcher) handleUser(entry *sessionEntry) {
	if entry.Message == nil || len(w.pendingAgents) == 0 {
		return
	}
	blocks := parseContentBlocks(entry.Message.Content)
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		// Match tool_result to a pending agent by tool_use_id.
		resultID := b.ToolUseID
		for i, ag := range w.pendingAgents {
			if ag.id == resultID {
				w.pendingAgents = append(w.pendingAgents[:i], w.pendingAgents[i+1:]...)
				w.notifyAgentStatus()
				break
			}
		}
	}
}

// notifyAgentStatus sends an agent status update if the callback is set.
func (w *sessionWatcher) notifyAgentStatus() {
	if w.onAgentStatus == nil {
		return
	}
	pending := len(w.pendingAgents)
	if pending == 0 {
		elapsed := time.Since(w.agentStart).Round(time.Second)
		w.onAgentStatus(fmt.Sprintf("✅ Agents complete (%s)", elapsed))
		w.agentStart = time.Time{}
	} else {
		var descs []string
		for _, ag := range w.pendingAgents {
			if ag.description != "" {
				descs = append(descs, ag.description)
			}
		}
		if len(descs) > 0 {
			w.onAgentStatus(fmt.Sprintf("🔄 %d agent(s) running: %s", pending, strings.Join(descs, ", ")))
		} else {
			w.onAgentStatus(fmt.Sprintf("🔄 %d agent(s) running", pending))
		}
	}
}

// extractAgentDescription parses the "description" field from an Agent tool_use input.
func extractAgentDescription(raw json.RawMessage) string {
	var input struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(raw, &input) == nil {
		return input.Description
	}
	return ""
}




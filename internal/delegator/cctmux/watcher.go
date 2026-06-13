package cctmux

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"foci/internal/delegator"

	"github.com/fsnotify/fsnotify"
)

// watchEvents is the sink the watcher dispatches parsed JSONL events to.
// Implemented by *Backend, which routes delivery (onText/onToolStart/onToolEnd)
// to the session-scoped SessionEvents and completion (onTurnComplete) to the
// per-turn TurnEvents. Kept as an interface so watcher tests can supply a fake.
type watchEvents interface {
	onText(text string)
	onToolStart(id, name, input string)
	onToolEnd(id, name, output string, isError bool)
	onTurnComplete(result *delegator.TurnResult)
}

// sessionWatcher tails a Claude Code session JSONL file and emits events
// as new entries are appended. Uses fsnotify for immediate event delivery.
type sessionWatcher struct {
	path   string
	fsnot  *fsnotify.Watcher
	mu     sync.Mutex
	offset int64       // current read position in the file
	events watchEvents // event sink; set once in startWatcher, never nil after

	// onPermissionCheck is called periodically to detect permission prompts
	// in the tmux pane. Decoupled from session events because prompts can
	// appear at any time (after sub-agent completion, slow tools, etc.).
	onPermissionCheck func()

	// agents tracks pending Agent tool_use calls within a turn and emits
	// aggregated status messages (e.g. "🔄 2 agent(s) running: ...").
	agents delegator.AgentTracker

	// turnState tracks the current turn's accumulated text, tool calls, and usage.
	turnText  string
	turnTools int
	turnUsage *delegator.TurnUsage // usage from the last assistant message
	turnModel string               // model from the last assistant message

	// toolNamesByID maps tool_use IDs observed in assistant messages to their
	// tool names, so the subsequent tool_result block (which only carries the
	// tool_use_id back-reference) can be dispatched to OnToolEnd with the
	// originating name. Scoped to the current turn.
	toolNamesByID map[string]string
}

// close shuts down the fsnotify watcher.
func (w *sessionWatcher) close() {
	_ = w.fsnot.Close()
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
		_ = fsnot.Close()
		return nil, fmt.Errorf("watch session file: %w", err)
	}

	offset := startOffset
	if offset < 0 {
		info, err := os.Stat(path)
		if err != nil {
			_ = fsnot.Close()
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
				e := w.events
				w.mu.Unlock()
				if e != nil {
					w.readNew(e)
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

// setEvents sets the watcher's event sink. Called once in startWatcher with the
// Backend; the sink is session-scoped, not per-turn.
func (w *sessionWatcher) setEvents(e watchEvents) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = e
}

// readNew reads any new lines appended since the last read and processes them.
func (w *sessionWatcher) readNew(events watchEvents) {
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
		w.processLine(line, events)
	}

	pos, err := f.Seek(0, io.SeekCurrent)
	if err == nil {
		w.offset = pos
	}
}

// processLine parses a single JSONL entry and dispatches events.
//
// Sidechain entries (subagent turns spawned via the Agent tool) are skipped
// before dispatch — their text, tool calls, tool results, and turn-duration
// events belong to the sub-agent's own transcript and must not fire callbacks
// on the parent turn handler. Without this guard, handleAssistant would
// overwrite turnUsage/turnModel with subagent values, handleUser would fire
// OnToolEnd for nested tool_results, and handleSystem's turn_duration
// path would fire OnTurnComplete on the parent prematurely.
func (w *sessionWatcher) processLine(line []byte, events watchEvents) {
	var entry sessionEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return // skip unparseable lines
	}

	if entry.IsSidechain {
		return
	}

	switch entry.Type {
	case "assistant":
		w.handleAssistant(&entry, events)
	case "user":
		w.handleUser(&entry, events)
	case "system":
		w.handleSystem(&entry, events)
		// progress, file-history-snapshot, queue-operation: ignored for now
	}
}

// handleAssistant processes an assistant entry, extracting text deltas
// and tool call events.
func (w *sessionWatcher) handleAssistant(entry *sessionEntry, events watchEvents) {
	if entry.Message == nil {
		return
	}

	blocks := parseContentBlocks(entry.Message.Content)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				w.turnText = b.Text
				events.onText(b.Text)
			}
		case "tool_use":
			w.turnTools++
			// Record id → name so handleUser can correlate tool_result
			// blocks (which only carry tool_use_id) back to the tool name.
			if w.toolNamesByID == nil {
				w.toolNamesByID = make(map[string]string)
			}
			w.toolNamesByID[b.ID] = b.Name
			events.onToolStart(b.ID, b.Name, string(b.Input))
			// Track Agent tool calls for status reporting.
			if b.Name == "Agent" {
				desc := delegator.ExtractAgentDescription(b.Input)
				w.agents.Add(b.ID, desc)
			}
		}
	}

	// Extract usage and model from the assistant message (last one wins per turn).
	if entry.Message.Usage != nil {
		w.turnUsage = &delegator.TurnUsage{
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
		w.fireTurnResult(events)
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
func (w *sessionWatcher) handleSystem(entry *sessionEntry, events watchEvents) {
	switch entry.Subtype {
	case "turn_duration", "compact_boundary":
		w.fireTurnResult(events)
	}
}

// fireTurnResult builds a TurnResult from accumulated state and fires
// the handler's OnTurnComplete. Called from both handleAssistant (end_turn)
// and handleSystem (turn_duration). If both fire for the same turn, the
// second is a safe no-op — fireTurnComplete is one-shot (nils the callback
// after first call) and turn state is already reset.
func (w *sessionWatcher) fireTurnResult(events watchEvents) {
	result := &delegator.TurnResult{
		Text:      w.turnText,
		ToolCalls: w.turnTools,
		Usage:     w.turnUsage,
		Model:     w.turnModel,
	}
	w.turnText = ""
	w.turnTools = 0
	w.turnUsage = nil
	w.turnModel = ""
	w.toolNamesByID = nil
	events.onTurnComplete(result)
}

// handleUser processes user entries, firing OnToolEnd for each tool_result
// block and tracking Agent tool_result completions. Tool results arrive on
// user messages (per the CC protocol) — the tool_use ID lets consumers
// correlate results with the matching OnToolStart event, and the recorded
// id → name map lets us pass the originating tool name through to
// OnToolEnd (tool_result blocks only carry the ID themselves).
func (w *sessionWatcher) handleUser(entry *sessionEntry, events watchEvents) {
	if entry.Message == nil {
		return
	}
	blocks := parseContentBlocks(entry.Message.Content)
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		name := w.toolNamesByID[b.ToolUseID]
		events.onToolEnd(b.ToolUseID, name, string(b.Content), b.IsError)
		delete(w.toolNamesByID, b.ToolUseID)
		if w.agents.Pending() > 0 {
			w.agents.Remove(b.ToolUseID)
		}
	}
}

package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"foci/internal/backend"

	"github.com/fsnotify/fsnotify"
)

// sessionWatcher tails a Claude Code session JSONL file and emits events
// as new entries are appended. Uses fsnotify for immediate event delivery.
type sessionWatcher struct {
	path    string
	fsnot   *fsnotify.Watcher
	mu      sync.Mutex
	offset  int64 // current read position in the file

	// turnState tracks the current turn's accumulated text and tool calls.
	turnText  string
	turnTools int
}

// newSessionWatcher creates a watcher for the given JSONL file path.
// It seeks to the end of the file so only new entries are processed.
func newSessionWatcher(path string) (*sessionWatcher, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat session file: %w", err)
	}

	fsnot, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	if err := fsnot.Add(path); err != nil {
		fsnot.Close()
		return nil, fmt.Errorf("watch session file: %w", err)
	}

	return &sessionWatcher{
		path:   path,
		fsnot:  fsnot,
		offset: info.Size(),
	}, nil
}

// watchLoop blocks until the context is cancelled, reading new JSONL entries
// as they are appended to the session file.
func (w *sessionWatcher) watchLoop(ctx context.Context, handler *backend.EventHandler) {
	defer w.fsnot.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.fsnot.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) {
				w.readNew(handler)
			}
		case _, ok := <-w.fsnot.Errors:
			if !ok {
				return
			}
			// fsnotify errors are transient; keep watching.
		}
	}
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
	case "system":
		w.handleSystem(&entry, handler)
	// user, progress, file-history-snapshot, queue-operation: ignored for now
	}
}

// handleAssistant processes an assistant entry, extracting text deltas
// and tool call events.
func (w *sessionWatcher) handleAssistant(entry *sessionEntry, handler *backend.EventHandler) {
	if entry.Message == nil {
		return
	}

	blocks := parseContentBlocks(entry.Message.Content)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			// CC writes incremental entries — each new assistant text entry
			// replaces the previous content for the same requestId. We emit
			// the full text each time; the platform streaming handler diffs.
			if b.Text != "" {
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
		}
	}

	// Check for turn completion.
	if entry.Message.StopReason != nil && *entry.Message.StopReason == "end_turn" {
		result := &backend.TurnResult{
			Text:      w.turnText,
			ToolCalls: w.turnTools,
		}
		if handler.OnTurnComplete != nil {
			handler.OnTurnComplete(result)
		}
	}
}

// handleSystem processes system entries (e.g. turn_duration markers).
func (w *sessionWatcher) handleSystem(entry *sessionEntry, handler *backend.EventHandler) {
	// turn_duration is emitted after each completed turn. We use
	// stop_reason == "end_turn" on the assistant message instead,
	// so this is just here for future use.
}

// resetTurn resets the per-turn accumulator state.
func (w *sessionWatcher) resetTurn() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.turnText = ""
	w.turnTools = 0
}

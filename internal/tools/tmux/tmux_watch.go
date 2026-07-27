package tmux

import (
	"context"
	"crypto/md5" // #nosec G501 - used for content checksums, not security
	"fmt"
	"regexp"
	"strings"
	"time"

	"foci/internal/tools"
	"foci/shared/prompts"
)

func (inst *tmuxInstance) watch(ctx context.Context, name string, window, thresholdSeconds int, conditional bool) (tools.ToolResult, error) {
	if name == "" {
		return tools.ToolResult{}, fmt.Errorf("name is required for watch")
	}
	if thresholdSeconds < 1 {
		thresholdSeconds = 30
	}

	// Info, not Debug: the watch lifecycle's other events ("inactivity
	// detected", "auto-unwatched") are Info, so at the default level a watch
	// that fires tells a complete story while one that never fires leaves no
	// trace at all — which is exactly the shape #1560 could not diagnose.
	tmuxLog.Infof("session=%s watch: name=%s window=%d threshold=%ds conditional=%v", tools.SessionKeyFromContext(ctx), name, window, thresholdSeconds, conditional)

	inst.mu.Lock()
	inst.lastAccess[name] = time.Now()
	key := fmt.Sprintf("%s:%d", name, window)
	if _, exists := inst.watched[key]; exists {
		inst.mu.Unlock()
		return tools.ToolResult{}, fmt.Errorf("session %s is already being watched", key)
	}

	// Capture initial pane content so the first poll doesn't reset the
	// activity timer by seeing a "changed" hash (zero-value → real hash).
	var initialHash [md5.Size]byte
	if out, err := inst.runTmux(context.Background(), "capture-pane", "-t",
		fmt.Sprintf("%s:%d", name, window), "-p"); err == nil {
		initialHash = md5.Sum([]byte(normalizePaneContent(out))) // #nosec G401
	} else {
		// Not fatal — the monitor re-reads on its first tick. But a pane that
		// is unreadable the instant a watch is placed usually means the
		// session is already gone, and the watch is about to tear itself down
		// silently. Say so now rather than leaving a watch that never fires.
		tmuxLog.Warnf("watch: initial capture-pane on %s:%d failed (%v) — watch may not fire", name, window, err)
	}

	monCtx, cancel := context.WithCancel(context.Background())
	ws := &watchedSession{
		session:         name,
		window:          window,
		threshold:       time.Duration(thresholdSeconds) * time.Second,
		lastActivity:    time.Now(),
		lastContent:     initialHash,
		notifier:        inst.notifier,
		agentSessionKey: tools.SessionKeyFromContext(ctx),
		autopilot:       inst.autopilot,
		conditional:     conditional,
		ctx:             monCtx,
		cancel:          cancel,
		done:            make(chan struct{}),
	}
	inst.watched[key] = ws
	inst.persistWatches()
	inst.mu.Unlock()

	// Start monitoring goroutine
	go tmuxWatchMonitor(ws, inst, key)

	label := "Watching"
	if conditional {
		label = "Conditionally watching"
	}
	return tools.TextResult(fmt.Sprintf("%s session %s (window %d) for inactivity (threshold: %ds)", label, name, window, thresholdSeconds)), nil
}

func (inst *tmuxInstance) unwatch(ctx context.Context, name string) (tools.ToolResult, error) {
	if name == "" {
		return tools.ToolResult{}, fmt.Errorf("name is required for unwatch")
	}

	sessionKey := tools.SessionKeyFromContext(ctx)
	tmuxLog.Debugf("unwatch: session=%s name=%s", sessionKey, name)

	inst.mu.Lock()
	// Collect watches matching this session name and session key
	prefix := name + ":"
	var toCancel []*watchedSession
	for key, ws := range inst.watched {
		if strings.HasPrefix(key, prefix) {
			// Only unwatch if session keys match (both empty = backwards compat)
			if ws.agentSessionKey == sessionKey || (ws.agentSessionKey == "" && sessionKey == "") {
				toCancel = append(toCancel, ws)
				delete(inst.watched, key)
			}
		}
	}
	if len(toCancel) == 0 {
		inst.mu.Unlock()
		return tools.ToolResult{}, fmt.Errorf("session %s is not being watched", name)
	}
	inst.persistWatches()
	inst.mu.Unlock()

	// Cancel goroutines outside the lock
	for _, ws := range toCancel {
		ws.cancel()
		<-ws.done
	}
	return tools.TextResult(fmt.Sprintf("Stopped watching session %s", name)), nil
}

// tuiNoisePatterns matches dynamic TUI elements that change without indicating
// meaningful activity. Only clocks and elapsed timers are filtered — spinners,
// token counts, percentages, and cost changes ARE signals of active work.
var tuiNoisePatterns = regexp.MustCompile(strings.Join([]string{
	`\d+[hm]\s*\d+[ms]`,             // elapsed timers: "1m 3s", "2h 30m"
	`\d+:\d{2}(:\d{2})?(\s*[AP]M)?`, // clocks: "14:30", "2:30:00 PM"
	`\d+\.\d+s`,                     // durations: "3.2s", "0.5s"
}, "|"))

// normalizePaneContent strips TUI noise from pane output so that only
// meaningful content changes are detected by the watch monitor. Only strips
// clocks and timers — spinners, token counts, and percentages are kept as
// they indicate active work.
func normalizePaneContent(content string) string {
	return tuiNoisePatterns.ReplaceAllString(content, "")
}

func tmuxWatchMonitor(ws *watchedSession, inst *tmuxInstance, key string) {
	defer close(ws.done)

	// Poll at threshold/3 so we get multiple observations per threshold
	// window — avoids boundary races where poll interval ≈ threshold.
	// Clamped to [500ms, 2s] for sensible bounds.
	pollInterval := ws.threshold / 3
	if pollInterval < 500*time.Millisecond {
		pollInterval = 500 * time.Millisecond
	}
	if pollInterval > 2*time.Second {
		pollInterval = 2 * time.Second
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Read pane content
			out, err := inst.runTmux(context.Background(), "capture-pane", "-t",
				fmt.Sprintf("%s:%d", ws.session, ws.window), "-p")
			if err != nil {
				// Usually the session exited normally, but this branch also
				// swallows a transient capture-pane failure — and either way
				// the watch stops firing forever. Warn, with the underlying
				// error: at Debug this was invisible at the default level, so
				// a watch that vanished looked identical to one that was
				// never registered (#1560).
				tmuxLog.Warnf("watch: capture-pane on %s:%d failed (%v) — treating session as exited, removing watch", ws.session, ws.window, err)
				inst.mu.Lock()
				delete(inst.watched, key)
				inst.persistWatches()
				inst.mu.Unlock()
				return
			}

			// Normalize pane content to filter out TUI noise (status bar
			// clocks, spinners, token counts, etc.) before hashing.
			normalized := normalizePaneContent(out)
			hash := md5.Sum([]byte(normalized)) // #nosec G401 - content change detection, not security

			// Check if content changed
			if hash != ws.lastContent {
				ws.lastContent = hash
				if ws.conditional {
					// First activity detected — convert to normal watch
					tmuxLog.Debugf("watch: conditional watch on %s:%d converted to normal (activity detected)", ws.session, ws.window)
					ws.conditional = false
				}
				ws.lastActivity = time.Now()
			} else if !ws.conditional {
				// Content unchanged and not conditional; check if threshold exceeded
				if time.Since(ws.lastActivity) > ws.threshold {
					tmuxLog.Infof("watch: inactivity detected on %s:%d (threshold %v exceeded)", ws.session, ws.window, ws.threshold)
					msg := prompts.FormatInjectedMessage("TMUX WATCH",
						time.Now(),
						fmt.Sprintf("Session %s:%d has been inactive for %v", ws.session, ws.window, ws.threshold))
					ws.notifier.InjectToAgent(ws.agentSessionKey, msg, "", "tmux_watch")

					if ws.autopilot {
						// Auto-unwatch: remove from watched map and exit goroutine
						tmuxLog.Infof("autopilot: auto-unwatched %s after inactivity", ws.session)
						inst.mu.Lock()
						delete(inst.watched, key)
						inst.persistWatches()
						inst.mu.Unlock()
						return
					}

					// Reset activity timer to avoid repeated alerts
					ws.lastActivity = time.Now()
				}
			}

		case <-ws.ctx.Done():
			return
		}
	}
}

package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"foci/internal/display"
	"foci/internal/log"
)

func (inst *tmuxInstance) start(ctx context.Context, name, command, workdir, keys string, watch bool) (ToolResult, error) {
	if name == "" {
		n := atomic.AddUint64(&tmuxCounter, 1)
		name = fmt.Sprintf("foci-%d", n)
	}

	log.Debugf("tmux", "start: name=%s command=%q workdir=%q keys=%q cols=%d rows=%d watch=%v", name, command, workdir, keys, inst.cols, inst.rows, watch)

	// Cancel any stale watches for this session name (e.g. from a prior
	// session that exited naturally before the monitor noticed).
	inst.cancelWatchesForSession(name)

	// Create the tmux session
	args := []string{"new-session", "-d", "-s", name}
	if workdir != "" {
		args = append(args, "-c", workdir)
	}

	// If keys are provided, append them as a shell-quoted argument to the command
	finalCommand := command
	if keys != "" && command != "" {
		finalCommand = command + " " + fmt.Sprintf("%q", keys)
	}
	if finalCommand != "" {
		args = append(args, finalCommand)
	}

	out, err := runTmux(ctx, args...)
	if err != nil {
		return ToolResult{}, fmt.Errorf("tmux new-session: %s %w", strings.TrimSpace(out), err)
	}

	// Resize window so output isn't truncated to a small default terminal size.
	if inst.cols > 0 && inst.rows > 0 {
		out, err = runTmux(ctx, "resize-window", "-t", name, "-x", fmt.Sprintf("%d", inst.cols), "-y", fmt.Sprintf("%d", inst.rows))
		if err != nil {
			log.Warnf("tmux", "resize-window: %s %v", strings.TrimSpace(out), err)
		}
	}

	sessionKey := SessionKeyFromContext(ctx)
	inst.mu.Lock()
	inst.owned[name] = sessionKey
	inst.lastAccess[name] = time.Now()
	inst.persistOwned()
	inst.mu.Unlock()

	result := fmt.Sprintf("Session started: %s", name)

	// Auto-watch for inactivity if requested and notifier is available
	if watch && inst.notifier != nil {
		watchRes, watchErr := inst.watch(ctx, name, 0, inst.watchThresholdSec)
		if watchErr != nil {
			log.Warnf("tmux", "auto-watch failed for %s: %v", name, watchErr)
		} else {
			result += "\n" + watchRes.Text
		}
	}

	return TextResult(result), nil
}

func (inst *tmuxInstance) send(ctx context.Context, name, keys string, enter bool) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, fmt.Errorf("name is required for send")
	}
	if keys == "" && !enter {
		return ToolResult{}, fmt.Errorf("keys is required for send (or set enter=true to send just Enter)")
	}
	sessionKey := SessionKeyFromContext(ctx)
	if !inst.owns(name, sessionKey) {
		return ToolResult{}, fmt.Errorf("session %q not owned by this session", name)
	}

	inst.mu.Lock()
	inst.lastAccess[name] = time.Now()
	inst.mu.Unlock()

	log.Debugf("tmux", "send: name=%s keys=%q enter=%v", name, keys, enter)
	LogSendEntry(name, len(keys), enter)

	// Rate-limit: enforce minimum gap between consecutive sends to the same session.
	inst.sendMu.Lock()
	if last, ok := inst.lastSend[name]; ok {
		if gap := time.Since(last); gap < sendMinGap {
			wait := sendMinGap - gap
			log.Debugf("tmux", "send: rate-limiting %s, sleeping %v", name, wait)
			LogSendRateLimiting(gap, wait)
			time.Sleep(wait)
		}
	}
	inst.lastSend[name] = time.Now()
	inst.sendMu.Unlock()

	// Send keys first, then Enter as a separate send-keys call.
	// Combining them in one call is unreliable with certain key strings.
	// Use -l flag to send keys as literal string (prevents tmux from interpreting special characters).
	var out string
	var err error
	if keys != "" {
		LogSendSendKeys(len(keys))
		out, err = runTmux(ctx, "send-keys", "-t", name, "-l", keys)
		if err != nil {
			LogSendExit(false, err.Error())
			return ToolResult{}, fmt.Errorf("tmux send-keys: %s %w", strings.TrimSpace(out), err)
		}
	}
	if enter {
		// Brief pause so TUI apps (Claude Code, OpenCode) can process
		// the pasted input before receiving Enter (#26b).
		time.Sleep(200 * time.Millisecond)
		LogSendSendEnter()
		out, err = runTmux(ctx, "send-keys", "-t", name, "Enter")
		if err != nil {
			LogSendExit(false, err.Error())
			return ToolResult{}, fmt.Errorf("tmux send-keys Enter: %s %w", strings.TrimSpace(out), err)
		}
	}

	result := "Keys sent."

	// Best-effort verification: check if sent keys appeared in pane output
	if keys != "" {
		verified := inst.verifyKeysInPane(ctx, name, keys)
		if !verified {
			result += " Keys sent but not confirmed in pane output."
		}
	}

	// Autopilot: auto-watch after send if not already watched
	if inst.autopilot && inst.notifier != nil {
		inst.mu.Lock()
		alreadyWatched := false
		prefix := name + ":"
		for key := range inst.watched {
			if strings.HasPrefix(key, prefix) {
				alreadyWatched = true
				break
			}
		}
		inst.mu.Unlock()

		if !alreadyWatched {
			watchRes, watchErr := inst.watch(ctx, name, 0, inst.watchThresholdSec)
			if watchErr != nil {
				log.Warnf("tmux", "autopilot: auto-watch failed for %s: %v", name, watchErr)
			} else {
				log.Debugf("tmux", "autopilot: auto-watching %s after send", name)
				result += "\n" + watchRes.Text
			}
		}
	}

	LogSendExit(true, "")
	return TextResult(result), nil
}

// lettersOnly strips everything except ASCII and Unicode letters from s.
func lettersOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// verifyKeysInPane checks if the sent keys appear in the pane output within a timeout.
// Returns true if keys were found, false if not found within the timeout.
func (inst *tmuxInstance) verifyKeysInPane(ctx context.Context, name, keys string) bool {
	// Strip to letters only and truncate to 100 chars so matching is resilient
	// to TUI chrome, special characters, and formatting differences.
	needle := lettersOnly(keys)
	if len(needle) > 100 {
		needle = needle[:100]
	}
	if needle == "" {
		return true // nothing meaningful to verify
	}

	log.Debugf("tmux", "verifyKeysInPane: name=%s needle=%q", name, needle)

	// Poll every 200ms for up to 2 seconds
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(2 * time.Second)

	for {
		select {
		case <-ticker.C:
			// Capture pane content
			out, err := runTmux(ctx, "capture-pane", "-t", name, "-p")
			if err != nil {
				log.Debugf("tmux", "verifyKeysInPane: capture failed: %v", err)
				return false
			}

			haystack := lettersOnly(out)
			if strings.Contains(haystack, needle) {
				log.Debugf("tmux", "verifyKeysInPane: keys confirmed in pane output")
				return true
			}

		case <-timeout:
			log.Debugf("tmux", "verifyKeysInPane: timeout, keys not found in pane output")
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (inst *tmuxInstance) read(ctx context.Context, name string, lines int, raw bool) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, fmt.Errorf("name is required for read")
	}
	sessionKey := SessionKeyFromContext(ctx)
	if !inst.owns(name, sessionKey) {
		return ToolResult{}, fmt.Errorf("session %q not owned by this session", name)
	}

	inst.mu.Lock()
	inst.lastAccess[name] = time.Now()
	inst.mu.Unlock()

	log.Debugf("tmux", "read: name=%s lines=%d raw=%v", name, lines, raw)

	out, err := runTmux(ctx, "capture-pane", "-t", name, "-p", fmt.Sprintf("-S-%d", lines))
	if err != nil {
		return ToolResult{}, fmt.Errorf("tmux capture-pane: %s %w", strings.TrimSpace(out), err)
	}
	content := strings.TrimRight(out, "\n")

	if raw {
		return TextResult(content), nil
	}

	agent := detectTUIAgent(content)
	if agent == "" {
		return TextResult(content), nil
	}
	return TextResult(cleanTUIOutput(content, agent)), nil
}

func (inst *tmuxInstance) list(ctx context.Context) (ToolResult, error) {
	out, err := runTmux(ctx, "list-sessions", "-F", "#{session_name}|#{session_windows}|#{session_created}")
	if err != nil {
		if strings.Contains(out, "no server running") || strings.Contains(out, "no current") {
			inst.clearStaleOwned()
			return TextResult("No tmux sessions."), nil
		}
		return ToolResult{}, fmt.Errorf("tmux list-sessions: %s %w", strings.TrimSpace(out), err)
	}

	// Get the current session key to filter sessions
	currentSessionKey := SessionKeyFromContext(ctx)

	inst.mu.Lock()
	ownedNames := make(map[string]string, len(inst.owned))
	for k, v := range inst.owned {
		ownedNames[k] = v
	}
	watched := make(map[string]*watchedSession, len(inst.watched))
	for k, v := range inst.watched {
		watched[k] = v
	}
	inst.mu.Unlock()

	var rows [][]string
	var ownedStillExist bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}
		name := parts[0]
		windows := parts[1]
		createdUnix, _ := strconv.ParseInt(parts[2], 10, 64)
		age := "?"
		if createdUnix != 0 {
			age = display.FormatDuration(time.Since(time.Unix(createdUnix, 0)))
		}

		storedKey, isOwned := ownedNames[name]
		if isOwned {
			ownedStillExist = true
		}

		// Only show sessions owned by the current agent session
		// (both empty = backwards compat for context.Background() in tests)
		if !isOwned || (storedKey != currentSessionKey && !(storedKey == "" && currentSessionKey == "")) {
			continue
		}

		// Owner: show the full session ID
		owner := storedKey
		if owner == "" {
			owner = "self"
		}

		watchInfo := "-"
		for _, ws := range watched {
			if ws.session == name {
				watchInfo = fmt.Sprintf("w%d: %s", ws.window, ws.threshold.Round(time.Second))
				break
			}
		}

		rows = append(rows, []string{name, windows, age, owner, watchInfo})
	}

	if len(rows) == 0 {
		return TextResult("No tmux sessions."), nil
	}

	// Clean up stale owned entries if none still exist in tmux
	if len(ownedNames) > 0 && !ownedStillExist {
		inst.clearStaleOwned()
	}

	cols := []display.Column{
		{Header: "SESSION"},
		{Header: "W", Align: display.AlignRight},
		{Header: "AGE", Align: display.AlignRight},
		{Header: "OWNER"},
		{Header: "WATCH"},
	}
	return TextResult(display.MarkdownTable(cols, rows)), nil
}

func (inst *tmuxInstance) kill(ctx context.Context, name string) (ToolResult, error) {
	if name == "" {
		return ToolResult{}, fmt.Errorf("name is required for kill")
	}
	sessionKey := SessionKeyFromContext(ctx)
	if !inst.owns(name, sessionKey) {
		return ToolResult{}, fmt.Errorf("session %q not owned by this session", name)
	}

	log.Debugf("tmux", "kill: name=%s", name)

	// Stop any watches first so the monitor goroutine doesn't fire during cleanup
	inst.cancelWatchesForSession(name)

	// Kill the tmux session and clean up child processes that survived SIGHUP
	killed, err := killSessionWithChildren(ctx, name)
	if err != nil {
		return ToolResult{}, err
	}

	// If no sessions remain, kill the server to avoid an orphaned tmux
	// server process. This is safe: we only kill when the server is empty.
	serverKilled := maybeKillTmuxServer(ctx)

	inst.mu.Lock()
	delete(inst.owned, name)
	delete(inst.lastAccess, name)
	inst.persistOwned()
	inst.mu.Unlock()

	inst.sendMu.Lock()
	delete(inst.lastSend, name)
	inst.sendMu.Unlock()

	result := fmt.Sprintf("Session killed: %s", name)
	if killed > 0 {
		result += fmt.Sprintf(" (%d child process(es) terminated)", killed)
		log.Infof("tmux", "kill %s: terminated %d orphaned child process(es)", name, killed)
	}
	if serverKilled {
		log.Infof("tmux", "kill %s: no sessions remain, killed tmux server", name)
	}

	return TextResult(result), nil
}

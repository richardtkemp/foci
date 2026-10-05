package ccstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"foci/internal/delegator"
)

// SendControl translates a backend-agnostic ControlRequest into the
// ccstream wire format and sends it to the CC subprocess.
func (b *Backend) SendControl(ctx context.Context, req delegator.ControlRequest) error {
	switch r := req.(type) {
	case *delegator.SetModelRequest:
		return b.sendSetModel(ctx, r.Model)
	case *delegator.SetPermissionModeRequest:
		b.mu.Lock()
		b.permMode = r.Mode
		b.mu.Unlock()
		return b.writer.SendControl(newRequestID(), &SetPermissionModeRequest{
			Subtype: "set_permission_mode",
			Mode:    r.Mode,
		})
	case *delegator.ApplyFlagSettingsRequest:
		if eff, ok := r.Settings["effortLevel"].(string); ok {
			b.mu.Lock()
			b.effortLevel = eff
			b.mu.Unlock()
		}
		return b.writer.SendControl(newRequestID(), &ApplyFlagSettingsRequest{
			Subtype:  "apply_flag_settings",
			Settings: r.Settings,
		})
	default:
		return fmt.Errorf("ccstream: unsupported control request type %T", req)
	}
}

// Interrupt cancels the current agent turn by sending an interrupt control
// message over the stdio protocol.
func (b *Backend) Interrupt(ctx context.Context) error {
	return b.writer.SendInterrupt()
}

// StopSubagents stops every tracked Agent-tool subagent with a stop_task
// control request per task, leaving the main turn and shell commands alone
// (#2138, #2140). Returns how many requests were sent. The stopped tasks
// retire from the tracker through their "stopped" task_notification.
//
// Verified live on CC 2.1.285 (#2140): stop_task stops a background AND a
// foreground subagent, with the commands it was running; a foreground one's
// Agent call returns "[Request interrupted by user for tool use]" and the
// turn carries on.
func (b *Backend) StopSubagents(ctx context.Context) (int, error) {
	return b.stopTasks("subagents", b.agents.IDsOfKind(delegator.SubagentKindAgent))
}

// StopCommands stops the main agent's own shell commands, foreground and
// background, with a stop_task per command, leaving the turn and the
// subagents alone (#2140). Verified live on CC 2.1.285: a stopped foreground
// command returns "Exit code 137 [killed]" and the turn carries on. A
// foreground command CC has not registered as a task yet (it does within a
// couple of seconds) has no task_id and is skipped.
func (b *Backend) StopCommands(ctx context.Context) (int, error) {
	keys := b.agents.IDsOfKind(delegator.SubagentKindCommand)
	return b.stopTasks("commands", append(keys, b.mainShellCallIDs()...))
}

// stopTasks sends one stop_task per key that has a recorded task_id, and
// returns how many it sent.
func (b *Backend) stopTasks(what string, keys []string) (int, error) {
	sent := 0
	var errs []error
	for _, key := range keys {
		taskID := b.taskIDFor(key)
		if taskID == "" {
			b.logger().Warnf("stop %s: no task_id for %s (task_started not seen), not stopped", what, key)
			continue
		}
		if err := b.writer.SendControl(newRequestID(), &StopTaskRequest{Subtype: "stop_task", TaskID: taskID}); err != nil {
			errs = append(errs, fmt.Errorf("stop_task %s: %w", taskID, err))
			continue
		}
		b.logger().Infof("stop %s: stop_task sent task_id=%s key=%s", what, taskID, key)
		sent++
	}
	return sent, errors.Join(errs...)
}

// noteMainShellCall records a main-thread foreground shell call (#2140).
func (b *Backend) noteMainShellCall(toolUseID string) {
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if b.mainShellCalls == nil {
		b.mainShellCalls = map[string]struct{}{}
	}
	b.mainShellCalls[toolUseID] = struct{}{}
}

// mainShellCallIDs returns the running main-thread foreground shell calls.
func (b *Backend) mainShellCallIDs() []string {
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	ids := make([]string, 0, len(b.mainShellCalls))
	for id := range b.mainShellCalls {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// dropUnregisteredShellCalls forgets, at turn end, the foreground shell calls
// CC never registered as tasks: they finished too quickly to, and no
// task_notification will come to retire them. A registered call stays until
// its own notification, which also covers one CC moved to the background.
func (b *Backend) dropUnregisteredShellCalls() {
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	for id := range b.mainShellCalls {
		if b.trackedTaskIDs[id] == "" {
			delete(b.mainShellCalls, id)
		}
	}
}

// recordTaskID remembers the task_id behind a tracker key (#2138).
func (b *Backend) recordTaskID(key, taskID string) {
	if key == "" || taskID == "" {
		return
	}
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	if b.trackedTaskIDs == nil {
		b.trackedTaskIDs = map[string]string{}
	}
	b.trackedTaskIDs[key] = taskID
}

// forgetTaskID drops a finished task's entry, and its main-thread shell call
// if it was one. A reactivation records it again at its own task_started.
func (b *Backend) forgetTaskID(key string) {
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	delete(b.trackedTaskIDs, key)
	delete(b.mainShellCalls, key)
}

// taskIDFor returns the task_id recorded for a tracker key, or "".
func (b *Backend) taskIDFor(key string) string {
	b.subagentRunsMu.Lock()
	defer b.subagentRunsMu.Unlock()
	return b.trackedTaskIDs[key]
}

// SetModel sends a set_model control request to CC via the generic
// ControlSender interface. Convenience method retained for direct callers.
func (b *Backend) SetModel(ctx context.Context, model string) error {
	return b.SendControl(ctx, &delegator.SetModelRequest{Model: model})
}

// sendSetModel sends a set_model control request and waits for CC's
// control_response, unlike set_permission_mode/apply_flag_settings which are
// intentionally fire-and-forget. CC validates the model id synchronously
// (e.g. rejects an unrecognized id) and reports success/failure in the
// response — without waiting for it, that signal was silently dropped
// (reqID discarded, OnControlResponse finds no waiter) and /model always
// reported "switched" even for a bogus model name. See controlResponseInbound
// for the verified wire shape.
func (b *Backend) sendSetModel(ctx context.Context, model string) error {
	reqID := newRequestID()

	ch := make(chan json.RawMessage, 1)
	b.pendingControlMu.Lock()
	if b.pendingControls == nil {
		b.pendingControls = make(map[string]chan json.RawMessage)
	}
	b.pendingControls[reqID] = ch
	b.pendingControlMu.Unlock()

	if err := b.writer.SendControl(reqID, &SetModelRequest{
		Subtype: "set_model",
		Model:   model,
	}); err != nil {
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return fmt.Errorf("send set_model: %w", err)
	}

	select {
	case raw := <-ch:
		var env controlResponseInbound
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("unmarshal set_model control_response: %w", err)
		}
		if env.Response.Subtype != "success" {
			if env.Response.Error != "" {
				return fmt.Errorf("%s", env.Response.Error)
			}
			return fmt.Errorf("set_model returned subtype %q", env.Response.Subtype)
		}
		return nil
	case <-ctx.Done():
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return ctx.Err()
	}
}

// ccStreamCacheTTL is Claude Code's prompt-cache time-to-live. CC marks its
// prompt with 1-hour extended cache_control, so a session's cache stays warm
// for an hour after the last turn (not the Anthropic 5-minute default).
const ccStreamCacheTTL = time.Hour

// StatusDetail returns the current CC permission mode for /status display.
func (b *Backend) StatusDetail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.permMode != "" {
		return "permission mode: " + b.permMode
	}
	return ""
}

// GetContextWindow sends a get_context_usage control request and returns the
// model's context window size. Zero API cost — CC computes this locally.
func (b *Backend) GetContextWindow(ctx context.Context) (*delegator.ContextWindow, error) {
	reqID := newRequestID()

	// Arm response channel before sending.
	ch := make(chan json.RawMessage, 1)
	b.pendingControlMu.Lock()
	if b.pendingControls == nil {
		b.pendingControls = make(map[string]chan json.RawMessage)
	}
	b.pendingControls[reqID] = ch
	b.pendingControlMu.Unlock()

	if err := b.writer.SendControl(reqID, &GetContextUsageRequest{
		Subtype: "get_context_usage",
	}); err != nil {
		// Clean up on send failure.
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return nil, fmt.Errorf("send get_context_usage: %w", err)
	}

	select {
	case raw := <-ch:
		var env controlResponseInbound
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("unmarshal control_response envelope: %w", err)
		}
		if env.Response.Subtype != "success" {
			return nil, fmt.Errorf("get_context_usage returned subtype %q", env.Response.Subtype)
		}
		var payload contextUsagePayload
		if err := json.Unmarshal(env.Response.Response, &payload); err != nil {
			return nil, fmt.Errorf("unmarshal context_usage payload: %w", err)
		}
		cats := make([]delegator.ContextCategory, len(payload.Categories))
		for i, c := range payload.Categories {
			cats[i] = delegator.ContextCategory{Name: c.Name, Tokens: c.Tokens}
		}
		return &delegator.ContextWindow{
			MaxTokens:   payload.MaxTokens,
			Model:       prefixedModel(payload.Model), // same spelling as TurnResult.Model (#1645)
			TotalTokens: payload.TotalTokens,
			Categories:  cats,
		}, nil
	case <-ctx.Done():
		// Clean up on timeout.
		b.pendingControlMu.Lock()
		delete(b.pendingControls, reqID)
		b.pendingControlMu.Unlock()
		return nil, ctx.Err()
	}
}

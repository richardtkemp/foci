package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"foci/internal/app/fap"
)

// AppInvoker routes a tool call to a connected app device and awaits its
// result. Implemented by *app.appConn (via the platform.Connection it
// satisfies); resolved at tool call time so a missing device returns
// gracefully rather than crashing registration.
//
// Returns fap.ToolResult directly (not a mirror struct) so *appConn — whose
// InvokeTool returns fap.ToolResult — actually satisfies this interface. An
// earlier local mirror (AppToolResult) had the same fields but a distinct type,
// so the runtime conn.(AppInvoker) assertion in tool_table.go always failed and
// the tool reported "no device" even when one was connected. fap is a leaf
// package (already imported by ask.go), so depending on it here is cycle-free.
type AppInvoker interface {
	InvokeTool(ctx context.Context, tool, action string, args json.RawMessage) (fap.ToolResult, error)
}

// NewAppAndroidTool creates the `app_android` tool — the agent-facing half of
// the device-tool mechanism. Sends an FAP `tool.invoke` to the connected
// Android device and awaits the reply.
//
// v1 limitations the description calls out to the agent:
//   - The on-device allowlist is empty by default; the user edits it in the
//     app's Advanced settings to expose tasks. "list" returns what's exposed.
//   - A task that overruns the device's sync window sends status="pending" as a
//     keepalive; the server keeps waiting (up to appToolInvokeTimeout) for the
//     real result, so slow tasks resolve synchronously. Only a task that also
//     exceeds that budget returns pending-with-no-result to the agent.
//   - If no device is connected, the tool returns an error string (not a Go
//     error) so the agent can react rather than abort the turn.

// appToolInvokeTimeout bounds how long the server waits for a device tool.result
// (including across "pending" keepalives). It MUST exceed the device's own hard
// cap for tracking a task, so the device always sends a terminal result before
// the server gives up — otherwise a slow result is lost. Device cap is ~55s
// (see AndroidTaskerToolHandler); 60s here leaves margin for round-trips.
const appToolInvokeTimeout = 60 * time.Second
//
// The invoker resolver is a func() so a missing app connection at process start
// doesn't disable the tool — it just returns "no device" when actually called,
// and the agent never sees the tool vanish between registrations.
func NewAppAndroidTool(invoker func() (AppInvoker, bool)) *Tool {
	return &Tool{
		Name:       "app_android",
		ExecExport: true,
		Positional: []string{"action"},
		Description: `Invoke a tool on the user's connected Android device (via Tasker).
action="list" returns the device's allowlisted tasks as JSON.
action="perform" runs a named task; pass its args as par1 (stringly-typed; structured args go as JSON-stringified in par1) and optionally par2.
Returns an error string if no device is connected. A "pending" result means the task is still running on-device past the sync window — the result is dropped (no async delivery yet).`,
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"enum": ["list", "perform"],
					"description": "list = show available tasks; perform = run a task"
				},
				"task": {"type": "string", "description": "Task name (required for action=perform; taken from the device's allowlist)"},
				"par1": {"type": "string", "description": "First positional param (%par1 in Tasker). For structured args, JSON-stringify into this slot."},
				"par2": {"type": "string", "description": "Second positional param (%par2 in Tasker)"}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			inv, ok := invoker()
			if !ok {
				return TextResult("Error: no Android device connected for this agent. Ask the user to open the foci app on their phone."), nil
			}
			var p struct {
				Action string `json:"action"`
				Task   string `json:"task"`
				Par1   string `json:"par1"`
				Par2   string `json:"par2"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return TextResult(""), fmt.Errorf("app_android: parse params: %w", err)
			}
			argsObj := map[string]any{"task": p.Task, "par1": p.Par1, "par2": p.Par2}
			argsJSON, _ := json.Marshal(argsObj)

			// Bound the wait even if the caller's ctx has no deadline. A device
			// "pending" keepalive doesn't end this wait — InvokeTool keeps
			// waiting for the terminal result up to appToolInvokeTimeout.
			ctx, cancel := context.WithTimeout(ctx, appToolInvokeTimeout)
			defer cancel()

			res, err := inv.InvokeTool(ctx, "android", p.Action, argsJSON)
			if err != nil {
				return TextResult(fmt.Sprintf("Error: %v", err)), nil
			}
			switch res.Status {
			case fap.ToolStatusCompleted:
				if len(res.Output) > 0 {
					return TextResult(string(res.Output)), nil
				}
				return TextResult("(task completed; no output)"), nil
			case fap.ToolStatusPending:
				// Only reached if the task ran longer than appToolInvokeTimeout —
				// the device kept it running but the server gave up waiting.
				msg := fmt.Sprintf("Task is still running on the device and did not finish within %s; its result won't be delivered.", appToolInvokeTimeout)
				if res.Error != "" {
					msg += " " + res.Error
				}
				return TextResult(msg), nil
			case fap.ToolStatusError:
				return TextResult(fmt.Sprintf("Device tool error: %s", res.Error)), nil
			default:
				return TextResult(fmt.Sprintf("Unknown status %q: %s", res.Status, res.Error)), nil
			}
		},
	}
}

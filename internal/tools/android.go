package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// AppToolResult is the device-side tool's reply, mirroring the FAP
// `tool.result` payload. Status is "completed" / "pending" / "error".
type AppToolResult struct {
	Status string
	Output json.RawMessage
	Error  string
}

// AppInvoker routes a tool call to a connected app device and awaits its
// result. Implemented by *app.appConn (via the platform.Connection it
// satisfies); resolved at tool call time so a missing device returns
// gracefully rather than crashing registration.
type AppInvoker interface {
	InvokeTool(ctx context.Context, tool, action string, args json.RawMessage) (AppToolResult, error)
}

// NewAppAndroidTool creates the `app_android` tool — the agent-facing half of
// the device-tool mechanism. Sends an FAP `tool.invoke` to the connected
// Android device and awaits the reply.
//
// v1 limitations the description calls out to the agent:
//   - The on-device allowlist is empty by default; the user edits the app's
//     TaskerAllowlist to expose tasks. "list" returns whatever the user exposed.
//   - If the device's sync window (10s) expires, the device returns
//     status="pending" and the result is dropped (no async injection yet).
//   - If no device is connected, the tool returns an error string (not a Go
//     error) so the agent can react rather than abort the turn.
//
// The invoker resolver is a func() so a missing app connection at process start
// doesn't disable the tool — it just returns "no device" when actually called,
// and the agent never sees the tool vanish between registrations.
func NewAppAndroidTool(invoker func() (AppInvoker, bool)) *Tool {
	return &Tool{
		Name: "app_android",
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

			// Bound the wait even if the caller's ctx has no deadline. The
			// device's sync window is 10s; 60s leaves headroom for slow
			// round-trips and any Tasker settling.
			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			res, err := inv.InvokeTool(ctx, "android", p.Action, argsJSON)
			if err != nil {
				return TextResult(fmt.Sprintf("Error: %v", err)), nil
			}
			switch res.Status {
			case "completed":
				if len(res.Output) > 0 {
					return TextResult(string(res.Output)), nil
				}
				return TextResult("(task completed; no output)"), nil
			case "pending":
				msg := "Task is still running on the device past the sync window."
				if res.Error != "" {
					msg += " " + res.Error
				}
				msg += " The result is dropped (no async delivery yet)."
				return TextResult(msg), nil
			case "error":
				return TextResult(fmt.Sprintf("Device tool error: %s", res.Error)), nil
			default:
				return TextResult(fmt.Sprintf("Unknown status %q: %s", res.Status, res.Error)), nil
			}
		},
	}
}

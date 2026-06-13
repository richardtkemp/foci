package dispatch

import "strings"

// CallbackAction identifies the type of button callback.
type CallbackAction int

const (
	// CallbackCommand is a command keyboard callback ("cmd:" prefix).
	CallbackCommand CallbackAction = iota
	// CallbackInteractive is an interactive message callback ("im:" prefix).
	CallbackInteractive
	// CallbackToolCall is a tool call expand/collapse callback ("tc:" prefix).
	CallbackToolCall
	// CallbackThinking is a thinking block expand/collapse callback ("th:" prefix).
	CallbackThinking
	// CallbackSubagentHide is a "hide this subagent's messages" callback ("sa:" prefix).
	CallbackSubagentHide
	// CallbackUnknown is an unrecognized callback type.
	CallbackUnknown
)

// ParseCallback extracts the action type and data from a callback string.
// The data is everything after the prefix (e.g. "cmd:/status" → CallbackCommand, "/status").
func ParseCallback(data string) (CallbackAction, string) {
	if strings.HasPrefix(data, "cmd:") {
		return CallbackCommand, data[4:]
	}
	if strings.HasPrefix(data, "im:") {
		return CallbackInteractive, data[3:]
	}

	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		return CallbackUnknown, data
	}
	switch parts[0] {
	case "tc":
		return CallbackToolCall, parts[1]
	case "th":
		return CallbackThinking, parts[1]
	case "sa":
		return CallbackSubagentHide, parts[1]
	default:
		return CallbackUnknown, data
	}
}

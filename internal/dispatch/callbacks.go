package dispatch

import (
	"strconv"
	"strings"
)

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
	// CallbackWizard is a wizard step button callback ("wz:" prefix); the
	// payload is a step token plus an option index or "cancel".
	CallbackWizard
	// CallbackUnknown is an unrecognized callback type.
	CallbackUnknown
)

// wizardCancelChoice is the CallbackWizard payload suffix for the Cancel
// button ("<token>:cancel").
const wizardCancelChoice = "cancel"

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
	case "wz":
		return CallbackWizard, parts[1]
	default:
		return CallbackUnknown, data
	}
}

// ParseWizardCallback splits a CallbackWizard payload into its step token and
// choice. data is "<token>:<index>" (an option index) or "<token>:cancel"
// (the Cancel button, choice -1). ok is false for a malformed payload; the
// platform treats such a press as stale ("This step is no longer active.")
// rather than feeding anything to the wizard.
func ParseWizardCallback(data string) (token string, choice int, ok bool) {
	token, rest, has := strings.Cut(data, ":")
	if !has || token == "" || rest == "" {
		return "", 0, false
	}
	if rest == wizardCancelChoice {
		return token, -1, true
	}
	idx, err := strconv.Atoi(rest)
	if err != nil || idx < 0 {
		return "", 0, false
	}
	return token, idx, true
}

// Package usercmd holds the transport-neutral rules of the chat "!cmd" feature
// (#2812): what counts as a command, what timeout it gets, and how its result
// reads to the agent, the app and the text platforms. Running one lives in
// internal/tools/shell (UserRunner) and the agent's wrapper in internal/agent
// (usercmd.go); see docs/WIRING.md "Chat shell commands".
package usercmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Parse reports whether text typed by a person is a shell command, and returns
// its script: everything after the '!', every line of it. Leading whitespace
// before the '!' is allowed, as for '/' commands. A '!' followed by whitespace,
// and a lone '!', are ordinary messages. The caller decides eligibility: only
// typed text with no attachment on a chat platform may reach this function
// (voice, send_to_session, /send, webhooks and the rest never do).
func Parse(text string) (script string, ok bool) {
	s := strings.TrimLeftFunc(text, unicode.IsSpace)
	if len(s) < 2 || s[0] != '!' {
		return "", false
	}
	if r, _ := utf8.DecodeRuneInString(s[1:]); unicode.IsSpace(r) {
		return "", false
	}
	return s[1:], true
}

// The states a command moves through. The strings are also the wire values of
// fap.ShellCommand.State.
const (
	StateRunning   = "running"
	StateExited    = "exited"    // ran to its end; ExitCode says how
	StateTimedOut  = "timed_out" // killed at the timeout
	StateCancelled = "cancelled" // killed by the user's cancel
	StateFailed    = "failed"    // never ran to an end (start error, gateway restart)
)

// Refusal texts: the short reply a refused command gets.
const (
	RefusedGroup       = "Shell commands (!) are refused in group chats."
	RefusedNotOperator = "Shell commands (!) are refused: only this agent's operators may run them."
	RefusedUnavailable = "Shell commands (!) are refused: this agent cannot run them."
)

// Result is one finished command. Output and Command are secret-scrubbed.
type Result struct {
	Command  string        // the script, scrubbed
	State    string        // a State* constant
	ExitCode int           // meaningful for StateExited
	Timeout  time.Duration // the limit that applied
	Output   string        // the whole output, or only its start when File is set
	File     string        // saved full output (over the size limit); "" when Output is whole
	Size     int64         // total output bytes when File is set
	Detail   string        // why, for StateFailed
	ForAgent string        // the output as the agent gets it (full, or path + summary)
}

// Status is the one-line outcome.
func (r Result) Status() string {
	switch r.State {
	case StateExited:
		return fmt.Sprintf("exit status %d", r.ExitCode)
	case StateTimedOut:
		return "timed out after " + r.Timeout.String()
	case StateCancelled:
		return "cancelled"
	case StateFailed:
		if r.Detail != "" {
			return "failed: " + r.Detail
		}
		return "failed"
	}
	return r.State
}

// ForAgentText is the message that carries a result to the agent and starts its
// turn.
func (r Result) ForAgentText() string {
	var sb strings.Builder
	sb.WriteString("[The user ran this shell command from chat with `!`. It has finished; reply to its output.]\n$ ")
	sb.WriteString(r.Command)
	sb.WriteString("\n[" + r.Status() + "]\n")
	sb.WriteString(r.ForAgent)
	return sb.String()
}

// displayHeadChars bounds the start of an over-limit output shown in a chat
// message. It fits Discord's 2000 and Telegram's 4096 limits with the command,
// status and path beside it.
const displayHeadChars = 1000

// commandDisplayChars bounds the command line of a text-platform reply.
const commandDisplayChars = 500

func head(s string, maxChars int) string {
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	return string([]rune(s)[:maxChars])
}

// RunningText is the readable form of a command that is still running.
func RunningText(command string) string {
	return "$ " + command + "\n[running]"
}

// DisplayText is the readable form of a result in the app: the whole output
// under the size limit, else its start and where the rest is saved. A client
// that does not know shell chits shows exactly this (#2812 req 13).
func (r Result) DisplayText() string {
	return r.chatText(r.Command)
}

func (r Result) chatText(command string) string {
	var sb strings.Builder
	sb.WriteString("$ " + command + "\n")
	out := r.Output
	if r.File != "" {
		out = head(out, displayHeadChars)
	}
	if out != "" {
		sb.WriteString(out)
		if !strings.HasSuffix(out, "\n") {
			sb.WriteByte('\n')
		}
	}
	if r.File != "" {
		fmt.Fprintf(&sb, "… (%s bytes in all; full output saved to %s)\n", strconv.FormatInt(r.Size, 10), r.File)
	}
	sb.WriteString("[" + r.Status() + "]")
	return sb.String()
}

// ReplyText is the plain-text reply on Telegram, Discord and Slack: the output
// in a code fence so no platform formats it. A long reply under the size limit
// goes through each platform's own message splitter.
func (r Result) ReplyText() string {
	return "```\n" + r.chatText(head(r.Command, commandDisplayChars)) + "\n```"
}

// APITimeout is the limit for a command on an API agent: [tools]
// exec_default_timeout seconds; <=0 is the shell tool's built-in 30 s
// (shell.execCommand). A delegated agent's limit comes from its backend's spec
// (delegator.Spec.BashDefaultTimeout).
func APITimeout(execDefaultSec int) time.Duration {
	if execDefaultSec <= 0 {
		execDefaultSec = 30
	}
	return time.Duration(execDefaultSec) * time.Second
}

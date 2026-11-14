package app

import (
	"context"
	"time"
)

// Running-command descriptions (#3118, Dick's ruling Q48, 2026-11-14: "cmd
// shortened. also send this to the fast summary model to produce a max 6 word
// description of the command's intent"). A shell call with no description of
// its own (every codex shell call; a CC Bash call that sets none) first shows
// the command itself on the activity line (toolformat.ActivityLabel). If the
// command is still running after commandSummaryDelay, the fast-summary path
// (the pin label's model, endpoint and key: useShared, as for the voice-mode
// block sentences) is asked for a description of at most six words, which
// replaces the command on the line while that same call runs. The path off or
// misconfigured, a failed or over-long answer, or a call that ends first:
// the command stays.

// commandSummaryDelay is how long a command must run before its description
// is asked for. Most of codex's shell calls end in well under a second, so
// their line is never seen and no call is spent on them (clutch's default,
// not ruled). A var so a test can shorten it.
var commandSummaryDelay = time.Second

const (
	// commandSummaryWords is the description length the prompt asks for
	// (Dick: "max 6 word description").
	commandSummaryWords = 6
	// commandSummaryMaxTokens is ample for six words.
	commandSummaryMaxTokens = 30
	// commandSummaryMaxInputRunes caps the command sent: a long script is
	// described from its start.
	commandSummaryMaxInputRunes = 2000
)

// commandSummaryPrompt is the system prompt. The examples are Dick's.
const commandSummaryPrompt = "You label what a shell command is for, for a one-line status display. " +
	"The user sends a shell command an AI agent is running. Describe the command's intent in AT MOST SIX WORDS. " +
	"This limit is strict: never more than six words. Lower case, plain words, " +
	"for example \"find markdown files\" or \"query contacts database for John Oliver\". " +
	"Reply with the description only: no quotes, no preamble, no trailing full stop, one line."

var commandSummaryAsk = summaryAsk{
	what:          "command description",
	use:           useShared,
	system:        commandSummaryPrompt,
	maxInputRunes: commandSummaryMaxInputRunes,
	maxTokens:     commandSummaryMaxTokens,
	maxWords:      commandSummaryWords,
	timeout:       pinSummaryTimeout,
}

// commandSummary asks the fast-summary model for a description of command.
// "" with a nil error when the path is off or not usable (no call is made).
// session and agentID name where the call is booked.
func (h *Hub) commandSummary(ctx context.Context, command, session, agentID string) (string, error) {
	if h == nil || !h.pinSum.usable(useShared) {
		return "", nil
	}
	return h.pinSum.ask(ctx, commandSummaryAsk, command, session, agentID)
}

// describeRunningCommand waits delay (commandSummaryDelay, read by the caller)
// and, if tool call seq is still the one running on b, asks describe for a description of command and
// shows it in place of the command while that call still runs. Run in its own
// goroutine: a model call never holds up the turn's event stream.
func describeRunningCommand(b *convBinding, seq uint64, command string, delay time.Duration, describe func(ctx context.Context, command string) (string, error)) {
	time.Sleep(delay)
	if !b.turnToolRunning(seq) {
		return
	}
	desc, err := describe(context.Background(), command)
	if err != nil {
		appLog.Debugf("command description (conv=%s): %v", b.convID, err)
		return
	}
	if desc == "" {
		return
	}
	b.setTurnToolDetail(seq, desc)
}

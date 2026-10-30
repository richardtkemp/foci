package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"foci/internal/memory"
)

// todoDoneUsageLine is the one-line synopsis of `foci todo done`, shared by
// the help text and the shape errors so they cannot drift.
const todoDoneUsageLine = "foci todo done [-a agent] <id> <reason...>"

// todoCLIUsage prints the `foci todo` subcommand list.
func todoCLIUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci todo <subcommand> [args...]

Subcommands:
  done [-a agent] <id> <reason...>   Close one todo of the agent with a reason
                                      (see 'foci todo done -h')
`)
}

// todoDoneUsage prints the help for `foci todo done`.
func todoDoneUsage() {
	fmt.Fprintf(os.Stderr, `Usage: %s

Mark one todo of the named agent done through the gateway, recording the
reason as the todo's close reason (shown by /todo show and the agent's
foci_todo get). The reason is stored byte for byte — any bytes a command
line can carry (the kernel allows 128 KiB per argument and about 2 MiB
across the whole command line); pass it as one quoted argument to keep
it exact.

The global flags (--addr, --api-key, --socket) are read only before the
"todo" command word (so they are for "command", "send", "branch" and
"eval", the other commands whose own arguments are free text; every other
command also reads them after its command word), and -a/--agent only
before <id> (only one — a second complete pair is an error naming the
rule). Everything from the reason onward is reason verbatim,
whatever it looks like: "-a", "--addr", "--api-key sekret" and "-n" are
reason words, stored as typed, never read as flags. Before the id only
-a/--agent is a flag; a connection flag there cannot route the request
and the error says to move it before the todo command word, and any
other word is the id position, named by the id parser. The agent is
required — todo ids are per agent; name it
with -a/--agent or FOCI_AGENT. An empty reason is refused; a reason of
spaces only is not empty — it is accepted and stored as-is, like the
agent todo tool. Exits 0 on success; non-zero with one
stderr line naming the todo if the todo or agent does not exist, the
reason is empty, or the gateway is unreachable (no retry).

Used by services without an agent exec (the factory's landing) to close
their tickets.
`, todoDoneUsageLine)
}

// cmdTodo dispatches `foci todo`'s subcommands. Help is recognised only as
// the first argument — a reason word is allowed to be --help. A bare
// `foci todo` is an error like every argument-less sibling command (it
// prints the usage first); any other shape error is a verdict, not a
// usage dump, as cmdTodoDone's already were.
func cmdTodo(base string, args []string) error {
	if len(args) == 0 {
		todoCLIUsage()
		return fmt.Errorf("todo requires a subcommand (done)")
	}
	if isHelpWord(args[0]) {
		todoCLIUsage()
		return nil
	}
	switch args[0] {
	case "done":
		return cmdTodoDone(base, args[1:])
	default:
		// flagName: an "=value" tail may be a credential; the one error
		// line names the word's head only.
		return fmt.Errorf("unknown subcommand: %s", flagName(args[0]))
	}
}

// todoDoneArgs is the parsed command line of `foci todo done`.
type todoDoneArgs struct {
	agent  string
	id     int64
	reason string
	// help is set when -h/--help appears in the leading flag run — the
	// place where help is a request. A reason word that looks like help
	// is a reason (a.help stays false): `todo done 7 --help` closes #7
	// with the reason "--help".
	help bool
}

// todoDoneLead declares `foci todo done`'s half of the one placement
// scan: the landmark is the todo id (so the duplicate -a error names
// it), a connection flag there names the todo command word, and a
// literal -- may end the flag region so an id that looks like a flag
// can still be named.
var todoDoneLead = leadSpec{word: "todo command", landmark: "id", dashDash: true}

// parseTodoDoneArgs parses `foci todo done`'s command line with the one
// scan (scanCommandLine, main.go): the leading flag region is -a/--agent
// only (one pair — a second complete one is refused by name), and the
// landmark is the todo id. The reason is the remaining argv verbatim,
// one element byte for byte, several words joined with single spaces
// like `foci send`.
// The one placement line (Dick, 2026-10-17): flags are interpreted only
// BEFORE the free text starts, and everything from the reason onward is
// reason verbatim, whatever it looks like — "-a", "--addr", "--api-key
// sekret", "-n" are stored, never read as flags, never refused. Before
// the id only -a/--agent is a flag; any other word there is the id
// position, and the id parser names it (a "-5" is a bad id like any
// other non-digit word) — except a connection flag, which cannot route
// from there and is told where it goes. The server owns the
// agent/reason rules; only shape errors are refused here, as one error
// line.
func parseTodoDoneArgs(args []string) (todoDoneArgs, error) {
	var a todoDoneArgs
	lead, err := scanCommandLine(args, noOwnFlags{}, todoDoneLead, true)
	if err != nil {
		return a, err
	}
	if lead.help {
		a.help = true
		a.agent = lead.agent
		return a, nil
	}
	if len(lead.rest) == 0 {
		return a, fmt.Errorf("usage: %s", todoDoneUsageLine)
	}
	// The id grammar is digits only (memory.ParseTodoID — the grammar the
	// /todo verbs and this command share), so "+7" and "-7" are bad ids,
	// never ticket numbers, and "007" is 7. flagName keeps an "=value"
	// tail (an API key, an address) out of the error line.
	id, ok := memory.ParseTodoID(lead.rest[0])
	if !ok || id <= 0 {
		return a, fmt.Errorf("todo id must be a positive integer, got %q", flagName(lead.rest[0]))
	}
	reason := lead.rest[1:]
	if len(reason) == 0 {
		return a, fmt.Errorf("todo #%d: a reason is required (usage: %s)", id, todoDoneUsageLine)
	}
	a.agent = lead.agent
	a.id = id
	a.reason = strings.Join(reason, " ")
	return a, nil
}

// cmdTodoDone closes one todo of the named agent through POST /todo/done.
// One POST, no dial retry: a landing script needs an exit status and one
// stderr line, not a minute of retry banners while the gateway restarts.
// The reason travels base64-encoded in the wire field reason_base64 (see
// todoDoneRequest) so every byte of the argv element survives, invalid
// UTF-8 included. The body is marshalled here, once — postJSONOnce takes
// it already encoded.
func cmdTodoDone(base string, args []string) error {
	a, err := parseTodoDoneArgs(args)
	if err != nil {
		return err // one line: a shape error is a verdict, not a usage dump
	}
	if a.help {
		todoDoneUsage()
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"agent":         a.agent,
		"id":            a.id,
		"reason_base64": []byte(a.reason),
	})
	if err != nil {
		return err
	}
	if err := postJSONOnce(base+"/todo/done", body); err != nil {
		var te *transportError
		if errors.As(err, &te) {
			err = fmt.Errorf("gateway unreachable: %w", te.err)
		}
		return fmt.Errorf("closing todo #%d for agent %q: %w", a.id, a.agent, err)
	}
	return nil
}

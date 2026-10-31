package main

import (
	"fmt"
	"os"
	"strings"

	"foci/internal/platform"
)

// messageSourceSpecs declares the model override and the explicit
// message source pair `foci send` and `foci branch` share — -m/--model,
// -mt/--message-text and -mf/--message-file — as flagSpecs so the one
// scan, the env defaults and the dangling-flag predicate all read one
// declaration (the gateFlags pattern). The callers hand in their own
// field pointers; the struct shape stays each command's own.
func messageSourceSpecs(model, messageText, messageFile *string) []flagSpec {
	return []flagSpec{
		{"--model", []string{"-m"}, "FOCI_MODEL", "", "model", model},
		{"--message-text", []string{"--mt", "-mt"}, "FOCI_MESSAGE_TEXT", "", "", messageText},
		{"--message-file", []string{"--mf", "-mf"}, "FOCI_MESSAGE_FILE", "", "", messageFile},
	}
}

type sendFlags struct {
	agent       string
	sessionFlag        // -s/--session selector + FOCI_SESSION (#2284)
	model       string // model override (group name, alias, or developer/model_id)
	gateFlags          // ifWarm / ifCold / ifUserActive / ifUserInactive (TODO #753)
	waitFlags          // wait-until gates + --no-gate
	humanFlag          // --human: a human, not a cron, sent this (#1130)
	messageText string // explicit --message-text / -mt
	messageFile string // explicit --message-file / -mf
	async       bool   // fire-and-forget mode
	sync        bool   // wait for response (overrides async)
	broadcast   bool   // deliver the response to every live surface for the agent
	steerFlag   bool   // --steer / FOCI_STEER=steer: fold into a running USER turn (#1318)
	queueFlag   bool   // --queue / FOCI_STEER=queue: fresh turn behind a running one
	// help is set when -h/--help appears in the leading flag run — the
	// place where help is a request. A message word that looks like help
	// is message (help stays false): `foci send hello -h` sends
	// "hello -h".
	help bool
}

// sendLead declares `foci send`'s half of the one placement scan: the
// message text is the free text.
var sendLead = leadSpec{word: "send command", landmark: "text"}

// messageSpecs is send's own message-source declaration (see
// messageSourceSpecs).
func (flags *sendFlags) messageSpecs() []flagSpec {
	return messageSourceSpecs(&flags.model, &flags.messageText, &flags.messageFile)
}

// consumeOwnFlag parses one of `foci send`'s own flags — everything but
// the agent selector the scan itself owns — at args[i].
func (flags *sendFlags) consumeOwnFlag(args []string, i int) (next int, consumed bool) {
	switch args[i] {
	case "--broadcast":
		flags.broadcast = true
		return i, true
	case "--async", "--no-wait":
		flags.async = true
		return i, true
	case "--sync", "--wait":
		flags.sync = true
		return i, true
	case "--steer":
		flags.steerFlag = true
		return i, true
	case "--queue":
		flags.queueFlag = true
		return i, true
	}
	if c, ni := flags.sessionFlag.tryParseSessionArg(args, i); c {
		return ni, true
	}
	if c, ni := flags.gateFlags.tryParseGateArg(args, i); c {
		return ni, true
	}
	if c, ni := flags.waitFlags.tryParseWaitArg(args, i); c {
		return ni, true
	}
	if flags.humanFlag.tryParseHumanArg(args[i]) {
		return i, true
	}
	if c, ni := tryParseFlagArg(flags.messageSpecs(), args, i); c {
		return ni, true
	}
	return i, false
}

// ownValueFlagWord reports the detached spellings of send's own
// value-taking flags (the bare switches --broadcast, --async, --sync,
// --steer, --queue and --human are not value flags).
func (flags *sendFlags) ownValueFlagWord(tok string) bool {
	return isDetachedFlagWord(flags.sessionFlag.specs(), tok) ||
		isDetachedFlagWord(flags.gateFlags.specs(), tok) ||
		isDetachedFlagWord(flags.waitFlags.specs(), tok) ||
		isDetachedFlagWord(flags.messageSpecs(), tok)
}

// applyEnvDefaults applies send's env fallbacks (flag > env > default).
func (flags *sendFlags) applyEnvDefaults() {
	flags.sessionFlag.applyEnvDefault()
	applyFlagEnvDefaults(flags.messageSpecs())
	flags.gateFlags.applyEnvDefaults()
	flags.waitFlags.applyEnvDefaults()
	flags.humanFlag.applyEnvDefault()
	flags.async = envBool(flags.async, "FOCI_ASYNC")
	flags.sync = envBool(flags.sync, "FOCI_SYNC")
	// FOCI_STEER sets the delivery mode ("steer"/"queue"); other values are
	// ignored → absent. Mapped through the one shared wire mapping
	// (platform.SteerPreferenceFromWire, #1318). A flag plus the OTHER
	// mode's env value is a usage error (steerBody below) — two live
	// sources disagreeing is a mistake to surface, not resolve.
	switch platform.SteerPreferenceFromWire(envDefault("", "FOCI_STEER")) {
	case platform.SteerAlways:
		flags.steerFlag = true
	case platform.SteerNever:
		flags.queueFlag = true
	}
}

// steerBody resolves the mutually-exclusive delivery-mode flags (#1318) to
// the wire body's steer value: "steer", "queue", or "" (no choice — the body
// carries no key). Both flags set — directly or flag+env — is a usage error:
// two live sources disagreeing is a mistake to surface, not resolve.
func (f sendFlags) steerBody() (string, error) {
	if f.steerFlag && f.queueFlag {
		return "", fmt.Errorf("--steer and --queue are mutually exclusive")
	}
	if f.steerFlag {
		return "steer", nil
	}
	if f.queueFlag {
		return "queue", nil
	}
	return "", nil
}

// parseSendFlags parses `foci send`'s line with the one scan
// (scanCommandLine, main.go): send's flags are read only before the
// message text starts, and every word from the text on is message
// verbatim, whatever it looks like (the one placement rule, Dick,
// 2026-10-17 — `foci send hello --if-warm 8h` sends "hello --if-warm
// 8h"; the gate belongs before the text). A connection flag between the
// "send" word and the text, a value-flag that ends the line before the
// text, and a second complete -a pair are refused there, naming the
// rule.
func parseSendFlags(args []string) (flags sendFlags, rest []string, err error) {
	lead, err := scanCommandLine(args, &flags, sendLead, true)
	if err != nil {
		return sendFlags{}, nil, err
	}
	flags.agent = lead.agent
	flags.help = lead.help
	flags.applyEnvDefaults()
	return flags, lead.rest, nil
}

// resolveMessage determines the message text from flags and trailing args.
// Priority: --message-text / --message-file / trailing args (implicit -mt).
// Returns error if both -mt and -mf are set, or if the file cannot be read.
func resolveMessage(flags sendFlags, trailingArgs []string) (string, error) {
	if flags.messageText != "" && flags.messageFile != "" {
		return "", fmt.Errorf("cannot specify both --message-text and --message-file")
	}
	if flags.messageFile != "" {
		data, err := os.ReadFile(flags.messageFile)
		if err != nil {
			return "", fmt.Errorf("reading message file: %w", err)
		}
		return string(data), nil
	}
	if flags.messageText != "" {
		return flags.messageText, nil
	}
	if len(trailingArgs) > 0 {
		return strings.Join(trailingArgs, " "), nil
	}
	return "", nil
}

func sendUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci send [-a agent] [-s session] [-m model] [--steer | --queue] [--if-active <dur>] [--if-inactive <dur>] [--if-user-active <dur>] [--if-user-inactive <dur>] [--wait-warm <dur>] [--wait-cold <dur>] [--sync] [-mt text | -mf file] <message>

Send a message to the agent's session.

By default, send is asynchronous (fire-and-forget): the CLI returns immediately
and the agent's response is delivered to the chat. Use --sync/--wait to block
until the agent's reply is available. NOTE: --sync/--wait is NOT one of the
--wait-* gates — it waits for the reply of a send that runs NOW.

Activity gates come in two dispositions on the same conditions (warm/cold/
user-active/user-inactive):
  --if-*   SKIP the send if the condition is not met (evaluated once, now).
  --wait-* DEFER the send until the condition holds, then deliver. Deferred
           sends are persisted (they survive a gateway restart) and are
           always async — an unmet --wait-* gate returns a deferred receipt
           immediately, even with --sync. If the condition never holds,
           --wait-timeout / --deadline (default 2h) sends anyway.
  --if-warm / --if-cold (aliases --if-active / --if-inactive) and their --wait-*
     counterparts consult SESSION cache-warmth — whether THIS session ran a turn
     (any trigger) within the duration; a turn in flight always counts as warm.
  --if-user-* / --wait-user-* consult USER-attention activity — whether the
     user themselves messaged this agent within the duration.

DEFAULT: a send with NO if/wait flag defaults to --wait-cold 1m, so it waits for
the session to be idle 1m before delivering (avoids interleaving with active
work). Use --no-gate to send immediately with no gating. An explicit --steer or
--queue skips only this implicit default; explicit gates still apply.

Delivery mode (#1318): --steer folds the message into a running user turn
(a turn started by a chat message or a choice-carrying send) at its next
tool boundary; an idle session starts at once. A system turn in flight
(cron, keepalive, webhook, a plain send with no mode) differs by backend:
on an API-mode agent it is never folded — the message waits for it to
finish and then runs as its own turn — but on a delegated agent (Claude
Code, opencode, codex) the backend's running turn counts, so the steer
CAN fold into it. --queue runs it as its own fresh turn behind a running
one. Neither mode is consumed as plan feedback or an ask answer unless
--human is given (then --steer can be; --queue never is). The two are
mutually exclusive; a deferred send keeps its mode.

Flag placement — the message is free text: send's flags are read only
BEFORE the message starts (the first word that is none of them starts
the message), and every word from there on is the message, typed
exactly, whatever it looks like — a trailing "--if-warm 8h", "-a
research" or "--api-key sekret" is message text, never a flag, and a
help word inside the message is message (foci send hello -h sends
"hello -h"). Three shapes error before anything is sent: a connection
flag typed between the "send" word and the message ("--addr must come
before the send command word" — it cannot route the request from
there), a value-flag that ends the line before the message starts
("flag --if-warm needs a value"), and a second complete -a/--agent pair
("only one -a/--agent may precede the text").

Flags:
  -a, --agent <id>          Target agent (env: FOCI_AGENT)
  -s, --session <id|alias>  Target session name or a chat alias (env: FOCI_SESSION, default: main)
  --broadcast               Deliver the response to every live surface for the agent (telegram, app, …); with a delivery mode the mode is ignored
  -m, --model <model>       Model override: group name, alias, or developer/model_id (env: FOCI_MODEL)
  --steer                   Redirect the agent now: fold into a running user turn (a running system turn is waited out); idle starts at once (env: FOCI_STEER=steer)
  --queue                   Run as its own fresh turn, behind a running one if any (an idle session starts it at once; env: FOCI_STEER=queue)
  --if-warm <dur>           Skip if this session has not run a turn within duration (env: FOCI_IF_WARM; alias --if-active)
  --if-cold <dur>           Skip if this session has run a turn within duration (env: FOCI_IF_COLD; alias --if-inactive)
  --if-user-active <dur>    Skip if user has not touched this agent within duration (env: FOCI_IF_USER_ACTIVE)
  --if-user-inactive <dur>  Skip if user has touched this agent within duration (env: FOCI_IF_USER_INACTIVE)
  --wait-warm <dur>         Defer until this session is warm (env: FOCI_WAIT_WARM; alias --wait-active)
  --wait-cold <dur>         Defer until this session is cold (env: FOCI_WAIT_COLD; alias --wait-inactive)
  --wait-user-active <dur>  Defer until the user has touched this agent within duration (env: FOCI_WAIT_USER_ACTIVE)
  --wait-user-inactive <dur> Defer until the user has NOT touched this agent within duration (env: FOCI_WAIT_USER_INACTIVE)
  --wait-timeout <dur>      Max wait before sending anyway (default 2h; alias --deadline; env: FOCI_WAIT_TIMEOUT)
  --no-gate                 Send immediately: no wait default, no gating (env: FOCI_NO_GATE)
  --human                   Declare a human sent this: counts as user attention once
                            dispatched; skipped/deferred requests never do (env: FOCI_HUMAN)
  --sync, --wait            Wait for the agent's reply; NOT a --wait-* gate (env: FOCI_SYNC)
  --async, --no-wait        Fire-and-forget (default) (env: FOCI_ASYNC)
  -mt, --message-text       Message text (env: FOCI_MESSAGE_TEXT)
  -mf, --message-file       Read message from file (env: FOCI_MESSAGE_FILE)

Trailing args without a flag are treated as implicit --message-text.
Cannot use both -mt and -mf, or both --steer and --queue.
`)
}

func cmdSend(base string, args []string) error {
	flags, rest, err := parseSendFlags(args)
	if err != nil {
		return err
	}
	if flags.help {
		sendUsage()
		return nil
	}
	steer, err := flags.steerBody()
	if err != nil {
		return err
	}
	text, err := resolveMessage(flags, rest)
	if err != nil {
		return err
	}
	if text == "" {
		return fmt.Errorf("usage: foci send [-a agent] [-s session] [-mt text | -mf file] <message text>")
	}
	// Default async=true unless --sync/--wait or FOCI_SYNC is set
	async := !flags.sync
	if flags.async {
		async = true // explicit --async overrides
	}
	body := map[string]interface{}{"text": text, "async": async}
	if flags.agent != "" {
		body["agent"] = flags.agent
	}
	flags.sessionFlag.addToBody(body)
	if flags.broadcast {
		body["policy"] = "broadcast"
	}
	flags.gateFlags.addToBody(body)
	flags.waitFlags.addToBody(body)
	flags.humanFlag.addToBody(body)
	if flags.model != "" {
		body["model"] = flags.model
	}
	if steer != "" {
		body["steer"] = steer
	}
	return postJSON(base+"/send", body)
}

func branchUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci branch [-a agent] [-s session] [-m model] [--if-active <dur>] [--if-inactive <dur>] [--if-user-active <dur>] [--if-user-inactive <dur>] [--wait-warm <dur>] [--wait-cold <dur>] [--no-compact] [--no-reset-hook] [--oneshot] [--sync] [-mt text | -mf file] [text]

Fork a branch session from the agent's main (default) session, or from the
session -s/--session names. The parent session is not interrupted: a copy of
its context answers.

By default, branch is asynchronous (fire-and-forget): the CLI returns immediately
and the agent's response is delivered to the chat. Use --sync/--wait to block
until the agent's reply is available. NOTE: --sync/--wait is NOT one of the
--wait-* gates — it waits for the reply of a branch that runs NOW.

Activity gates come in two dispositions on the same conditions (warm/cold/
user-active/user-inactive):
  --if-*   SKIP the branch if the condition is not met (evaluated once, now).
  --wait-* DEFER the branch until the condition holds, then run it. Deferred
           requests are persisted (they survive a gateway restart) and are
           always async — an unmet --wait-* gate returns a deferred receipt
           immediately, even with --sync. If the condition never holds,
           --wait-timeout / --deadline (default 2h) runs the branch anyway.
  --if-warm / --if-cold (aliases --if-active / --if-inactive) and their --wait-*
     counterparts consult SESSION cache-warmth — whether the PARENT session ran
     a turn (any trigger) within the duration; a turn in flight always counts
     as warm.
  --if-user-* / --wait-user-* consult USER-attention activity — whether the
     user themselves messaged this agent within the duration.

Unlike send (which defaults to --wait-cold 1m), a branch with NO if/wait flag
runs immediately. Use --no-gate to ignore any wait condition.

Flag placement — the branch text is free text, like send's message:
branch's flags are read only BEFORE the text starts, and every word from
there on is the text, typed exactly, whatever it looks like. The same
three shapes error before anything is sent: a connection flag between
the "branch" word and the text, a value-flag that ends the line before
the text starts, and a second complete -a/--agent pair.

Flags:
  -a, --agent <id>          Target agent (env: FOCI_AGENT)
  -s, --session <id|alias>  Parent session: key, name or chat alias; a name that
                            matches nothing creates a new, empty session (env: FOCI_SESSION, default: the agent's main (default) session)
  -m, --model <model>       Model override: group name, alias, or developer/model_id (env: FOCI_MODEL)
  --if-warm <dur>           Skip if the parent session has not run a turn within duration (env: FOCI_IF_WARM; alias --if-active)
  --if-cold <dur>           Skip if the parent session has run a turn within duration (env: FOCI_IF_COLD; alias --if-inactive)
  --if-user-active <dur>    Skip if user has not touched this agent within duration (env: FOCI_IF_USER_ACTIVE)
  --if-user-inactive <dur>  Skip if user has touched this agent within duration (env: FOCI_IF_USER_INACTIVE)
  --wait-warm <dur>         Defer until the parent session is warm (env: FOCI_WAIT_WARM; alias --wait-active)
  --wait-cold <dur>         Defer until the parent session is cold (env: FOCI_WAIT_COLD; alias --wait-inactive)
  --wait-user-active <dur>  Defer until the user has touched this agent within duration (env: FOCI_WAIT_USER_ACTIVE)
  --wait-user-inactive <dur> Defer until the user has NOT touched this agent within duration (env: FOCI_WAIT_USER_INACTIVE)
  --wait-timeout <dur>      Max wait before running anyway (default 2h; alias --deadline; env: FOCI_WAIT_TIMEOUT)
  --no-gate                 Run now, ignoring any wait condition (env: FOCI_NO_GATE)
  --human                   Declare a human sent this: counts as user attention on the
                            new branch session once it runs (env: FOCI_HUMAN)
  --no-compact              Skip compaction if context limit reached (env: FOCI_NO_COMPACT)
  --no-reset-hook           Skip pre-reset memory hook (env: FOCI_NO_RESET_HOOK)
  --oneshot                 Shorthand for --no-compact --no-reset-hook --silent
                            (env: FOCI_ONESHOT sets only --no-compact --no-reset-hook)
  --silent                  No chat delivery of the branch response
  --sync, --wait            Wait for the agent's reply; NOT a --wait-* gate (env: FOCI_SYNC)
  --async, --no-wait        Fire-and-forget (default) (env: FOCI_ASYNC)
  -mt, --message-text       Branch text (env: FOCI_MESSAGE_TEXT)
  -mf, --message-file       Read branch text from file (env: FOCI_MESSAGE_FILE)

`)
}

// branchFlags is the parsed flag set of `foci branch`: the shared if/wait
// gate flag sets, the -s/--session parent selector, the branch-specific
// options, model override, sync/async and message source. Consumed by the
// one scan (scanCommandLine, main.go) through its ownFlagLine methods, so
// the flag handling is testable in-process (the parseSendFlags pattern).
type branchFlags struct {
	gateFlags
	waitFlags
	humanFlag
	sessionFlag // -s/--session parent selector + FOCI_SESSION (#2284)
	noCompact   bool
	noResetHook bool
	silent      bool
	model       string
	messageText string
	messageFile string
	async       bool
	sync        bool
	agent       string
	// help is set when -h/--help appears in the leading flag run; a help
	// word inside the branch text is text.
	help bool
}

// branchLead declares `foci branch`'s half of the one placement scan:
// the optional branch text is the free text.
var branchLead = leadSpec{word: "branch command", landmark: "text"}

// messageSpecs is branch's own message-source declaration (see
// messageSourceSpecs).
func (flags *branchFlags) messageSpecs() []flagSpec {
	return messageSourceSpecs(&flags.model, &flags.messageText, &flags.messageFile)
}

// consumeOwnFlag parses one of `foci branch`'s own flags — everything
// but the agent selector the scan itself owns — at args[i].
//
// Note the deliberate --oneshot asymmetry, preserved verbatim from the
// original inline parsing: the --oneshot FLAG sets no-compact +
// no-reset-hook + silent, while the FOCI_ONESHOT env var sets only
// no-compact + no-reset-hook (no silent).
func (flags *branchFlags) consumeOwnFlag(args []string, i int) (next int, consumed bool) {
	switch args[i] {
	case "--no-compact":
		flags.noCompact = true
		return i, true
	case "--no-reset-hook":
		flags.noResetHook = true
		return i, true
	case "--silent":
		flags.silent = true
		return i, true
	case "--oneshot":
		flags.noCompact = true
		flags.noResetHook = true
		flags.silent = true
		return i, true
	case "--async", "--no-wait":
		flags.async = true
		return i, true
	case "--sync", "--wait":
		flags.sync = true
		return i, true
	}
	if c, ni := flags.gateFlags.tryParseGateArg(args, i); c {
		return ni, true
	}
	if c, ni := flags.waitFlags.tryParseWaitArg(args, i); c {
		return ni, true
	}
	if flags.humanFlag.tryParseHumanArg(args[i]) {
		return i, true
	}
	if c, ni := flags.sessionFlag.tryParseSessionArg(args, i); c {
		return ni, true
	}
	if c, ni := tryParseFlagArg(flags.messageSpecs(), args, i); c {
		return ni, true
	}
	return i, false
}

// ownValueFlagWord reports the detached spellings of branch's own
// value-taking flags (the bare switches --no-compact, --no-reset-hook,
// --silent, --oneshot, --async, --sync and --human are not value flags).
func (flags *branchFlags) ownValueFlagWord(tok string) bool {
	return isDetachedFlagWord(flags.gateFlags.specs(), tok) ||
		isDetachedFlagWord(flags.waitFlags.specs(), tok) ||
		isDetachedFlagWord(flags.sessionFlag.specs(), tok) ||
		isDetachedFlagWord(flags.messageSpecs(), tok)
}

// applyEnvDefaults applies branch's env fallbacks (flag > env > default).
func (flags *branchFlags) applyEnvDefaults() {
	flags.noCompact = envBool(flags.noCompact, "FOCI_NO_COMPACT")
	flags.noResetHook = envBool(flags.noResetHook, "FOCI_NO_RESET_HOOK")
	if envBool(false, "FOCI_ONESHOT") {
		flags.noCompact = true
		flags.noResetHook = true
	}
	flags.model = envDefault(flags.model, "FOCI_MODEL")
	flags.async = envBool(flags.async, "FOCI_ASYNC")
	flags.sync = envBool(flags.sync, "FOCI_SYNC")
	flags.sessionFlag.applyEnvDefault()
	flags.gateFlags.applyEnvDefaults()
	flags.waitFlags.applyEnvDefaults()
	flags.humanFlag.applyEnvDefault()
	flags.messageText = envDefault(flags.messageText, "FOCI_MESSAGE_TEXT")
	flags.messageFile = envDefault(flags.messageFile, "FOCI_MESSAGE_FILE")
}

// parseBranchFlags parses `foci branch`'s line with the one scan
// (scanCommandLine, main.go): branch's flags are read only before the
// branch text starts, and every word from the text on is text verbatim
// (the one placement rule, Dick, 2026-10-17).
func parseBranchFlags(args []string) (flags branchFlags, rest []string, err error) {
	lead, err := scanCommandLine(args, &flags, branchLead, true)
	if err != nil {
		return branchFlags{}, nil, err
	}
	flags.agent = lead.agent
	flags.help = lead.help
	flags.applyEnvDefaults()
	return flags, lead.rest, nil
}

func cmdBranch(base string, args []string) error {
	flags, filtered, err := parseBranchFlags(args)
	if err != nil {
		return err
	}
	if flags.help {
		branchUsage()
		return nil
	}

	sf := sendFlags{messageText: flags.messageText, messageFile: flags.messageFile}
	text, err := resolveMessage(sf, filtered)
	if err != nil {
		return err
	}

	// Default async=true unless --sync/--wait or FOCI_SYNC is set
	async := !flags.sync
	if flags.async {
		async = true // explicit --async overrides
	}

	body := map[string]interface{}{"async": async}
	if flags.agent != "" {
		body["agent"] = flags.agent
	}
	flags.sessionFlag.addToBody(body)
	if text != "" {
		body["text"] = text
	}
	if flags.noCompact {
		body["no_compact"] = true
	}
	if flags.noResetHook {
		body["no_reset_hook"] = true
	}
	flags.gateFlags.addToBody(body)
	flags.waitFlags.addToBody(body)
	flags.humanFlag.addToBody(body)
	if flags.silent {
		body["silent"] = true
	}
	if flags.model != "" {
		body["model"] = flags.model
	}
	return postJSON(base+"/branch", body)
}

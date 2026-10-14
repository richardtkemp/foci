package main

import (
	"fmt"
	"os"
	"strings"

	"foci/internal/platform"
)

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

func parseSendFlags(args []string) (flags sendFlags, rest []string) {
	var filtered []string
	for i := 0; i < len(args); i++ {
		consumed := false
		if args[i] == "-a" || args[i] == "--agent" {
			if i+1 < len(args) {
				flags.agent = args[i+1]
				i++
				consumed = true
			}
		} else if args[i] == "--broadcast" {
			flags.broadcast = true
			consumed = true
		} else if c, ni := flags.sessionFlag.tryParseSessionArg(args, i); c {
			i = ni
			consumed = true
		} else if strings.HasPrefix(args[i], "--agent=") {
			flags.agent = args[i][len("--agent="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "-a=") {
			flags.agent = args[i][len("-a="):]
			consumed = true
		} else if args[i] == "-m" || args[i] == "--model" {
			if i+1 < len(args) {
				flags.model = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--model=") {
			flags.model = args[i][len("--model="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "-m=") {
			flags.model = args[i][len("-m="):]
			consumed = true
		} else if c, ni := flags.gateFlags.tryParseGateArg(args, i); c {
			i = ni
			consumed = true
		} else if c, ni := flags.waitFlags.tryParseWaitArg(args, i); c {
			i = ni
			consumed = true
		} else if c, ni := flags.humanFlag.tryParseHumanArg(args, i); c {
			i = ni
			consumed = true
		} else if args[i] == "--message-text" || args[i] == "--mt" || args[i] == "-mt" {
			if i+1 < len(args) {
				flags.messageText = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--message-text=") {
			flags.messageText = args[i][len("--message-text="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "--mt=") || strings.HasPrefix(args[i], "-mt=") {
			flags.messageText = args[i][strings.Index(args[i], "=")+1:]
			consumed = true
		} else if args[i] == "--message-file" || args[i] == "--mf" || args[i] == "-mf" {
			if i+1 < len(args) {
				flags.messageFile = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--message-file=") {
			flags.messageFile = args[i][len("--message-file="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "--mf=") || strings.HasPrefix(args[i], "-mf=") {
			flags.messageFile = args[i][strings.Index(args[i], "=")+1:]
			consumed = true
		} else if args[i] == "--async" || args[i] == "--no-wait" {
			flags.async = true
			consumed = true
		} else if args[i] == "--sync" || args[i] == "--wait" {
			flags.sync = true
			consumed = true
		} else if args[i] == "--steer" {
			flags.steerFlag = true
			consumed = true
		} else if args[i] == "--queue" {
			flags.queueFlag = true
			consumed = true
		}
		if !consumed {
			filtered = append(filtered, args[i])
		}
	}
	// Apply env var fallbacks (flag > env > default)
	flags.agent = envDefault(flags.agent, "FOCI_AGENT")
	flags.sessionFlag.applyEnvDefault()
	flags.model = envDefault(flags.model, "FOCI_MODEL")
	flags.gateFlags.applyEnvDefaults()
	flags.waitFlags.applyEnvDefaults()
	flags.humanFlag.applyEnvDefault()
	flags.messageText = envDefault(flags.messageText, "FOCI_MESSAGE_TEXT")
	flags.messageFile = envDefault(flags.messageFile, "FOCI_MESSAGE_FILE")
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
	return flags, filtered
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

Flags:
  -a, --agent <id>          Target agent (env: FOCI_AGENT)
  -s, --session <id|alias>  Target session name or a chat alias (env: FOCI_SESSION, default: main)
  --broadcast               Deliver the response to every live surface for the agent (telegram, app, …); with a delivery mode the mode is ignored
  -m, --model <model>       Model override: group name, alias, or developer/model_id (env: FOCI_MODEL)
  --steer                   Redirect the agent now: fold into a running user turn (a running system turn too on delegated agents; on API agents it is waited out); idle starts at once (env: FOCI_STEER=steer)
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
	if wantsHelp(args) {
		sendUsage()
		return nil
	}
	flags, args := parseSendFlags(args)
	steer, err := flags.steerBody()
	if err != nil {
		return err
	}
	text, err := resolveMessage(flags, args)
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

// branchFlags is the parsed flag set of `foci branch`: the shared if/wait gate
// flag sets, the -s/--session parent selector, and the branch-specific
// options, model override, sync/async and message source. Parsed by
// parseBranchFlags so the flag handling is testable in-process (the
// parseSendFlags pattern).
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
}

// parseBranchFlags consumes `foci branch`'s flags (and their env defaults)
// from args, returning the flags and the remaining positional text words.
//
// Note the deliberate --oneshot asymmetry, preserved verbatim from the
// original inline parsing: the --oneshot FLAG sets no-compact + no-reset-hook
// + silent, while the FOCI_ONESHOT env var sets only no-compact +
// no-reset-hook (no silent).
func parseBranchFlags(args []string) (flags branchFlags, rest []string) {
	var filtered []string
	for i := 0; i < len(args); i++ {
		if c, ni := flags.gateFlags.tryParseGateArg(args, i); c {
			i = ni
			continue
		}
		if c, ni := flags.waitFlags.tryParseWaitArg(args, i); c {
			i = ni
			continue
		}
		if c, ni := flags.humanFlag.tryParseHumanArg(args, i); c {
			i = ni
			continue
		}
		if c, ni := flags.sessionFlag.tryParseSessionArg(args, i); c {
			i = ni
			continue
		}
		switch {
		case args[i] == "--no-compact":
			flags.noCompact = true
		case args[i] == "--no-reset-hook":
			flags.noResetHook = true
		case args[i] == "--silent":
			flags.silent = true
		case args[i] == "--oneshot":
			flags.noCompact = true
			flags.noResetHook = true
			flags.silent = true
		case args[i] == "--async" || args[i] == "--no-wait":
			flags.async = true
		case args[i] == "--sync" || args[i] == "--wait":
			flags.sync = true
		case (args[i] == "-m" || args[i] == "--model") && i+1 < len(args):
			flags.model = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--model="):
			flags.model = args[i][len("--model="):]
		case strings.HasPrefix(args[i], "-m="):
			flags.model = args[i][len("-m="):]
		case (args[i] == "--message-text" || args[i] == "--mt" || args[i] == "-mt") && i+1 < len(args):
			flags.messageText = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--message-text="):
			flags.messageText = args[i][len("--message-text="):]
		case strings.HasPrefix(args[i], "--mt=") || strings.HasPrefix(args[i], "-mt="):
			flags.messageText = args[i][strings.Index(args[i], "=")+1:]
		case (args[i] == "--message-file" || args[i] == "--mf" || args[i] == "-mf") && i+1 < len(args):
			flags.messageFile = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--message-file="):
			flags.messageFile = args[i][len("--message-file="):]
		case strings.HasPrefix(args[i], "--mf=") || strings.HasPrefix(args[i], "-mf="):
			flags.messageFile = args[i][strings.Index(args[i], "=")+1:]
		default:
			filtered = append(filtered, args[i])
		}
	}
	// Apply env var fallbacks (flag > env > default)
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
	return flags, filtered
}

func cmdBranch(base string, args []string) error {
	if wantsHelp(args) {
		branchUsage()
		return nil
	}
	agent, args := parseAgentFlag(args)
	flags, filtered := parseBranchFlags(args)

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
	if agent != "" {
		body["agent"] = agent
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

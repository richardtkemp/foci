package main

import (
	"fmt"
	"os"
	"strings"
)

type sendFlags struct {
	agent       string
	session     string
	model       string // model override (group name, alias, or developer/model_id)
	gateFlags          // ifWarm / ifCold / ifUserActive / ifUserInactive (TODO #753)
	messageText string // explicit --message-text / -mt
	messageFile string // explicit --message-file / -mf
	async       bool   // fire-and-forget mode
	sync        bool   // wait for response (overrides async)
	broadcast   bool   // deliver the response to every live surface for the agent
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
		} else if args[i] == "-s" || args[i] == "--session" {
			if i+1 < len(args) {
				flags.session = args[i+1]
				i++
				consumed = true
			}
		} else if strings.HasPrefix(args[i], "--agent=") {
			flags.agent = args[i][len("--agent="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "-a=") {
			flags.agent = args[i][len("-a="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "--session=") {
			flags.session = args[i][len("--session="):]
			consumed = true
		} else if strings.HasPrefix(args[i], "-s=") {
			flags.session = args[i][len("-s="):]
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
		}
		if !consumed {
			filtered = append(filtered, args[i])
		}
	}
	// Apply env var fallbacks (flag > env > default)
	flags.agent = envDefault(flags.agent, "FOCI_AGENT")
	flags.session = envDefault(flags.session, "FOCI_SESSION")
	flags.model = envDefault(flags.model, "FOCI_MODEL")
	flags.gateFlags.applyEnvDefaults()
	flags.messageText = envDefault(flags.messageText, "FOCI_MESSAGE_TEXT")
	flags.messageFile = envDefault(flags.messageFile, "FOCI_MESSAGE_FILE")
	flags.async = envBool(flags.async, "FOCI_ASYNC")
	flags.sync = envBool(flags.sync, "FOCI_SYNC")
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
	fmt.Fprintf(os.Stderr, `Usage: foci send [-a agent] [-s session] [-m model] [--if-active <dur>] [--if-inactive <dur>] [--if-user-active <dur>] [--if-user-inactive <dur>] [--sync] [-mt text | -mf file] <message>

Send a message to the agent's session.

By default, send is asynchronous (fire-and-forget): the CLI returns immediately
and the agent's response is delivered to the chat. Use --sync/--wait to block
until the response is available.

Activity gates (TODO #753):
  --if-warm / --if-cold consult SESSION cache-warmth — whether THIS session ran a
    turn (any trigger) within the duration. A turn currently in flight always
    counts as warm. Use these for keepalives that should yield to running work.
    (Aliases: --if-active / --if-inactive.)
  --if-user-active / --if-user-inactive consult USER-attention activity — whether
    the user themselves messaged this agent within the duration. Use these for
    nudges that should only fire when the user is engaged (or away).

Flags:
  -a, --agent <id>          Target agent (env: FOCI_AGENT)
  -s, --session <id|alias>  Target session name or a chat alias (env: FOCI_SESSION, default: main)
  --broadcast               Deliver the response to every live surface for the agent (telegram, app, …)
  -m, --model <model>       Model override: group name, alias, or developer/model_id (env: FOCI_MODEL)
  --if-warm <dur>           Skip if this session has not run a turn within duration (env: FOCI_IF_WARM; alias --if-active)
  --if-cold <dur>           Skip if this session has run a turn within duration (env: FOCI_IF_COLD; alias --if-inactive)
  --if-user-active <dur>    Skip if user has not touched this agent within duration (env: FOCI_IF_USER_ACTIVE)
  --if-user-inactive <dur>  Skip if user has touched this agent within duration (env: FOCI_IF_USER_INACTIVE)
  --sync, --wait            Wait for the response (env: FOCI_SYNC)
  --async, --no-wait        Fire-and-forget (default) (env: FOCI_ASYNC)
  -mt, --message-text       Message text (env: FOCI_MESSAGE_TEXT)
  -mf, --message-file       Read message from file (env: FOCI_MESSAGE_FILE)

Trailing args without a flag are treated as implicit --message-text.
Cannot use both -mt and -mf.
`)
}

func cmdSend(base string, args []string) error {
	if wantsHelp(args) {
		sendUsage()
		return nil
	}
	flags, args := parseSendFlags(args)
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
	if flags.session != "" {
		body["session"] = flags.session
	}
	if flags.broadcast {
		body["policy"] = "broadcast"
	}
	flags.gateFlags.addToBody(body)
	if flags.model != "" {
		body["model"] = flags.model
	}
	return postJSON(base+"/send", body)
}

func branchUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci branch [-a agent] [-m model] [--if-active <dur>] [--if-inactive <dur>] [--if-user-active <dur>] [--if-user-inactive <dur>] [--no-compact] [--no-reset-hook] [--oneshot] [--sync] [-mt text | -mf file] [text]

Fork a branch session from the agent's main chat.

By default, branch is asynchronous (fire-and-forget): the CLI returns immediately
and the agent's response is delivered to the chat. Use --sync/--wait to block
until the response is available.

Activity gates (TODO #753):
  --if-warm / --if-cold consult SESSION cache-warmth (whether the target session
    ran a turn within the duration; a turn in flight always counts).
    (Aliases: --if-active / --if-inactive.)
  --if-user-active / --if-user-inactive consult USER-attention activity (whether
    the user touched this agent within the duration).

Flags:
  -a, --agent <id>          Target agent (env: FOCI_AGENT)
  -m, --model <model>       Model override: group name, alias, or developer/model_id (env: FOCI_MODEL)
  --if-warm <dur>           Skip if target session has not run a turn within duration (env: FOCI_IF_WARM; alias --if-active)
  --if-cold <dur>           Skip if target session has run a turn within duration (env: FOCI_IF_COLD; alias --if-inactive)
  --if-user-active <dur>    Skip if user has not touched this agent within duration (env: FOCI_IF_USER_ACTIVE)
  --if-user-inactive <dur>  Skip if user has touched this agent within duration (env: FOCI_IF_USER_INACTIVE)
  --no-compact              Skip compaction if context limit reached (env: FOCI_NO_COMPACT)
  --no-reset-hook           Skip pre-reset memory hook (env: FOCI_NO_RESET_HOOK)
  --oneshot                 Shorthand for --no-compact --no-reset-hook (env: FOCI_ONESHOT)
  --sync, --wait            Wait for the response (env: FOCI_SYNC)
  --async, --no-wait        Fire-and-forget (default) (env: FOCI_ASYNC)
  -mt, --message-text       Message text (env: FOCI_MESSAGE_TEXT)
  -mf, --message-file       Read message from file (env: FOCI_MESSAGE_FILE)

`)
}

func cmdBranch(base string, args []string) error {
	if wantsHelp(args) {
		branchUsage()
		return nil
	}
	agent, args := parseAgentFlag(args)
	noCompact := false
	noResetHook := false
	silent := false
	asyncFlag := false
	syncFlag := false
	model := ""
	var gf gateFlags
	messageText := ""
	messageFile := ""
	var filtered []string
	for i := 0; i < len(args); i++ {
		if c, ni := gf.tryParseGateArg(args, i); c {
			i = ni
			continue
		}
		switch {
		case args[i] == "--no-compact":
			noCompact = true
		case args[i] == "--no-reset-hook":
			noResetHook = true
		case args[i] == "--silent":
			silent = true
		case args[i] == "--oneshot":
			noCompact = true
			noResetHook = true
			silent = true
		case args[i] == "--async" || args[i] == "--no-wait":
			asyncFlag = true
		case args[i] == "--sync" || args[i] == "--wait":
			syncFlag = true
		case (args[i] == "-m" || args[i] == "--model") && i+1 < len(args):
			model = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--model="):
			model = args[i][len("--model="):]
		case strings.HasPrefix(args[i], "-m="):
			model = args[i][len("-m="):]
		case (args[i] == "--message-text" || args[i] == "--mt" || args[i] == "-mt") && i+1 < len(args):
			messageText = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--message-text="):
			messageText = args[i][len("--message-text="):]
		case strings.HasPrefix(args[i], "--mt=") || strings.HasPrefix(args[i], "-mt="):
			messageText = args[i][strings.Index(args[i], "=")+1:]
		case (args[i] == "--message-file" || args[i] == "--mf" || args[i] == "-mf") && i+1 < len(args):
			messageFile = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--message-file="):
			messageFile = args[i][len("--message-file="):]
		case strings.HasPrefix(args[i], "--mf=") || strings.HasPrefix(args[i], "-mf="):
			messageFile = args[i][strings.Index(args[i], "=")+1:]
		default:
			filtered = append(filtered, args[i])
		}
	}
	// Apply env var fallbacks for branch-specific flags
	noCompact = envBool(noCompact, "FOCI_NO_COMPACT")
	noResetHook = envBool(noResetHook, "FOCI_NO_RESET_HOOK")
	if envBool(false, "FOCI_ONESHOT") {
		noCompact = true
		noResetHook = true
	}
	model = envDefault(model, "FOCI_MODEL")
	asyncFlag = envBool(asyncFlag, "FOCI_ASYNC")
	syncFlag = envBool(syncFlag, "FOCI_SYNC")
	gf.applyEnvDefaults()
	messageText = envDefault(messageText, "FOCI_MESSAGE_TEXT")
	messageFile = envDefault(messageFile, "FOCI_MESSAGE_FILE")

	// Default async=true unless --sync/--wait or FOCI_SYNC is set
	async := !syncFlag
	if asyncFlag {
		async = true // explicit --async overrides
	}

	sf := sendFlags{messageText: messageText, messageFile: messageFile}
	text, err := resolveMessage(sf, filtered)
	if err != nil {
		return err
	}
	body := map[string]interface{}{"async": async}
	if agent != "" {
		body["agent"] = agent
	}
	if text != "" {
		body["text"] = text
	}
	if noCompact {
		body["no_compact"] = true
	}
	if noResetHook {
		body["no_reset_hook"] = true
	}
	gf.addToBody(body)
	if silent {
		body["silent"] = true
	}
	if model != "" {
		body["model"] = model
	}
	return postJSON(base+"/branch", body)
}

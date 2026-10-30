package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"foci/internal/fociroot"
	"foci/internal/secrets"
)

func statusUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci status [-a agent]

Query agent status (session info, model, uptime).

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)
`)
}

func cmdStatus(base string, args []string) error {
	if wantsHelp(args) {
		statusUsage()
		return nil
	}
	agent, _ := parseAgentFlag(args)
	url := base + "/status"
	if agent != "" {
		url += "?agent=" + agent
	}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return printResponse(resp)
}

// evalUsageLine is the one-line synopsis of `foci eval`, shared by the
// help text and the shape error so they cannot drift.
const evalUsageLine = "foci eval [-a agent] <shell command>"

// evalLead declares eval's half of the one placement scan: the shell
// command is free text (it may look like any flag — `foci eval grep -a
// foo file.txt` must run `grep -a foo file.txt`, not select agent foo),
// so the shared agent selector is its only leading flag.
var evalLead = leadSpec{word: "eval command", landmark: "text"}

func evalUsage() {
	fmt.Fprintf(os.Stderr, `Usage: %s

Ask the agent to run a shell command and show output.

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)

The shell command is free text: it starts at the first word that is not
-a/--agent, and every word from there on is the command, verbatim — a
trailing "-a foo" or "--if-warm 8h" is command text, never a flag. The
connection flags (--addr, --api-key, --socket) are read only before the
"eval" word.
`, evalUsageLine)
}

func cmdEval(base string, args []string) error {
	lead, err := scanCommandLine(args, noOwnFlags{}, evalLead, true)
	if err != nil {
		return err
	}
	if lead.help {
		evalUsage()
		return nil
	}
	if len(lead.rest) == 0 {
		return fmt.Errorf("usage: %s", evalUsageLine)
	}
	cmd := strings.Join(lead.rest, " ")
	text := fmt.Sprintf("Run this command and show the output:\n```\n%s\n```", cmd)
	body := map[string]string{"text": text}
	if lead.agent != "" {
		body["agent"] = lead.agent
	}
	return postJSON(base+"/send", body)
}

// commandUsageLine is the one-line synopsis of `foci command`, shared by
// the help text and the shape error so they cannot drift.
const commandUsageLine = "foci command [-a agent] [-s session] [gate flags] </cmd> [args]"

func commandUsage() {
	fmt.Fprintf(os.Stderr, `Usage: %s

Dispatch a slash command via the gateway (bypasses agent conversation).

Flags:
  -a, --agent <id>          Target agent (env: FOCI_AGENT)
  -s, --session <id|alias>  Target session the command runs on: full session key,
                            session name, or chat alias; a name nothing matches
                            creates a new, empty session. Unlike send, foci
                            command does not read FOCI_SESSION — with no -s, the
                            command runs on the agent's default session

Activity gates come in two dispositions on the same conditions, evaluated
against the session the command targets (-s names it; otherwise the agent's
default session — a turn in flight always counts as warm):
  --if-warm <dur>           Skip unless this session ran a turn within duration (env: FOCI_IF_WARM; alias --if-active)
  --if-cold <dur>           Skip if this session ran a turn within duration (env: FOCI_IF_COLD; alias --if-inactive)
  --if-user-active <dur>    Skip unless the user touched this agent within duration (env: FOCI_IF_USER_ACTIVE)
  --if-user-inactive <dur>  Skip if the user touched this agent within duration (env: FOCI_IF_USER_INACTIVE)
  --wait-warm <dur>         Defer until this session is warm (env: FOCI_WAIT_WARM; alias --wait-active)
  --wait-cold <dur>         Defer until this session is cold (env: FOCI_WAIT_COLD; alias --wait-inactive)
  --wait-user-active <dur>  Defer until the user has touched this agent within duration (env: FOCI_WAIT_USER_ACTIVE)
  --wait-user-inactive <dur> Defer until the user has NOT touched this agent within duration (env: FOCI_WAIT_USER_INACTIVE)
  --wait-timeout <dur>      Max wait before running anyway (default 2h; alias --deadline; env: FOCI_WAIT_TIMEOUT)
  --no-gate                 Run now, ignoring any wait condition (env: FOCI_NO_GATE)
  --human                   Declare a human sent this: counts as user attention on the
                            command's session once dispatched (env: FOCI_HUMAN)

--wait-* DEFER the command until the condition holds, then dispatch it.
Deferred requests are persisted (they survive a gateway restart) and are
always async — an unmet --wait-* gate returns a deferred receipt immediately.
If the condition never holds, --wait-timeout / --deadline (default 2h) runs
the command anyway. Unlike send (which defaults to --wait-cold 1m), a command
with NO if/wait flag runs immediately.

Every flag above is read only BEFORE the slash command's words — move it
in front of the text — and the connection flags (--addr, --api-key,
--socket) only before the "command" word itself. From the first word that
is none of those flags on, everything is the dispatched text, typed
exactly, whatever it looks like: a trailing "--if-warm 8h" or "-a b"
after the text has started is command text, not a flag, and is
dispatched as-is (foci command /todo done 10 -a b closes the default
agent's todo 10 with the reason "-a b"). Three shapes error before any
dispatch: a connection flag typed between the "command" word and the
slash text (it cannot route the request from there), a value-flag that
ends the line before the text starts ("-s", "--if-warm" with no value to
take), and a second complete -a/--agent pair before the text (only one
agent may lead). Several words are
joined with single spaces — pass the slash command as ONE quoted
argument when the exact spacing matters. One rule for every command
whose own argument is free text: send's message, branch's text, eval's
shell command and todo done's reason carry it too (see their -h). The
ping and pair shorthands share this scan but have no free text: their
flags are read wherever they sit, and a valueless flag word that ends
their line stays a word, dispatched as typed.

Example (overnight reset that runs once the session has been cold for 55m,
instead of skipping and hoping the next cron tick lands):
  foci command --wait-cold 55m -a helen /reset
`, commandUsageLine)
}

// commandFlags is the parsed flag set of `foci command`: the shared if/wait
// gate flag sets, the human declaration and the session selector. Everything
// else is the command string itself. Consumed by the one scan
// (scanCommandLine, main.go) through its ownFlagLine methods, so the flag
// handling is testable in-process (the parseSendFlags pattern).
type commandFlags struct {
	gateFlags
	waitFlags
	humanFlag
	sessionFlag // -s/--session target (#2296) — no FOCI_SESSION default, see applyEnvDefaults
}

// commandLead declares the command family's half of the one placement
// scan; ping and pair share it (their own flags are the same set).
var commandLead = leadSpec{word: "command", landmark: "text"}

// consumeOwnFlag parses one of the command family's own flags — the
// session selector, the activity gates, the wait gates and --human;
// -a/--agent belongs to the scan itself — at args[i].
func (flags *commandFlags) consumeOwnFlag(args []string, i int) (next int, consumed bool) {
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
	return i, false
}

// ownValueFlagWord reports the detached spellings of the command
// family's own value-taking flags (the bare switches --no-gate and
// --human are not value flags) — the words whose value is missing when
// they end the line where the free text would start.
func (flags *commandFlags) ownValueFlagWord(tok string) bool {
	return isDetachedFlagWord(flags.gateFlags.specs(), tok) ||
		isDetachedFlagWord(flags.waitFlags.specs(), tok) ||
		isDetachedFlagWord(flags.sessionFlag.specs(), tok)
}

// applyEnvDefaults applies the command family's env defaults and the one
// it deliberately skips: unlike send and branch, the command family must
// not inherit FOCI_SESSION — a cron or script that happens to have it
// exported keeps dispatching on the agent's default session (#2296
// ruling). The agent fallback (FOCI_AGENT) is applied by the scan.
func (flags *commandFlags) applyEnvDefaults() {
	flags.gateFlags.applyEnvDefaults()
	flags.waitFlags.applyEnvDefaults()
	flags.humanFlag.applyEnvDefault()
}

// dispatchCommand POSTs one dispatched slash command: the text words
// joined into the command string (a leading "/" added when the caller
// did not type one) with the parsed flags and agent selector. Both
// dispositions of the one scan end here, so the /command wire contract
// exists once.
func dispatchCommand(base string, flags commandFlags, agent string, text []string) error {
	cmd := strings.Join(text, " ")
	if !strings.HasPrefix(cmd, "/") {
		cmd = "/" + cmd
	}
	body := map[string]interface{}{"command": cmd}
	if agent != "" {
		body["agent"] = agent
	}
	flags.gateFlags.addToBody(body)
	flags.waitFlags.addToBody(body)
	flags.humanFlag.addToBody(body)
	flags.sessionFlag.addToBody(body)
	return postJSON(base+"/command", body)
}

// cmdCommand dispatches a slash command via POST /command. Its own
// argument is free text — a dispatched slash command carries arbitrary
// words (a /todo close reason may look like any flag) — which is why
// `command` registers with freeTextCommands (main.go): main reads its
// global flags only from the leading run before the "command" word. The
// one scan (scanCommandLine, main.go) owns the whole placement rule: it
// finds where the text starts, refuses the words that cannot start it (a
// connection flag, a valueless own flag, a second agent pair), and from
// there every word is dispatched typed exactly, whatever it looks like.
func cmdCommand(base string, args []string) error {
	var flags commandFlags
	lead, err := scanCommandLine(args, &flags, commandLead, true)
	if err != nil {
		return err
	}
	if lead.help {
		commandUsage()
		return nil
	}
	if len(lead.rest) == 0 {
		return fmt.Errorf("usage: %s", commandUsageLine)
	}
	flags.applyEnvDefaults()
	return dispatchCommand(base, flags, lead.agent, lead.rest)
}

// cmdPing dispatches the `foci ping` shorthand: /ping takes no
// arguments, so its line is flags only — no free text to protect — and
// keeps the historical whole-line disposition of the one scan; the /ping
// word lands after them.
func cmdPing(base string, args []string) error {
	if wantsHelp(args) {
		pingUsage()
		return nil
	}
	var flags commandFlags
	lead, err := scanCommandLine(args, &flags, commandLead, false)
	if err != nil {
		return err
	}
	flags.applyEnvDefaults()
	return dispatchCommand(base, flags, lead.agent, append(lead.rest, "/ping"))
}

// cmdPair dispatches the `foci pair`/`pair-key` shorthand: the host is
// its one argument — no free text — so the line keeps the historical
// whole-line disposition of the one scan, and the /pair word lands
// before the host.
func cmdPair(base string, args []string) error {
	if wantsHelp(args) {
		pairKeyUsage()
		return nil
	}
	var flags commandFlags
	lead, err := scanCommandLine(args, &flags, commandLead, false)
	if err != nil {
		return err
	}
	flags.applyEnvDefaults()
	return dispatchCommand(base, flags, lead.agent, append([]string{"/pair"}, lead.rest...))
}

func pairKeyUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci pair-key [-a agent] [host]

Mint a single-use, short-TTL Android pairing key (#862) and print it. The device
exchanges this key at POST /app/pair for its own revocable token — there is no
persisted master key. Pass an optional host to also print a foci://pair string.

Requires the app provider to be running (the key lives in the gateway's memory).
Use this for headless onboarding when there's no telegram/discord chat to run the
/android wizard in.
`)
}

func pingUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci ping [-a agent]

Liveness check (shorthand for 'foci command /ping').

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)
`)
}

func authUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci auth [--config <path>] [--addr <host:port>] [--provider <name>] [--api-key <key>]

Set an API key for an LLM provider. Saved to secrets.toml.

If a foci gateway is running, the new credentials are hot-reloaded
immediately (no restart needed).

Flags:
  --config <path>       Path to foci.toml (secrets.toml is written alongside it)
                        Default secrets path: ~/config/secrets.toml
  --addr <host:port>    Gateway address for credential hot-reload notification
                        Env: FOCI_ADDR / Default: %s
  --provider <name>     Provider: anthropic, gemini, openai, openrouter (default: anthropic)
  --api-key <key>       API key (prompted interactively if omitted)

The HTTP API key (http.api_key in secrets.toml) is read automatically
to authenticate the reload request to the gateway.
`, defaultAddr)
}

func cmdAuth(args []string) error {
	if wantsHelp(args) {
		authUsage()
		return nil
	}

	// Parse flags
	configPath := ""
	addr := ""
	providerKey := ""
	apiKey := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config" && i+1 < len(args):
			configPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			configPath = args[i][len("--config="):]
		case args[i] == "--addr" && i+1 < len(args):
			addr = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--addr="):
			addr = args[i][len("--addr="):]
		case args[i] == "--provider" && i+1 < len(args):
			providerKey = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--provider="):
			providerKey = args[i][len("--provider="):]
		case args[i] == "--api-key" && i+1 < len(args):
			apiKey = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--api-key="):
			apiKey = args[i][len("--api-key="):]
		}
	}
	addr = envDefault(addr, "FOCI_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	if providerKey == "" {
		providerKey = "anthropic"
	}

	prov := providerByKey(providerKey)
	if prov == nil {
		return fmt.Errorf("unknown provider %q; use: anthropic, gemini, openai, openrouter", providerKey)
	}

	secretsPath := resolveSecretsPath(configPath)

	// If the file doesn't exist, confirm path with the user before writing
	if _, err := os.Stat(secretsPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Secrets file will be created at: %s\nConfirm? [Y/n] ", secretsPath)
		var answer string
		_, _ = fmt.Scanln(&answer) // ignore error if user just presses enter
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "" && answer != "y" && answer != "yes" {
			return fmt.Errorf("aborted")
		}
		if err := os.MkdirAll(filepath.Dir(secretsPath), 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", filepath.Dir(secretsPath), err)
		}
	}

	store, err := secrets.Load(secretsPath)
	if err != nil {
		return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
	}

	// Prompt for API key if not provided
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "%s API key: ", prov.Name)
		_, _ = fmt.Scanln(&apiKey)
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			return fmt.Errorf("API key cannot be empty")
		}
	}

	if err := store.Set(prov.SecretKey, apiKey); err != nil {
		return fmt.Errorf("set secret: %w", err)
	}
	if err := store.Save(); err != nil {
		return fmt.Errorf("save secrets: %w", err)
	}
	fmt.Fprintf(os.Stderr, "API key saved to %s\n", secretsPath)

	// Notify running gateway to hot-reload credentials.
	// Prefer Unix socket (no API key needed), fall back to TCP with key.
	notifyGatewayReload(addr, store)
	return nil
}

// notifyGatewayReload sends a POST to the gateway's /-/reload-credentials endpoint.
// Prefers Unix socket (same-user auth, no key needed). Falls back to TCP with
// http.api_key from the secrets store. Best-effort: if the gateway isn't
// running, prints a note and continues.
func notifyGatewayReload(addr string, store *secrets.Store) {
	c := &http.Client{Timeout: 3 * time.Second}

	var u string
	if sock := resolveGWSocket(""); sock != "" {
		c.Transport = unixSocketTransport(sock)
		u = "http://foci-gw/-/reload-credentials"
	} else {
		u = fmt.Sprintf("http://%s/-/reload-credentials", addr)
		if apiKey, _ := store.Get("http.api_key"); apiKey != "" {
			c.Transport = &authTransport{key: apiKey}
		}
	}

	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Gateway not reachable — restart to use new credentials.\n")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Gateway not reachable — restart to use new credentials.\n")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		fmt.Fprintf(os.Stderr, "Gateway credentials hot-reloaded.\n")
	} else {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Gateway reload returned HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func secretsUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci secrets <subcommand> [args...]

Manage secrets in secrets.toml without a running gateway.

Subcommands:
  list                          List secret names (no values)
  get <section.key>             Print secret value to stdout
  set <section.key> <value>     Add or update a secret
  delete <section.key>          Remove a secret

Flags:
  --config <path>       Path to foci.toml (secrets.toml is resolved alongside it)
                        Default secrets path: ~/config/secrets.toml
`)
}

func cmdSecrets(args []string) error {
	if len(args) == 0 {
		secretsUsage()
		return nil
	}
	// Show top-level secrets help only when -h/--help is the first arg
	// (not a subcommand). Subcommands handle their own help.
	if isHelpWord(args[0]) {
		secretsUsage()
		return nil
	}

	// Parse --config flag
	configPath := ""
	var filtered []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config" && i+1 < len(args):
			configPath = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			configPath = args[i][len("--config="):]
		default:
			filtered = append(filtered, args[i])
		}
	}

	secretsPath := resolveSecretsPath(configPath)

	if len(filtered) == 0 {
		secretsUsage()
		return nil
	}

	sub := filtered[0]
	subArgs := filtered[1:]

	switch sub {
	case "list":
		if wantsHelp(subArgs) {
			fmt.Fprintf(os.Stderr, "Usage: foci secrets list\n\nList all secret names (values are not shown).\n")
			return nil
		}
		store, err := secrets.Load(secretsPath)
		if err != nil {
			return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
		}
		names := store.Names()
		if len(names) == 0 {
			fmt.Fprintf(os.Stderr, "no secrets in %s\n", secretsPath)
			return nil
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil

	case "get":
		if wantsHelp(subArgs) {
			fmt.Fprintf(os.Stderr, "Usage: foci secrets get <section.key>\n\nPrint the value of a secret to stdout.\n")
			return nil
		}
		if len(subArgs) != 1 {
			return fmt.Errorf("usage: foci secrets get <section.key>")
		}
		store, err := secrets.Load(secretsPath)
		if err != nil {
			return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
		}
		val, ok := store.Get(subArgs[0])
		if !ok {
			return fmt.Errorf("secret %q not found", subArgs[0])
		}
		fmt.Print(val)
		return nil

	case "set":
		if wantsHelp(subArgs) {
			fmt.Fprintf(os.Stderr, "Usage: foci secrets set <section.key> <value>\n\nAdd or update a secret. Key must be in section.key format (e.g. custom.github_token).\n")
			return nil
		}
		if len(subArgs) != 2 {
			return fmt.Errorf("usage: foci secrets set <section.key> <value>")
		}
		if !strings.Contains(subArgs[0], ".") {
			return fmt.Errorf("key must be in section.key format (e.g. custom.github_token)")
		}
		store, err := secrets.Load(secretsPath)
		if err != nil {
			return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
		}
		if err := store.Set(subArgs[0], subArgs[1]); err != nil {
			return fmt.Errorf("set secret: %w", err)
		}
		if err := store.Save(); err != nil {
			return fmt.Errorf("save secrets: %w", err)
		}
		fmt.Fprintf(os.Stderr, "set %s in %s\n", subArgs[0], secretsPath)
		return nil

	case "delete":
		if wantsHelp(subArgs) {
			fmt.Fprintf(os.Stderr, "Usage: foci secrets delete <section.key>\n\nRemove a secret by name.\n")
			return nil
		}
		if len(subArgs) != 1 {
			return fmt.Errorf("usage: foci secrets delete <section.key>")
		}
		store, err := secrets.Load(secretsPath)
		if err != nil {
			return fmt.Errorf("load secrets (%s): %w", secretsPath, err)
		}
		found, err := store.Remove(subArgs[0])
		if err != nil {
			return fmt.Errorf("remove secret: %w", err)
		}
		if !found {
			return fmt.Errorf("secret %q not found", subArgs[0])
		}
		if err := store.Save(); err != nil {
			return fmt.Errorf("save secrets: %w", err)
		}
		fmt.Fprintf(os.Stderr, "deleted %s from %s\n", subArgs[0], secretsPath)
		return nil

	default:
		return fmt.Errorf("unknown subcommand: %s\nRun 'foci secrets --help' for usage", sub)
	}
}

// resolveSecretsPath returns the secrets.toml path derived from configPath,
// or the default <install root>/config/secrets.toml if configPath is empty.
func resolveSecretsPath(configPath string) string {
	if configPath != "" {
		return filepath.Join(filepath.Dir(configPath), "secrets.toml")
	}
	home, err := fociroot.Dir()
	if err != nil {
		return "secrets.toml" // fallback
	}
	return filepath.Join(home, "config", "secrets.toml")
}

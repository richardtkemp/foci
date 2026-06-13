package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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

func evalUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci eval [-a agent] <shell command>

Ask the agent to run a shell command and show output.

Flags:
  -a, --agent <id>        Target agent (env: FOCI_AGENT)
`)
}

func cmdEval(base string, args []string) error {
	if wantsHelp(args) {
		evalUsage()
		return nil
	}
	agent, args := parseAgentFlag(args)
	if len(args) == 0 {
		return fmt.Errorf("usage: foci eval [-a agent] <shell command>")
	}
	cmd := strings.Join(args, " ")
	text := fmt.Sprintf("Run this command and show the output:\n```\n%s\n```", cmd)
	body := map[string]string{"text": text}
	if agent != "" {
		body["agent"] = agent
	}
	return postJSON(base+"/send", body)
}

func commandUsage() {
	fmt.Fprintf(os.Stderr, `Usage: foci command [-a agent] [gate flags] </cmd> [args]

Dispatch a slash command via the gateway (bypasses agent conversation).

Flags:
  -a, --agent <id>          Target agent (env: FOCI_AGENT)

Activity gates (evaluated server-side; skip the command unless/if the target
session has run a turn recently — a turn in flight always counts as active):
  --if-active <dur>         Skip unless this session ran a turn within duration (env: FOCI_IF_ACTIVE)
  --if-inactive <dur>       Skip if this session ran a turn within duration (env: FOCI_IF_INACTIVE)
  --if-user-active <dur>    Skip unless the user touched this agent within duration (env: FOCI_IF_USER_ACTIVE)
  --if-user-inactive <dur>  Skip if the user touched this agent within duration (env: FOCI_IF_USER_INACTIVE)

Example (overnight reset that won't interrupt active or in-flight work):
  foci command --if-inactive 55m -a helen /reset
`)
}

func cmdCommand(base string, args []string) error {
	if wantsHelp(args) {
		commandUsage()
		return nil
	}
	agent, args := parseAgentFlag(args)
	var gf gateFlags
	var rest []string
	for i := 0; i < len(args); i++ {
		if c, ni := gf.tryParseGateArg(args, i); c {
			i = ni
			continue
		}
		rest = append(rest, args[i])
	}
	gf.applyEnvDefaults()
	if len(rest) == 0 {
		return fmt.Errorf("usage: foci command [-a agent] [gate flags] </cmd> [args]")
	}
	cmd := strings.Join(rest, " ")
	if !strings.HasPrefix(cmd, "/") {
		cmd = "/" + cmd
	}
	body := map[string]interface{}{"command": cmd}
	if agent != "" {
		body["agent"] = agent
	}
	gf.addToBody(body)
	return postJSON(base+"/command", body)
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

	store.Set(prov.SecretKey, apiKey)
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
	if args[0] == "-h" || args[0] == "--help" {
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
		store.Set(subArgs[0], subArgs[1])
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
		if !store.Remove(subArgs[0]) {
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
// or the default ~/config/secrets.toml if configPath is empty.
func resolveSecretsPath(configPath string) string {
	if configPath != "" {
		return filepath.Join(filepath.Dir(configPath), "secrets.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "secrets.toml" // fallback
	}
	return filepath.Join(home, "config", "secrets.toml")
}

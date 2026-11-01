package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"foci/internal/config"
	"foci/internal/configapprove"
)

// cmd_config.go implements `foci config` (#479), the agent's config tool.
// foci.toml is read-only to agents (root:foci-secrets 0664), so a change is a
// REQUEST: the gateway shows the user an Approve/Deny prompt with the path,
// the old value and the new one, and the command waits for the answer.
//
//	foci config show                     the running config, secrets redacted (= /config toml)
//	foci config get <path>               the value at <path> in the config FILE, or that it is unset
//	foci config set <path> <value>       ask the user to approve setting <path>
//	foci config unset <path>             ask the user to approve removing <path>
//	foci config wait <id>                follow a request made earlier (e.g. by /config set over HTTP)
//
// set, unset and wait print exactly one outcome — applied, denied, timed out
// or failed (with a reason) — and exit 0 only for applied.

// Poll pacing, swappable in tests.
var (
	configPollSleep = time.Sleep
	configPollNow   = time.Now
)

// configRetryEvery is the pause between attempts while the gateway cannot
// be reached (it may be restarting).
const configRetryEvery = 2 * time.Second

// configWaitGrace is how much longer than the request's own timeout the CLI
// keeps polling before it gives up on a gateway that never answers.
const configWaitGrace = 30 * time.Second

// cmdConfig runs `foci config` and returns the exit code.
func cmdConfig(base string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpWord(args[0]) || wantsHelp(args[1:]) {
		configUsage(stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		if err := cmdCommand(base, []string{"/config", "toml"}); err != nil {
			_, _ = fmt.Fprintf(stderr, "config show: %v\n", err)
			return 1
		}
		return 0
	case "get":
		return configGetCmd(rest, stdout, stderr)
	case "set", "unset", "wait":
		return configRequestCmd(base, sub, rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "config: unknown subcommand %q\n", sub)
		configUsage(stderr)
		return 2
	}
}

// configGetCmd prints the value at a path in the config file.
func configGetCmd(args []string, stdout, stderr io.Writer) int {
	configPath, args := parseFlagValue(args, "config")
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: foci config get <path> [--config <file>]")
		return 2
	}
	p, err := config.ParseKeyPath(args[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "config get: %v\n", err)
		return 2
	}
	if configPath == "" {
		if configPath, err = config.DefaultConfigPath(); err != nil {
			_, _ = fmt.Fprintf(stderr, "config get: %v\n", err)
			return 1
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "config get: %v\n", err)
		return 1
	}
	v, ok, err := config.LookupPath(string(data), p)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "config get: %v\n", err)
		return 1
	}
	if !ok {
		_, _ = fmt.Fprintf(stdout, "%s is unset in %s (the default applies)\n", p, configPath)
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "%s = %s\n", p, config.RenderTOMLValue(v))
	return 0
}

// configRequestCmd submits a set/unset request, or follows one (wait), and
// prints its outcome.
func configRequestCmd(base, sub string, args []string, stdout, stderr io.Writer) int {
	waitFlag, args := parseFlagValue(args, "wait")
	session, args := parseFlagValue(args, "session")
	timeout := configapprove.DefaultTimeout
	if waitFlag != "" {
		d, err := time.ParseDuration(waitFlag)
		if err != nil || d <= 0 {
			_, _ = fmt.Fprintf(stderr, "config %s: --wait wants a duration such as 5m (got %q)\n", sub, waitFlag)
			return 2
		}
		timeout = configapprove.ClampTimeout(d)
	}

	var st configapprove.Status
	switch sub {
	case "wait":
		if len(args) != 1 {
			_, _ = fmt.Fprintln(stderr, "usage: foci config wait <request-id> [--wait <duration>]")
			return 2
		}
		st = configapprove.Status{ID: args[0], Outcome: configapprove.Pending}
	default:
		want := 2
		if sub == "unset" {
			want = 1
		}
		if len(args) != want {
			_, _ = fmt.Fprintf(stderr, "usage: foci config %s\n", map[string]string{"set": "set <path> <value> [--wait <duration>]", "unset": "unset <path> [--wait <duration>]"}[sub])
			return 2
		}
		if session == "" {
			session = os.Getenv("FOCI_SESSION_KEY")
		}
		if session == "" {
			_, _ = fmt.Fprintf(stderr, "config %s: no session: run it from an agent session (FOCI_SESSION_KEY) or pass --session <key>\n", sub)
			return 2
		}
		body := map[string]any{"session": session, "op": sub, "path": args[0], "timeout_secs": int(timeout / time.Second)}
		if sub == "set" {
			body["value"] = args[1]
		}
		var err error
		st, err = postConfigRequest(base, body)
		if err != nil {
			return printConfigOutcome(stdout, configapprove.Status{Outcome: configapprove.Failed, Reason: err.Error()})
		}
		if st.Outcome == configapprove.Pending {
			_, _ = fmt.Fprintf(stderr, "waiting for the user to approve %s (request %s, up to %s)...\n", st.Path, st.ID, timeout)
		}
	}
	if st.Outcome == configapprove.Pending {
		st = pollConfigRequest(base, st, timeout+configWaitGrace)
	}
	return printConfigOutcome(stdout, st)
}

// postConfigRequest submits a request. A gateway that cannot be reached is
// retried briefly (it may be restarting); a served error is final.
func postConfigRequest(base string, body map[string]any) (configapprove.Status, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return configapprove.Status{}, err
	}
	deadline := configPollNow().Add(postSendGiveUpAfter)
	for {
		resp, err := client.Post(base+"/config/request", "application/json", bytes.NewReader(data))
		if err != nil {
			if configPollNow().After(deadline) {
				return configapprove.Status{}, fmt.Errorf("the gateway is unreachable: %w", err)
			}
			configPollSleep(configRetryEvery)
			continue
		}
		return decodeConfigStatus(resp)
	}
}

// decodeConfigStatus reads a /config/request response.
func decodeConfigStatus(resp *http.Response) (configapprove.Status, error) {
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return configapprove.Status{}, fmt.Errorf("read response: %w", err)
	}
	var st configapprove.Status
	if jerr := json.Unmarshal(raw, &st); jerr != nil || st.Outcome == "" {
		return configapprove.Status{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return st, nil
}

// errUnknownRequest is a 404 from the poll: the gateway no longer knows the
// request, which means it restarted.
var errUnknownRequest = errors.New("unknown request")

// pollConfigRequest long-polls until the request is no longer pending, the
// gateway forgets it (restart), or giveUp passes.
func pollConfigRequest(base string, st configapprove.Status, giveUp time.Duration) configapprove.Status {
	deadline := configPollNow().Add(giveUp)
	for configPollNow().Before(deadline) {
		resp, err := client.Get(base + "/config/request?id=" + url.QueryEscape(st.ID) + "&wait=20")
		if err != nil {
			configPollSleep(configRetryEvery) // gateway restarting: the next answer is a 404
			continue
		}
		got, derr := decodeConfigStatus(resp)
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return configapprove.Status{ID: st.ID, Path: st.Path, Outcome: configapprove.Failed,
				Reason: "the gateway restarted and forgot the request (" + errUnknownRequest.Error() + "); nothing was changed — ask again"}
		case derr != nil:
			return configapprove.Status{ID: st.ID, Path: st.Path, Outcome: configapprove.Failed, Reason: derr.Error()}
		case got.Outcome != configapprove.Pending:
			return got
		}
		st = got
	}
	return configapprove.Status{ID: st.ID, Path: st.Path, Outcome: configapprove.Failed,
		Reason: "no outcome from the gateway in time; it may still be waiting for the user — follow it with: foci config wait " + st.ID}
}

// printConfigOutcome prints one outcome line and returns the exit code.
func printConfigOutcome(w io.Writer, st configapprove.Status) int {
	var line string
	switch st.Outcome {
	case configapprove.Applied:
		line = fmt.Sprintf("applied: %s = %s (was %s)", st.Path, st.New, st.Old)
	case configapprove.Denied:
		line = fmt.Sprintf("denied: the user did not approve %s; nothing was changed", st.Path)
	case configapprove.TimedOut:
		line = fmt.Sprintf("timed out: nobody answered the approval prompt for %s; nothing was changed", st.Path)
	default:
		line = "failed: " + st.Reason
	}
	_, _ = fmt.Fprintln(w, line)
	if st.Outcome == configapprove.Applied {
		return 0
	}
	return 1
}

func configUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `Usage: foci config <show|get|set|unset|wait> [args]

foci.toml is read-only to agents. A change is a request: the user sees the
setting, its current value and the new value in chat, and taps Approve or
Deny. Nothing changes unless they approve.

Subcommands:
  show                       The running config, secrets redacted (same as /config toml)
  get <path>                 The value at <path> in the config file, or that it is unset
  set <path> <value>         Ask the user to approve setting <path> to <value>
  unset <path>               Ask the user to approve removing <path> (its default applies)
  wait <request-id>          Follow a request made earlier (e.g. by /config set over HTTP)

Paths are TOML dotted keys; agents[<id>] selects one agent's block:
  agent_loop.max_tool_loops   agents[main].nudge.enable   models."claude.fast".id

Values are TOML values: "text", 42, 1.5, true, 2026-11-01T10:00:00Z,
["a", "b"], {key = "v"}, or [{path = "/x"}, {path = "/y"}] for a whole array
of tables (replaced whole). Quote the value for your shell, and quote a
path holding agents[...] (the brackets are a shell glob):
  foci config set 'agents[main].model' '"opus"'

set, unset and wait print one outcome: applied, denied, timed out, or failed
with a reason. Exit 0 only for applied.

Flags:
  --wait <duration>          How long the user has to answer (default %s, at most %s)
  --session <key>            The requesting session (default: $FOCI_SESSION_KEY)
  --config <path>            For get: the config file (default: $FOCI_CONFIG, else ~/config/foci.toml)
`, configapprove.DefaultTimeout, configapprove.MaxTimeout)
}

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"foci/internal/config"
	"foci/internal/configcheck"
	"foci/internal/delegator"
	"foci/internal/nudge"
	"foci/internal/workspace"
)

// cmdCheck implements `foci check config|nudge` (#2223): read-only validity
// checks an agent can run offline, before any gateway connection, like
// `foci pretool`. Neither subcommand writes a file or runs an extraction.
// It returns the process exit code.
func cmdCheck(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpWord(args[0]) {
		checkUsage(stderr)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	sub, rest := args[0], args[1:]
	if wantsHelp(rest) {
		checkUsage(stderr)
		return 0
	}
	switch sub {
	case "config":
		return checkConfig(rest, stdout, stderr)
	case "nudge":
		return checkNudge(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "check: unknown subcommand %q (want config or nudge)\n", sub)
		checkUsage(stderr)
		return 2
	}
}

// checkConfigMissingHint follows a "no config file" verdict: it names this
// command's own flag.
const checkConfigMissingHint = "Point at one explicitly with --config PATH, e.g. foci check config --config /home/foci/config/foci.toml"

// checkConfig runs the same check as `foci-gw -check-config`
// (configcheck.Run), with the same lines and exit codes.
func checkConfig(args []string, stdout, stderr io.Writer) int {
	configPath, args := parseFlagValue(args, "config")
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "check config: unexpected argument %q\n", args[0])
		return 2
	}
	if configPath == "" {
		p, err := config.DefaultConfigPath()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "check config: %v\n", err)
			return 2
		}
		configPath = p
	}
	return configcheck.Run(configPath, stdout, stderr, checkConfigMissingHint)
}

// checkNudge checks one agent's nudge-rules.json (or --file) with the same
// validator the gateway's scheduler warns with, against the agent's backend
// capabilities and character files.
func checkNudge(args []string, stdout, stderr io.Writer) int {
	configPath, args := parseFlagValue(args, "config")
	agentID, args := parseFlagValue(args, "agent")
	file, args := parseFlagValue(args, "file")
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "check nudge: unexpected argument %q\n", args[0])
		return 2
	}
	if agentID == "" {
		// Inside an agent session, default to that agent.
		agentID, _, _ = strings.Cut(os.Getenv("FOCI_SESSION_KEY"), "/")
	}
	if agentID == "" {
		_, _ = fmt.Fprintln(stderr, "check nudge: no agent given: name one with --agent <id> (there is no FOCI_SESSION_KEY to default from)")
		return 2
	}
	if configPath == "" {
		p, err := config.DefaultConfigPath()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "check nudge: %v\n", err)
			return 2
		}
		configPath = p
	}
	cfg, err := config.Load(configPath, delegator.RegisteredNames())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check nudge: load config: %v\n", err)
		return 2
	}
	var acfg *config.AgentConfig
	for i := range cfg.Agents {
		if cfg.Agents[i].ID == agentID {
			acfg = &cfg.Agents[i]
		}
	}
	if acfg == nil {
		_, _ = fmt.Fprintf(stderr, "check nudge: no agent %q in %s\n", agentID, configPath)
		return 2
	}

	if file == "" {
		file = nudge.RulesPath(acfg.Workspace)
	}
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintf(stdout, "agent %s has no nudge-rules.json at %s: nothing to check\n", agentID, file)
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check nudge: %v\n", err)
		return 2
	}

	canPostTool, canPreAnswer := nudge.CapabilitiesFor(*acfg)
	fileOrder := acfg.System.SystemFiles
	if len(fileOrder) == 0 {
		fileOrder = workspace.DefaultFileOrder
	}
	findings := nudge.ValidateFile(data, nudge.ValidateOpts{
		CanPostTool:  canPostTool,
		CanPreAnswer: canPreAnswer,
		WorkspaceDir: acfg.Workspace,
		FileOrder:    fileOrder,
	})

	var out strings.Builder
	errs, warns := 0, 0
	for _, f := range findings {
		out.WriteString(f.String() + "\n")
		if f.Severity == nudge.SeverityError {
			errs++
		} else {
			warns++
		}
	}
	verdict := "OK"
	if errs > 0 {
		verdict = "FAILED"
	}
	fmt.Fprintf(&out, "nudge check %s: %s: %d error(s), %d warning(s)\n", verdict, file, errs, warns)
	if _, err := io.WriteString(stdout, out.String()); err != nil {
		return 2
	}
	if errs > 0 {
		return 1
	}
	return 0
}

func checkUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: foci check <config|nudge> [flags]

Read-only validity checks, run offline (no gateway). Neither changes any file.

Subcommands:
  config               Check the foci config file: the same check, lines and
                       exit codes as foci-gw -check-config. Exit 0 when it
                       passes, non-zero with the failure lines when not.
  nudge                Check one agent's nudge-rules.json. Prints each error
                       (a rule that can never fire, an unknown key, bad JSON)
                       and warning (a trigger this agent's backend cannot run;
                       a content_hash that makes the gateway re-extract the
                       file), then a one-line verdict with the counts. Exit
                       non-zero when there is an error; a missing file is not
                       one.

Flags:
  --config <path>      Config file (default: $FOCI_CONFIG, else ~/config/foci.toml)

nudge flags:
  --agent <id>         Agent whose rules to check (default: from FOCI_SESSION_KEY)
  --file <path>        Check this file instead of the agent's nudge-rules.json,
                       with the same agent's backend capabilities
`)
}

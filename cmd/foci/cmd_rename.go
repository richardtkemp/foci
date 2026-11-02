package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"foci/internal/agentrename"
	"foci/internal/config"
	"foci/internal/delegator"
)

// cmdRenameAgent implements `foci rename-agent OLD NEW` (#1281): an offline
// rename of an agent id across every database, the sessions tree, the search
// index, optionally the workspace, and foci.toml, secrets.toml and the
// crontab: directly when run as root (the recommended way, #2786), else
// through sudo approval. `--restore DIR` undoes a rename from its
// backup. It never talks to the gateway: the gateway must be stopped.
func cmdRenameAgent(args []string, stdin io.Reader, stdout io.Writer) error {
	if wantsHelp(args) {
		renameAgentUsage(stdout)
		return nil
	}
	var (
		configPath, restore, asUser string
		move, planOnly, yes         bool
		pos                         []string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "--config", "--restore", "--user":
			if !hasVal {
				if i+1 >= len(args) {
					return fmt.Errorf("%s needs a value", name)
				}
				i++
				val = args[i]
			}
			switch name {
			case "--config":
				configPath = val
			case "--user":
				asUser = val
			default:
				restore = val
			}
		case "--move-workspace":
			move = true
		case "--plan", "--dry-run":
			planOnly = true
		case "--yes", "-y":
			yes = true
		default:
			if strings.HasPrefix(a, "-") {
				renameAgentUsage(os.Stderr)
				return fmt.Errorf("unknown flag %s", a)
			}
			pos = append(pos, a)
		}
	}
	// Run as root (#2786): resolve the install's user, its paths and config
	// as a run as that user would, and hand files back to it.
	var owner *agentrename.Owner
	if os.Geteuid() == 0 {
		o, p, err := agentrename.RootRun(asUser, configPath)
		if err != nil {
			return err
		}
		owner, configPath = o, p
	} else if asUser != "" {
		return errors.New("--user is for a run as root (sudo foci rename-agent --user NAME ...)")
	}
	if configPath == "" {
		p, err := config.DefaultConfigPath()
		if err != nil {
			return err
		}
		configPath = p
	}
	ctx := context.Background()
	interactive := isTerminal(os.Stdin)
	if restore != "" {
		if len(pos) > 0 || move || planOnly {
			return errors.New("--restore takes only the backup directory (and --config, --user, --yes)")
		}
		return agentrename.Restore(ctx, agentrename.RestoreOptions{
			ConfigPath: configPath, Backends: delegator.RegisteredNames(), BackupDir: restore,
			Yes: yes, In: stdin, Out: stdout, Interactive: interactive, Owner: owner,
		})
	}
	if len(pos) != 2 {
		renameAgentUsage(os.Stderr)
		return errors.New("rename-agent needs the old and the new id")
	}
	return agentrename.Run(ctx, agentrename.Options{
		ConfigPath: configPath, Backends: delegator.RegisteredNames(),
		Old: pos[0], New: pos[1], Move: move, PlanOnly: planOnly, Yes: yes,
		In: stdin, Out: stdout, Interactive: interactive, Owner: owner,
	})
}

// isTerminal reports whether f is a character device (a terminal).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func renameAgentUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage: sudo foci rename-agent [flags] --user USER OLD NEW
       sudo foci rename-agent --restore BACKUP_DIR --user USER [--config PATH] [--yes]

Rename agent OLD to NEW everywhere foci keeps the id. Stop the gateway first:
the command refuses while the gateway runs or anything has a database open.

Run it as root (sudo), with --user naming the user the gateway runs as (on a
Linux install: foci). As root every step writes directly, and every file it
writes keeps its owner, group and mode (new files go to USER), so the gateway
can use them after the restart. Run as USER instead, the [sudo] steps go
through sudo approval; on a host whose approvals travel through foci (aisudo)
none can arrive while the gateway is stopped, so they end PENDING.

It shows the plan (each step, how many rows or files it changes, which steps
need sudo), asks for confirmation, backs up every file it changes, then:
  1. moves data/sessions/OLD to data/sessions/NEW and fixes branch parent keys
  2. rewrites every database (state, api, app frames, deferred sends and the
     workspace's .data/*.db), each all-or-nothing
  3. moves the workspace search index aside (rebuilt at the next start)
  4. with --move-workspace: moves ~/OLD to ~/NEW and Claude Code's project folder
  5. [sudo] edits foci.toml (id, master_agent, [askgw] default_agent, agent lists,
     explicit bot = "OLD" so the agent keeps its bots; keeps the workspace
     path unless --move-workspace)
  6. [sudo] edits secrets.toml ([agents.OLD.*] tables, allowed/denied_agents)
  7. [sudo] edits USER's crontab (foci ... -a OLD lines)
A declined or failed sudo step is reported PENDING with the commands to run
by hand, and the command exits non-zero. A failed step stops the command:
re-run the same command to resume, or restore the backup.

Flags:
  --user USER         as root: the user the gateway runs as (default: $SUDO_USER);
                      its home is the install root unless FOCI_HOME is set
  --move-workspace    move ~/OLD to ~/NEW (default: keep the old directory)
  --plan, --dry-run   print the plan and exit; change nothing
  --yes, -y           do not ask for confirmation (non-interactive runs)
  --config PATH       foci.toml (default: $FOCI_CONFIG, else <install root>/config/foci.toml)
  --restore DIR       undo a rename from its backup directory

After a rename: start the gateway again. Turn ids change, so a Langfuse
re-export creates new traces for the renamed agent's old turns.
`)
}

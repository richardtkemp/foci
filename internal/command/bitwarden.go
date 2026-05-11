package command

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"foci/internal/procx"
)

// BitwardenStoreInfo provides read-only access to bitwarden store state
// for the /bitwarden status command.
type BitwardenStoreInfo interface {
	ItemCount() int
	RefreshedAt() time.Time
	CachedIDs() []string
}

// BitwardenCommand creates the /bitwarden slash command with setup and status subcommands.
func BitwardenCommand() *Command {
	cmd := &Command{
		Name:        "bitwarden",
		Description: "Bitwarden integration (setup/status)",
		Category:    "operations",
		Subcommands: []Subcommand{
			{
				Name:        "status",
				Description: "Show current bitwarden integration state",
				Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
					return Response{Text: bitwardenStatus(cc.BitwardenStore, cc.BitwardenEnabled)}, nil
				},
			},
			{
				Name:        "setup",
				Description: "Check prerequisites and create bitwarden system user",
				Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
					return Response{Text: bitwardenSetup()}, nil
				},
			},
		},
	}
	cmd.buildSubcommandDispatch()
	return cmd
}

func bitwardenSetup() string {
	var sb strings.Builder
	sb.WriteString("Bitwarden Setup\n")
	sb.WriteString(strings.Repeat("─", 40) + "\n\n")

	bwPath, err := exec.LookPath("bw")
	if err != nil {
		sb.WriteString("✗ bw CLI: NOT FOUND\n")
		sb.WriteString("  Install: https://bitwarden.com/help/cli/\n")
		sb.WriteString("\nSetup cannot continue without bw CLI.\n")
		return sb.String()
	}
	sb.WriteString(fmt.Sprintf("✓ bw CLI: %s\n", bwPath))

	if out, err := procx.Spawn(context.Background(), "bw", "--version").Output(); err == nil {
		sb.WriteString(fmt.Sprintf("  Version: %s\n", strings.TrimSpace(string(out))))
	}

	userExists := false
	if _, err := procx.Spawn(context.Background(), "id", "bitwarden").Output(); err == nil {
		userExists = true
		sb.WriteString("✓ bitwarden user: exists\n")
	} else {
		sb.WriteString("✗ bitwarden user: NOT FOUND\n")

		sb.WriteString("  Creating bitwarden system user via aisudo...\n")
		cmd := procx.Spawn(context.Background(), "sudo", "useradd", "--system", "--create-home", "--shell", "/usr/sbin/nologin", "bitwarden")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			sb.WriteString(fmt.Sprintf("  ✗ Failed: %s\n", strings.TrimSpace(stderr.String())))
			sb.WriteString("  Manual: sudo useradd --system --create-home --shell /usr/sbin/nologin bitwarden\n")
		} else {
			sb.WriteString("  ✓ Created bitwarden user\n")
			userExists = true
		}
	}

	if userExists {
		cmd := procx.Spawn(context.Background(), "sudo", "-u", "bitwarden", "bw", "status", "--nointeraction")
		out, err := cmd.Output()
		if err != nil {
			sb.WriteString("✗ bw status: cannot check (aisudo may need approval)\n")
		} else {
			status := strings.TrimSpace(string(out))
			if strings.Contains(status, `"authenticated"`) || strings.Contains(status, `"locked"`) {
				sb.WriteString("✓ bw login: authenticated\n")
				if strings.Contains(status, `"locked"`) {
					sb.WriteString("  Note: vault is locked — unlock and save session to file\n")
				}
			} else if strings.Contains(status, `"unauthenticated"`) {
				sb.WriteString("✗ bw login: NOT LOGGED IN\n")
				sb.WriteString("  Run as bitwarden user: sudo -u bitwarden bw login\n")
			} else {
				sb.WriteString(fmt.Sprintf("? bw status: %s\n", status))
			}
		}
	}

	sb.WriteString("\n")
	if userExists {
		sb.WriteString("Next steps:\n")
		sb.WriteString("  1. Ensure bitwarden user is logged in: sudo -u bitwarden bw login\n")
		sb.WriteString("  2. Unlock and save session key:\n")
		sb.WriteString("     sudo -u bitwarden bw unlock --raw | sudo -u bitwarden tee /home/bitwarden/.bw_session\n")
		sb.WriteString("     sudo -u bitwarden chmod 600 /home/bitwarden/.bw_session\n")
		sb.WriteString("  3. Set [bitwarden] enabled = true in foci.toml and restart\n")
	} else {
		sb.WriteString("Fix the issues above, then run /bitwarden setup again.\n")
	}

	return sb.String()
}

func bitwardenStatus(storeInfo BitwardenStoreInfo, enabled bool) string {
	var sb strings.Builder
	sb.WriteString("Bitwarden Status\n")
	sb.WriteString(strings.Repeat("─", 40) + "\n\n")

	if !enabled {
		sb.WriteString("State: DISABLED\n")
		sb.WriteString("\nEnable in foci.toml:\n")
		sb.WriteString("  [bitwarden]\n")
		sb.WriteString("  enabled = true\n")
		return sb.String()
	}

	sb.WriteString("State: ENABLED\n")

	if storeInfo == nil {
		sb.WriteString("Store: not initialized\n")
		return sb.String()
	}

	itemCount := storeInfo.ItemCount()
	refreshedAt := storeInfo.RefreshedAt()
	cachedIDs := storeInfo.CachedIDs()

	sb.WriteString(fmt.Sprintf("Cached items: %d\n", itemCount))

	if !refreshedAt.IsZero() {
		age := time.Since(refreshedAt).Round(time.Second)
		sb.WriteString(fmt.Sprintf("Last refresh: %s ago\n", age))
	} else {
		sb.WriteString("Last refresh: never\n")
	}

	sb.WriteString(fmt.Sprintf("Unlocked secrets: %d\n", len(cachedIDs)))
	if len(cachedIDs) > 0 {
		for _, id := range cachedIDs {
			sb.WriteString(fmt.Sprintf("  - %s\n", id))
		}
	}

	return sb.String()
}

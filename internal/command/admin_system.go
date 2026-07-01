package command

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"foci/internal/procx"
)

// BuildInfo holds version and build information.
type BuildInfo struct {
	Version   string
	GoVersion string
	GitCommit string
	BuildTime string
}

// VersionCommand returns a /version command.
func VersionCommand() *Command {
	return &Command{
		Name:        "version",
		Description: "Build version info",
		Category:    "diagnostics",
		Execute: func(_ context.Context, _ Request, cc CommandContext) (Response, error) {
			return Response{Text: fmt.Sprintf("version: %s\ngo: %s\ncommit: %s\nbuilt: %s",
				cc.BuildInfo.Version, cc.BuildInfo.GoVersion, cc.BuildInfo.GitCommit, cc.BuildInfo.BuildTime)}, nil
		},
	}
}

// restartFunc is the function used to trigger a restart. Overridable for testing.
var restartFunc = doRestart

// doRestart attempts to restart the service via systemctl, falling back to
// SIGTERM (relying on a process supervisor or Docker restart policy).
func doRestart() (string, error) {
	if _, err := exec.LookPath("systemctl"); err == nil {
		cmd := procx.Spawn(context.Background(), "systemctl", "restart", "foci")
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("systemctl restart failed: %w", err)
		}
		return "Restarting via systemctl...", nil
	}

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		return "", fmt.Errorf("restart failed: cannot find own process: %w", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return "", fmt.Errorf("restart failed: cannot send SIGTERM: %w", err)
	}
	return "Sent SIGTERM — waiting for process supervisor to restart...", nil
}

// RestartCommand creates a /restart command that restarts the foci service.
func RestartCommand() *Command {
	return &Command{
		Name:        "restart",
		Description: "Restart the foci service",
		Category:    "operations",
		Execute: func(_ context.Context, _ Request, _ CommandContext) (Response, error) {
			text, err := restartFunc()
			if err != nil {
				return Response{}, err
			}
			return Response{Text: text}, nil
		},
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"foci/internal/config"
)

// ccAllowedTools is the --allowedTools list a Claude Code agent launches with:
// the global [cc_backend] default_allowed_tools, the agent's own
// backend_config.allowed_tools, then foci's builtin rules
// (config.FociConfigAllowedTools, #3232). The launch (foldGlobalBackendConfig)
// and the environment block's "pre-approved by your backend" line both call
// it, so the prompt names exactly the rules the backend applies.
func ccAllowedTools(cc config.CCBackendConfig, perAgent any) string {
	return cc.MergedAllowedTools(perAgent, builtinCCAllowedTools()...)
}

// builtinCCAllowedTools is computed once: the CLI's path does not change while
// the gateway runs.
var builtinCCAllowedTools = sync.OnceValue(func() []string {
	return config.FociConfigAllowedTools(fociCLIPath(os.Executable))
})

// fociCLIPath returns the path of the foci CLI installed beside this gateway
// binary (/usr/local/bin/foci on the NUC, ~/foci/bin/foci on a Mac), or ""
// when there is none. An agent whose PATH lacks the CLI's directory types
// that path, and its rule must match it. A path with a comma is skipped: the
// rules travel comma-joined (config.BackendConfig.AllowedTools).
func fociCLIPath(executable func() (string, error)) string {
	exe, err := executable()
	if err != nil {
		return ""
	}
	cli := filepath.Join(filepath.Dir(exe), "foci")
	if strings.Contains(cli, ",") {
		return ""
	}
	if fi, err := os.Stat(cli); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return cli
}

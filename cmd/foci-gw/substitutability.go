package main

import (
	"os"

	"foci/internal/config"
	"foci/internal/execguard"
)

// applySubstitutabilityMode resolves [permissions] substitutability_check
// against this binary and installs the verdict, so every execguard.Live() view
// built afterwards — the startup drop, the built-in report, the match-time veto
// and -check-config's preview — sees the same answer. Call it before the first
// of those. When the check ends up off, logf gets one line saying why.
func applySubstitutabilityMode(cfg *config.Config, logf func(format string, args ...any)) {
	self, err := os.Executable()
	if err != nil {
		self = ""
	}
	on, why := execguard.Resolve(config.DerefStr(cfg.Permissions.SubstitutabilityCheck), self, execguard.Live())
	execguard.SetEnforced(on)
	if !on {
		logf("[permissions] executable substitutability check is OFF: %s", why)
	}
}

package main

import (
	"foci/internal/config"
	"foci/internal/configcheck"
	"foci/internal/execguard"
)

// warnSubstitutableBuiltins is the startup half of the built-in report: one
// WARN per built-in group command the match-time veto will refuse. The
// startup drop covers config entries only, while the veto also hits the
// built-in groups — without this WARN a vetoed built-in (mds, mdq on the
// live host) simply falls through to an approval prompt with no line
// anywhere explaining why (#1897). Called by reportOperatorEnv with the
// same environment the startup drop judged. The finder is shared with the
// config check's preview (configcheck.SubstitutableBuiltinsFor).
func warnSubstitutableBuiltins(cfg *config.Config, env execguard.Env) {
	for _, b := range configcheck.SubstitutableBuiltinsFor(cfg, env) {
		configLog.Warnf("[permissions] built-in %s group: command %q will be vetoed at match time and falls through to an approval prompt — %s (%s)",
			b.Group, b.Command, b.Reason, b.Path)
	}
}

package main

import (
	"io"

	"foci/internal/configcheck"
)

// checkConfigMissingHint is -check-config's line after a "no config file"
// verdict: it names this binary's own flag.
const checkConfigMissingHint = "Point at one explicitly with -config PATH, e.g. foci-gw -check-config -config /home/foci/config/foci.toml"

// runConfigCheck implements `foci-gw -check-config` (see package configcheck
// for what it checks and the exit codes). update.sh runs it, as root, before
// it stops the running daemon.
func runConfigCheck(path string, stdout, stderr io.Writer) int {
	return configcheck.Run(path, stdout, stderr, checkConfigMissingHint)
}

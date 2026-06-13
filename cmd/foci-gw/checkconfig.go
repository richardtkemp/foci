package main

// checkconfig.go implements `foci-gw -check-config`: a pre-flight validation
// of the config file that runs WITHOUT starting the server, opening databases,
// rotating the production log, or creating any directories.
//
// Purpose: update.sh builds a new foci-gw binary, then runs it with this flag
// (as root, against each service's -config path) BEFORE stopping the running
// daemon. If the new binary cannot load a config (a parse/validate error, or —
// under the strict policy — any unknown/deprecated key such as a renamed
// setting), the check exits non-zero and update.sh aborts, leaving the running
// foci untouched. Without this, config incompatibilities were only discovered
// after the old daemon had already been replaced, bricking the service.
//
// Exit codes:
//
//	0  config loads cleanly and has no unknown keys — the new binary will start
//	1  parse/validate error, OR one or more unknown/deprecated keys (strict)
//	2  usage error (e.g. config path unreadable for reasons other than load)
//
// Policy: STRICT. A silently-renamed key (old name still present in the file)
// is a real incompatibility — the old setting is lost on startup with only a
// warning — so unknown keys block the upgrade just like a hard load failure.
//
// Internal config.Load warnings spill to stderr via the default (pre-Init)
// logger; this is intentional and harmless — the production log is never opened.

import (
	"fmt"
	"os"

	"foci/internal/config"
)

// runConfigCheck loads the config at path and returns the process exit code.
// It performs no side effects beyond reading the file and printing a verdict.
func runConfigCheck(path string) int {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config check FAILED: %v\n", err)
		return 1
	}

	if len(cfg.UndefinedKeys) > 0 {
		fmt.Fprintf(os.Stderr, "config check FAILED: %d unknown/deprecated key(s) in %s:\n", len(cfg.UndefinedKeys), path)
		for _, k := range cfg.UndefinedKeys {
			fmt.Fprintf(os.Stderr, "  - %s\n", k)
		}
		fmt.Fprintf(os.Stderr, "These keys are silently ignored at startup (a rename loses the old setting). Fix or remove them before upgrading.\n")
		return 1
	}

	fmt.Fprintf(os.Stdout, "config check OK: %s loads cleanly with no unknown keys\n", path)
	return 0
}

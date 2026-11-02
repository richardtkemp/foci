package main

import (
	"fmt"
	"runtime"

	"foci/internal/config"
)

// config_protect.go is the gateway half of the config file protection (#479):
// the file is root:foci-secrets 0664 so no agent can write it, and every
// gateway load refuses a file in any other shape (fail closed) — at startup,
// at live apply after an edit, and at each reload of the delegated rules.
// `foci check config`, `foci pretool` and `foci debug` load the file
// without this check: they only read it.

// gwConfigProtect is the load-time protection check the gateway applies, set
// once in main from the startup config (nil: not enforced — the
// skip_security_checks escape hatch, or a platform without the
// root:foci-secrets model). Read by the rule reload sources.
var gwConfigProtect func(path string) error

// configProtection returns the protection check for cfg, logging once why it
// is off when it is.
func configProtection(cfg *config.Config) func(string) error {
	if !config.ConfigProtectionEnforced() {
		securityLog.Warnf("config file protection is not enforced on %s: agents run as the gateway's user there, so foci.toml cannot be kept from them", runtime.GOOS)
		return nil
	}
	if cfg.SkipSecurityChecks {
		securityLog.Warnf("skip_security_checks set — foci.toml ownership is NOT checked, so agents may be able to rewrite it")
		return nil
	}
	return config.CheckConfigProtected
}

// checkConfigLoad applies protect to path before a load; nil protect passes.
func checkConfigLoad(protect func(string) error, path string) error {
	if protect == nil {
		return nil
	}
	if err := protect(path); err != nil {
		return fmt.Errorf("refusing to load the config: %w", err)
	}
	return nil
}

package config

import (
	"fmt"
	"time"
)

// KeepaliveMaxUserIdleUnlimited reports whether a keepalive max_user_idle
// value means "no limit": empty, or a duration of zero. This is the reading
// the periodic runner applies (keepalive warms forever with no idle gate).
// An unparseable value counts as unlimited too, because the runner then
// applies no gate either.
func KeepaliveMaxUserIdleUnlimited(maxUserIdle string) bool {
	if maxUserIdle == "" {
		return true
	}
	d, err := time.ParseDuration(maxUserIdle)
	return err != nil || d <= 0
}

// PrecoldNeverFiresWarnings returns one warning per agent that enables
// compaction_precold (#1066) while keepalive is enabled with an unlimited
// max_user_idle. Keepalive then never stops warming the sessions it keeps
// alive, so their cache never goes cold and pre-cold compaction cannot fire
// for them. Load logs each returned line.
func PrecoldNeverFiresWarnings(cfg *Config) []string {
	var out []string
	for _, a := range cfg.Agents {
		rc := Resolve(cfg, a)
		if !rc.Compaction.Precold.Enabled || !rc.Keepalive.Enabled {
			continue
		}
		if !KeepaliveMaxUserIdleUnlimited(rc.Keepalive.MaxUserIdle) {
			continue
		}
		out = append(out, fmt.Sprintf(
			"agent %q: compaction_precold is enabled, but keepalive is enabled with max_user_idle = 0 (no limit) — keepalive never lets a session it warms go cold, so pre-cold compaction cannot fire for those sessions",
			a.ID))
	}
	return out
}

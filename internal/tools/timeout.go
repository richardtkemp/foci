package tools

import "time"

// TimeoutConfig defines the bounds for timeout validation.
type TimeoutConfig struct {
	DefaultSec int // default timeout in seconds (used when input ≤ 0)
	MaxSec     int // maximum timeout in seconds (0 = unlimited)
}

// ResolveTimeout validates and clamps a timeout value according to the config.
// Returns the resolved duration using DefaultSec when seconds ≤ 0, and
// clamping to MaxSec when set and exceeded.
func ResolveTimeout(seconds int, cfg TimeoutConfig) time.Duration {
	if seconds <= 0 {
		return time.Duration(cfg.DefaultSec) * time.Second
	}
	if cfg.MaxSec > 0 && seconds > cfg.MaxSec {
		return time.Duration(cfg.MaxSec) * time.Second
	}
	return time.Duration(seconds) * time.Second
}

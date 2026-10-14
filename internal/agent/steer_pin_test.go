package agent

import "foci/internal/platform"

// The steer-preference vocabulary lives in platform and nowhere else (#1318
// moved it there so a QueuedMessage can carry the sender's choice across the
// platform↔agent boundary before an Envelope exists). These aliases give
// agent tests the short names AND pin that home: re-declaring any of them in
// this package — a second copy of the type or its constants drifting back in
// — is a duplicate-declaration compile error, not a silent fork of the truth.
type SteerPreference = platform.SteerPreference

const (
	SteerDefault = platform.SteerDefault
	SteerAlways  = platform.SteerAlways
	SteerNever   = platform.SteerNever
)

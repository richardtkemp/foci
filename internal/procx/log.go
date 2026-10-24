package procx

import "foci/internal/log"

// execLog is the package logger. It lives in an untagged file because both
// the portable spawn code (procx.go) and the Linux capability code use it.
var execLog = log.NewComponentLogger("exec")

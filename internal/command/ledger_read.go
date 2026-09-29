package command

import (
	"time"

	"foci/internal/delegator/accounting"
)

// readCalls returns every call the cost views price, preferring the durable
// ledger in api.db over api.jsonl. The JSONL is archived to empty on every
// process start (initLogging's startup RotateOnce), so it under-reports
// everything before the last restart; it is read only when there is no
// ledger (unit tests, a gateway run without api_db) or the ledger holds
// nothing. Both carry every backend's calls: every booking goes through the
// ledger's Book, which writes the JSONL line too.
func readCalls(cc CommandContext) []accounting.CallRow {
	if l := accounting.Live(); l != nil {
		rows, err := l.Calls(time.Time{})
		if err != nil {
			commandLog.Warnf("read cost ledger: %v", err)
		}
		if len(rows) > 0 {
			return rows
		}
	}
	return accounting.ReadJSONL(cc.APILogPath)
}

// sessionStats reads one session's totals from the ledger, or nil when there
// is none.
func sessionStats(sessionKey string) *accounting.SessionStats {
	l := accounting.Live()
	if l == nil {
		return nil
	}
	st, err := l.SessionStats(sessionKey)
	if err != nil {
		commandLog.Warnf("session %s stats: %v", sessionKey, err)
		return nil
	}
	return st
}

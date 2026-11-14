package usagelog

import (
	"database/sql"
	"fmt"
	"time"

	"foci/internal/delegator"
)

// reports.go: the whole stored history of an account, as written. No
// production code reads it (/mana cached reads Current); the tests of this
// package and of its writers (command, the cmd/foci-gw wiring) do,
// to prove what was stored. The Makefile's deadcode check exempts this file.

// StoredReport is one stored report, as Reports returns it.
type StoredReport struct {
	delegator.UsageReport
	ID int64
}

// Reports lists the stored reports of account (every account when ""),
// oldest first, with their windows. For tests and inspection.
func (s *Store) Reports(account string) ([]StoredReport, error) {
	q := `SELECT id, received_at, backend, account, agent_id, source, payload FROM usage_reports`
	var args []any
	if account != "" {
		q += ` WHERE account = ?`
		args = append(args, account)
	}
	q += ` ORDER BY received_at, id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usagelog: reports: %w", err)
	}
	var out []StoredReport
	for rows.Next() {
		var (
			r        StoredReport
			received int64
			payload  string
		)
		if err := rows.Scan(&r.ID, &received, &r.Backend, &r.Account, &r.AgentID, &r.Source, &payload); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("usagelog: reports: %w", err)
		}
		r.ReceivedAt = time.Unix(0, received)
		if payload != "" {
			r.Payload = []byte(payload)
		}
		out = append(out, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usagelog: reports: %w", err)
	}
	for i := range out {
		ws, err := s.windows(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Windows = ws
	}
	return out, nil
}

func (s *Store) windows(reportID int64) ([]delegator.UsageReportWindow, error) {
	rows, err := s.db.Query(`SELECT window, length_sec, used, used_unit, resets_at, status, limit_reached FROM usage_windows WHERE report_id = ? ORDER BY id`, reportID)
	if err != nil {
		return nil, fmt.Errorf("usagelog: report windows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []delegator.UsageReportWindow
	for rows.Next() {
		var (
			w                 delegator.UsageReportWindow
			lengthSec, resets int64
			used              sql.NullFloat64
			reached           int
		)
		if err := rows.Scan(&w.Window, &lengthSec, &used, &w.UsedUnit, &resets, &w.Status, &reached); err != nil {
			return nil, fmt.Errorf("usagelog: report windows: %w", err)
		}
		w.Length = time.Duration(lengthSec) * time.Second
		if used.Valid {
			v := used.Float64
			w.Used = &v
		}
		if resets != 0 {
			w.ResetsAt = time.Unix(resets, 0)
		}
		w.LimitReached = reached != 0
		out = append(out, w)
	}
	return out, rows.Err()
}

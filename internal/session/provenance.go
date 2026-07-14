package session

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/timeutil"
)

// Provenance: point-in-time history lookup for stable session keys.
//
// A session key never changes, but its on-disk content rotates (compaction,
// /reset archive the live file in place) and its delegated CC session comes
// and goes (each respawn/resume observes a resume ID). To answer "where is
// the transcript covering moment T of session S — which JSONL file, and
// which CC session was live?", two append-only tables record the timeline:
//
//   - session_archives: one row per in-place archive rotation. The stamp
//     means "archived at": that file holds the session's history UP TO the
//     stamp. Written by the store event handler on compaction/reset events.
//   - cc_resume_history: one row per observed CC resume-ID change. The ID
//     live at time T is the newest observation at or before T. Written by
//     DelegatedManager.saveResumeID.
//
// Store.ArchiveFileAt provides a filesystem fallback that derives the same
// answer from archive filename stamps — covering archives that predate the
// tables (including files stamped by the legacy layout migration) and
// surviving a state.db loss.

// initProvenanceSchema creates the provenance tables. Called from
// NewSessionIndex alongside the main schema.
func initProvenanceSchema(db *sql.DB) {
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS session_archives (
			session_key TEXT NOT NULL,
			archived_at TEXT NOT NULL,
			file_path   TEXT NOT NULL,
			reason      TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (session_key, archived_at, file_path)
		)`,
		`CREATE TABLE IF NOT EXISTS cc_resume_history (
			session_key TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			resume_id   TEXT NOT NULL,
			PRIMARY KEY (session_key, observed_at, resume_id)
		)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			log.Errorf("session", "init provenance schema: %v", err)
		}
	}
}

// RecordArchive records that a session's live file was archived (compaction
// or reset) at this moment. filePath is the archive destination.
func (idx *SessionIndex) RecordArchive(sessionKey, filePath, reason string) {
	if idx == nil || filePath == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	_, err := idx.db.Exec(
		`INSERT OR IGNORE INTO session_archives (session_key, archived_at, file_path, reason) VALUES (?, ?, ?, ?)`,
		sessionKey, timeutil.Format(timeutil.Now()), filePath, reason,
	)
	if err != nil {
		sessionLog(sessionKey).Warnf("record archive for %s: %v", sessionKey, err)
	}
}

// timelineLookup runs a two-column (value, RFC3339 stamp) point-in-time
// query. Shared by ArchiveFileAt and CCResumeAt, whose only difference is
// the query direction.
func (idx *SessionIndex) timelineLookup(query, sessionKey string, at time.Time) (string, time.Time, bool) {
	if idx == nil {
		return "", time.Time{}, false
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var value, stamp string
	if err := idx.db.QueryRow(query, sessionKey, timeutil.Format(at)).Scan(&value, &stamp); err != nil {
		return "", time.Time{}, false
	}
	t, _ := time.Parse(time.RFC3339, stamp)
	return value, t, true
}

// ArchiveFileAt returns the archive file that covers moment `at` for a
// session: the earliest archive rotated at-or-after `at` (an archive holds
// history up to its stamp). ok=false means no recorded archive covers that
// moment — the live file does.
func (idx *SessionIndex) ArchiveFileAt(sessionKey string, at time.Time) (path string, archivedAt time.Time, ok bool) {
	return idx.timelineLookup(
		`SELECT file_path, archived_at FROM session_archives
		 WHERE session_key = ? AND unixepoch(archived_at) >= unixepoch(?)
		 ORDER BY unixepoch(archived_at) ASC LIMIT 1`,
		sessionKey, at,
	)
}

// RecordCCResume appends a CC resume-ID observation for a session.
// Consecutive observations of the same ID are collapsed (a respawn that
// resumes the same CC session is not a change).
func (idx *SessionIndex) RecordCCResume(sessionKey, resumeID string) {
	if idx == nil || sessionKey == "" || resumeID == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var latest string
	err := idx.db.QueryRow(
		`SELECT resume_id FROM cc_resume_history WHERE session_key = ?
		 ORDER BY unixepoch(observed_at) DESC LIMIT 1`,
		sessionKey,
	).Scan(&latest)
	if err == nil && latest == resumeID {
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		sessionLog(sessionKey).Warnf("cc resume history lookup for %s: %v", sessionKey, err)
		return
	}
	if _, err := idx.db.Exec(
		`INSERT OR IGNORE INTO cc_resume_history (session_key, observed_at, resume_id) VALUES (?, ?, ?)`,
		sessionKey, timeutil.Format(timeutil.Now()), resumeID,
	); err != nil {
		sessionLog(sessionKey).Warnf("record cc resume for %s: %v", sessionKey, err)
	}
}

// CCResumeAt returns the CC resume ID that was live for a session at moment
// `at`: the newest observation at or before it. ok=false means no CC session
// had been observed by then.
// AllCCResumes returns every CC resume ID ever recorded for sessionKey (newest
// first). Used by ephemeral-session cleanup to find every transcript file a
// session produced over its life (each post-compaction JSONL is a distinct ID).
func (idx *SessionIndex) AllCCResumes(sessionKey string) []string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	rows, err := idx.db.Query(
		`SELECT resume_id FROM cc_resume_history WHERE session_key = ?
		 ORDER BY unixepoch(observed_at) DESC`, sessionKey)
	if err != nil {
		return nil
	}
	defer rows.Close() // nolint:errcheck
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (idx *SessionIndex) CCResumeAt(sessionKey string, at time.Time) (resumeID string, observedAt time.Time, ok bool) {
	return idx.timelineLookup(
		`SELECT resume_id, observed_at FROM cc_resume_history
		 WHERE session_key = ? AND unixepoch(observed_at) <= unixepoch(?)
		 ORDER BY unixepoch(observed_at) DESC LIMIT 1`,
		sessionKey, at,
	)
}

// archiveStampLayouts are the filename-stamp formats produced by
// timeutil.FormatFilename over time (zone-offset current, Z-suffixed legacy).
var archiveStampLayouts = []string{"2006-01-02T15-04-05-0700", "2006-01-02T15-04-05Z"}

// parseArchiveStamp extracts the "archived at" time from an archive filename
// relative to its live stem, e.g. root.2026-03-04T02-30-00+0000.jsonl (or the
// counter variant root.<stamp>.2.jsonl, or .gz). Returns ok=false for
// non-archive names.
func parseArchiveStamp(name, stem string) (time.Time, bool) {
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), ".jsonl")
	if base == name || !strings.HasPrefix(base, stem+".") {
		return time.Time{}, false
	}
	rest := strings.TrimPrefix(base, stem+".")
	// Drop a trailing collision counter (".2") if present.
	if i := strings.LastIndex(rest, "."); i > 0 {
		if _, err := time.Parse(archiveStampLayouts[0], rest[:i]); err == nil {
			rest = rest[:i]
		} else if _, err := time.Parse(archiveStampLayouts[1], rest[:i]); err == nil {
			rest = rest[:i]
		}
	}
	for _, layout := range archiveStampLayouts {
		if t, err := time.Parse(layout, rest); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ArchiveFileAt answers the same question as SessionIndex.ArchiveFileAt from
// the filesystem alone: scan the session's directory for archive files and
// return the one with the earliest stamp at-or-after `at`. This covers
// archives that predate the provenance table — including files stamped by the
// legacy layout migration — and works without state.db. ok=false means the
// live file covers that moment.
func (s *Store) ArchiveFileAt(key string, at time.Time) (path string, archivedAt time.Time, ok bool) {
	livePath, err := s.SessionPath(key)
	if err != nil {
		return "", time.Time{}, false
	}
	dir := filepath.Dir(livePath)
	stem := strings.TrimSuffix(filepath.Base(livePath), ".jsonl")

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}
	type candidate struct {
		path string
		t    time.Time
	}
	var cands []candidate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if t, isArchive := parseArchiveStamp(e.Name(), stem); isArchive && !t.Before(at) {
			cands = append(cands, candidate{path: filepath.Join(dir, e.Name()), t: t})
		}
	}
	if len(cands) == 0 {
		return "", time.Time{}, false
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].t.Before(cands[j].t) })
	return cands[0].path, cands[0].t, true
}

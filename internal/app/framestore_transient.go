package app

import (
	"database/sql"
	"encoding/json"
	"sort"

	"foci/internal/fap"
)

// Frame-store half of transient frames (#1064): the per-conversation hello
// snapshot of meta and the prompt suggestion, and the one-off upgrade that
// drops the stored rows of the frame types that became transient.

// migrationTransient1064 names the upgrade in app_migrations.
const migrationTransient1064 = "transient-frames-1064"

// initTransientTables creates the tables this file uses. Called from
// newFrameStore after the main schema.
func initTransientTables(db *sql.DB) error {
	for _, stmt := range []string{
		// One row per conversation: the last meta it was sent (the payload JSON
		// without conversationId; "" = none) and its current prompt suggestion
		// ("" = none). Read when a binding is built, so a restart keeps both.
		`CREATE TABLE IF NOT EXISTS app_conv_snapshot (
			conv_id    TEXT NOT NULL PRIMARY KEY,
			meta       TEXT NOT NULL DEFAULT '',
			suggestion TEXT NOT NULL DEFAULT ''
		)`,
		// One-off data migrations that have run, by name.
		`CREATE TABLE IF NOT EXISTS app_migrations (
			name   TEXT    NOT NULL PRIMARY KEY,
			ran_ms INTEGER NOT NULL
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// PutConvSnapshot records a conversation's meta and prompt suggestion.
func (s *frameStore) PutConvSnapshot(convID string, meta *fap.ConversationMeta, suggestion string) {
	if s == nil {
		return
	}
	var metaJSON string
	if meta != nil {
		b, err := json.Marshal(meta)
		if err != nil {
			appLog.Errorf("frame store PutConvSnapshot (conv=%s): %v", convID, err)
			return
		}
		metaJSON = string(b)
	}
	if _, err := s.db.Exec(
		`INSERT INTO app_conv_snapshot (conv_id, meta, suggestion) VALUES (?, ?, ?)
		ON CONFLICT (conv_id) DO UPDATE SET meta = excluded.meta, suggestion = excluded.suggestion`,
		convID, metaJSON, suggestion,
	); err != nil {
		appLog.Errorf("frame store PutConvSnapshot (conv=%s): %v", convID, err)
	}
}

// ConvSnapshot returns a conversation's recorded meta (nil = none) and prompt
// suggestion ("" = none).
func (s *frameStore) ConvSnapshot(convID string) (*fap.ConversationMeta, string) {
	if s == nil {
		return nil, ""
	}
	var metaJSON, suggestion string
	err := s.db.QueryRow(`SELECT meta, suggestion FROM app_conv_snapshot WHERE conv_id = ?`, convID).Scan(&metaJSON, &suggestion)
	if err != nil {
		if err != sql.ErrNoRows {
			appLog.Errorf("frame store ConvSnapshot (conv=%s): %v", convID, err)
		}
		return nil, ""
	}
	if metaJSON == "" {
		return nil, suggestion
	}
	var m fap.ConversationMeta
	if err := json.Unmarshal([]byte(metaJSON), &m); err != nil {
		appLog.Errorf("frame store ConvSnapshot (conv=%s): bad meta: %v", convID, err)
		return nil, suggestion
	}
	return &m, suggestion
}

// transientTypesJSON is transientTypes as a JSON array, the argument of the
// queries' `IN (SELECT value FROM json_each(?))`.
func transientTypesJSON() string {
	list := make([]string, 0, len(transientTypes))
	for t := range transientTypes {
		list = append(list, t)
	}
	sort.Strings(list)
	b, _ := json.Marshal(list)
	return string(b)
}

// dropStoredTransientFrames is the one-off upgrade for #1064 (Dick,
// 2026-11-15: "old frames reclassified to ephemeral are dropped"). It deletes
// every stored row whose type is now transient, in one transaction, once:
//
//  1. It first seeds app_conv_snapshot from the rows it is about to delete:
//     each conversation's last meta, and its last prompt suggestion unless a
//     later stored frame made it stale. Those values used to reach a device by
//     replay; now only the hello snapshot carries them.
//  2. It records each conversation's seq high-water in app_conv_seq, as every
//     delete of app_frames rows must (#2197), so MaxSeq never goes back.
//  3. A conversation whose HIGHEST row is transient keeps that row, rewritten
//     in place into a conversation.ack with the same envelope id and seq: a
//     frame every app version accepts and ignores (the server already sends
//     the same placeholder for a tool.call, toolCallPlaceholder). Deleting it
//     would leave a hole at the top of the seq range that no later delivery
//     fills: the store's newest seq would be below the roster's lastSeq, so a
//     device behind it stays "behind" (re-requesting /app/replay on every
//     roster) until the conversation's next sequenced frame.
//  4. Every other transient row is deleted. A hole inside a conversation's
//     range is closed by the app's HTTP backfill, which covers each page
//     through its last seq (InboundTracker.coverThrough, foci-client #1996);
//     the hello's lastSeq ahead of the device's mark starts that backfill.
//
// It returns the number of rows deleted and rewritten.
func (s *frameStore) dropStoredTransientFrames() (deleted, rewritten int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var done int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM app_migrations WHERE name = ?`, migrationTransient1064).Scan(&done); err != nil {
		return 0, 0, err
	}
	if done > 0 {
		return 0, 0, nil
	}
	if err := seedConvSnapshots(tx); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(
		`INSERT INTO app_conv_seq (conv_id, max_seq)
			SELECT conv_id, MAX(seq) FROM app_frames GROUP BY conv_id
		ON CONFLICT (conv_id) DO UPDATE SET max_seq = MAX(max_seq, excluded.max_seq)`,
	); err != nil {
		return 0, 0, err
	}
	types := transientTypesJSON()
	rewritten, err = tombstoneTopTransientRows(tx, types)
	if err != nil {
		return 0, 0, err
	}
	res, err := tx.Exec(`DELETE FROM app_frames WHERE json_extract(wire, '$.t') IN (SELECT value FROM json_each(?))`, types)
	if err != nil {
		return 0, 0, err
	}
	deleted, _ = res.RowsAffected()
	if _, err := tx.Exec(`INSERT INTO app_migrations (name, ran_ms) VALUES (?, strftime('%s','now') * 1000)`, migrationTransient1064); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	if deleted > 0 {
		_, _ = s.db.Exec(`PRAGMA incremental_vacuum`)
	}
	return deleted, rewritten, nil
}

// suggestionStaleTypes are the stored frame types after which a stored
// prompt suggestion is stale: the app drops it on each (noteSnapshot).
var suggestionStaleTypes = map[string]bool{
	fap.TypeTurnStart: true, fap.TypeMessage: true, fap.TypeMedia: true, fap.TypeInteractive: true,
}

// seedConvSnapshots fills app_conv_snapshot (where a conversation has no row
// yet) from the stored meta and prompt.suggestion frames.
func seedConvSnapshots(tx *sql.Tx) error {
	rows, err := tx.Query(
		`SELECT conv_id, json_extract(wire, '$.t'), json_extract(wire, '$.d') FROM app_frames
		WHERE json_extract(wire, '$.t') IN ('meta','prompt.suggestion','turn.start','message','media','interactive')
		ORDER BY conv_id, seq`,
	)
	if err != nil {
		return err
	}
	type snap struct {
		meta, sugg string
	}
	snaps := map[string]*snap{}
	for rows.Next() {
		var convID, t string
		var d sql.NullString
		if err := rows.Scan(&convID, &t, &d); err != nil {
			_ = rows.Close()
			return err
		}
		sn := snaps[convID]
		if sn == nil {
			sn = &snap{}
			snaps[convID] = sn
		}
		switch {
		case t == fap.TypeMeta:
			var m fap.Meta
			if json.Unmarshal([]byte(d.String), &m) == nil {
				b, _ := json.Marshal(fap.ConversationMeta{Model: m.Model, PrevCostUsd: m.PrevCostUsd, Tokens: m.Tokens, Gap: m.Gap, CompactionLimitTokens: m.CompactionLimitTokens})
				sn.meta = string(b)
			}
		case t == fap.TypePromptSuggestion:
			var p fap.PromptSuggestion
			if json.Unmarshal([]byte(d.String), &p) == nil {
				sn.sugg = p.Text
			}
		case suggestionStaleTypes[t]:
			sn.sugg = ""
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for convID, sn := range snaps {
		if sn.meta == "" && sn.sugg == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO app_conv_snapshot (conv_id, meta, suggestion) VALUES (?, ?, ?) ON CONFLICT (conv_id) DO NOTHING`,
			convID, sn.meta, sn.sugg,
		); err != nil {
			return err
		}
	}
	return nil
}

// tombstoneTopTransientRows rewrites each conversation's highest-seq row, when
// it is of a transient type, into a same-id, same-seq conversation.ack.
func tombstoneTopTransientRows(tx *sql.Tx, types string) (int64, error) {
	rows, err := tx.Query(
		`SELECT f.conv_id, f.seq, f.wire FROM app_frames f
		JOIN (SELECT conv_id, MAX(seq) AS m FROM app_frames GROUP BY conv_id) top
			ON f.conv_id = top.conv_id AND f.seq = top.m
		WHERE json_extract(f.wire, '$.t') IN (SELECT value FROM json_each(?))`, types,
	)
	if err != nil {
		return 0, err
	}
	type top struct {
		convID string
		seq    int64
		wire   string
	}
	var tops []top
	for rows.Next() {
		var t top
		if err := rows.Scan(&t.convID, &t.seq, &t.wire); err != nil {
			_ = rows.Close()
			return 0, err
		}
		tops = append(tops, t)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()
	var n int64
	for _, t := range tops {
		var env fap.Envelope
		if err := json.Unmarshal([]byte(t.wire), &env); err != nil {
			return 0, err
		}
		wire, err := fap.Encode(fap.ConversationAck{ConversationID: t.convID}, t.seq, 0, env.ID, env.TS)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(
			`UPDATE app_frames SET wire = ?, visible = 0, preview = '' WHERE conv_id = ? AND seq = ?`,
			wire, t.convID, t.seq,
		); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

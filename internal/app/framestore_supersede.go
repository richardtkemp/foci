package app

import (
	"database/sql"
	"encoding/json"
	"strings"

	"foci/internal/fap"
)

// Latest-only stored frames (#1646).
//
// Some sequenced frames carry a WHOLE state and are re-sent in full on every
// change: a later copy makes every earlier one useless to replay. The frame
// store keeps only the newest stored copy of each (Dick 2026-11-15 16:26:
// "the frame store keeps only the newest stored copy per conversation of
// whole-snapshot frames, especially depth.feed"). The other whole-state
// frames he named (reminders, waitingOnOther) became transient in #1064 and
// are not stored at all.
//
//   - depth.feed: the whole depth tree of the root conversation; one per
//     conversation.
//   - interactive.progressEdit: a batched ask's whole answer list; one per
//     (conversation, promptId), since a conversation can hold several asks.
//
// Seq safety is the #1064 upgrade's: the copy kept is the newest, so a
// conversation's highest stored seq is never deleted, the seq high-water is
// recorded in app_conv_seq first (#2197), and a hole inside the range is
// closed by the app's HTTP backfill (foci-client #1996). The in-memory replay
// buffer is left alone: it is bounded, and a superseded copy in it is only
// redundant.

// migrationLatestOnly1646 names the one-off prune of existing copies.
const migrationLatestOnly1646 = "latest-only-frames-1646"

// supersedeKey reports whether a stored wire is a latest-only frame and, if
// so, the payload field that scopes "latest" beside the conversation ("" =
// per conversation). It reads the type off the front of the canonical wire
// fap.Encode writes (`{"t":"<type>",...`), so the common case costs no JSON
// parse.
func supersedeKey(wire string) (typ, field, value string, ok bool) {
	switch {
	case strings.HasPrefix(wire, `{"t":"`+fap.TypeDepthFeed+`"`):
		return fap.TypeDepthFeed, "", "", true
	case strings.HasPrefix(wire, `{"t":"`+fap.TypeInteractiveProgressEdit+`"`):
		var env struct {
			D struct {
				PromptID string `json:"promptId"`
			} `json:"d"`
		}
		if json.Unmarshal([]byte(wire), &env) != nil || env.D.PromptID == "" {
			return "", "", "", false
		}
		return fap.TypeInteractiveProgressEdit, "promptId", env.D.PromptID, true
	}
	return "", "", "", false
}

// supersede deletes the stored copies older than w of a latest-only frame.
// Called by the writer right after w is inserted.
func (s *frameStore) supersede(w frameWrite) {
	typ, field, value, ok := supersedeKey(w.wire)
	if !ok {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		appLog.Errorf("frame store supersede (conv=%s): %v", w.convID, err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	if err := recordSeqHighWater(tx, w.convID); err != nil {
		appLog.Errorf("frame store supersede (conv=%s): %v", w.convID, err)
		return
	}
	if field == "" {
		_, err = tx.Exec(`DELETE FROM app_frames WHERE conv_id = ? AND seq < ? AND json_extract(wire, '$.t') = ?`,
			w.convID, w.seq, typ)
	} else {
		_, err = tx.Exec(`DELETE FROM app_frames WHERE conv_id = ? AND seq < ? AND json_extract(wire, '$.t') = ?
			AND json_extract(wire, '$.d.promptId') = ?`,
			w.convID, w.seq, typ, value)
	}
	if err != nil {
		appLog.Errorf("frame store supersede (conv=%s seq=%d): %v", w.convID, w.seq, err)
		return
	}
	if err := tx.Commit(); err != nil {
		appLog.Errorf("frame store supersede (conv=%s): commit: %v", w.convID, err)
	}
}

// recordSeqHighWater records convID's current highest stored seq in
// app_conv_seq, never lowering it (#2197).
func recordSeqHighWater(tx *sql.Tx, convID string) error {
	_, err := tx.Exec(
		`INSERT INTO app_conv_seq (conv_id, max_seq)
			SELECT conv_id, MAX(seq) FROM app_frames WHERE conv_id = ? GROUP BY conv_id
		ON CONFLICT (conv_id) DO UPDATE SET max_seq = MAX(max_seq, excluded.max_seq)`, convID)
	return err
}

// pruneSupersededFrames is the one-off prune of the latest-only copies the
// store already holds: all but the newest of each key. Runs once (marker in
// app_migrations), before the writer starts. Returns the rows deleted.
func (s *frameStore) pruneSupersededFrames() (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var done int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM app_migrations WHERE name = ?`, migrationLatestOnly1646).Scan(&done); err != nil {
		return 0, err
	}
	if done > 0 {
		return 0, nil
	}
	if _, err := tx.Exec(
		`INSERT INTO app_conv_seq (conv_id, max_seq)
			SELECT conv_id, MAX(seq) FROM app_frames GROUP BY conv_id
		ON CONFLICT (conv_id) DO UPDATE SET max_seq = MAX(max_seq, excluded.max_seq)`,
	); err != nil {
		return 0, err
	}
	var deleted int64
	res, err := tx.Exec(
		`DELETE FROM app_frames WHERE json_extract(wire, '$.t') = ? AND seq < (
			SELECT MAX(g.seq) FROM app_frames g
			WHERE g.conv_id = app_frames.conv_id AND json_extract(g.wire, '$.t') = ?)`,
		fap.TypeDepthFeed, fap.TypeDepthFeed,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	deleted += n
	res, err = tx.Exec(
		`DELETE FROM app_frames WHERE json_extract(wire, '$.t') = ? AND seq < (
			SELECT MAX(g.seq) FROM app_frames g
			WHERE g.conv_id = app_frames.conv_id AND json_extract(g.wire, '$.t') = ?
			AND json_extract(g.wire, '$.d.promptId') = json_extract(app_frames.wire, '$.d.promptId'))`,
		fap.TypeInteractiveProgressEdit, fap.TypeInteractiveProgressEdit,
	)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	deleted += n
	if _, err := tx.Exec(`INSERT INTO app_migrations (name, ran_ms) VALUES (?, strftime('%s','now') * 1000)`, migrationLatestOnly1646); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if deleted > 0 {
		_, _ = s.db.Exec(`PRAGMA incremental_vacuum`)
	}
	return deleted, nil
}

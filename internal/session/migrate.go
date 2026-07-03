package session

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/timeutil"
)

// This file migrates installs from the pre-stable-identity era, when session
// keys carried a version segment ({agent}/{type}{id}/{versionTS}[/{child}])
// and session files lived in per-version directories. Both migrations are
// idempotent: they detect legacy artifacts and no-op on migrated installs.
//
//   - Store.MigrateLegacyLayout moves session files out of version
//     directories (newest version becomes the live file, older versions
//     become in-place archives) and rewrites branch_meta parent keys.
//   - migrateLegacyStateDB re-keys state.db rows (session_metadata,
//     chat_metadata, facet/tmux agent_metadata) and clears legacy
//     session_index rows so startup rebuilds the index from the migrated
//     files.

// LegacyKeyToStable converts a pre-stable-identity session key (with a
// version segment) to its stable form:
//
//	clutch/c123/1709590000        → clutch/c123
//	clutch/c123/1709590000/b1700  → clutch/c123/b1700
//	clutch/iwork/0                → clutch/iwork
//
// Returns ("", false) when the key is not a legacy key — already stable,
// or not a session key at all.
func LegacyKeyToStable(old string) (string, bool) {
	parts := strings.Split(old, "/")
	if len(parts) < 3 || len(parts) > 4 {
		return "", false
	}
	if parts[0] == "" {
		return "", false
	}
	typ, _, err := parseTypeID(parts[1])
	if err != nil || (typ != 'c' && typ != 'i') {
		return "", false
	}
	// The version segment is pure numeric — this is what distinguishes a
	// legacy 3-segment key from a stable branch key (whose third segment
	// starts with 'b' or 'i').
	if _, err := strconv.ParseInt(parts[2], 10, 64); err != nil {
		return "", false
	}
	stable := parts[0] + "/" + parts[1]
	if len(parts) == 4 {
		childType, _, err := parseTypeTS(parts[3])
		if err != nil || (childType != 'b' && childType != 'i') {
			return "", false
		}
		stable += "/" + parts[3]
	}
	return stable, true
}

// legacyVersionOf returns the version timestamp of a legacy key (used to
// order conflicting rows so the newest version wins). Callers must have
// validated the key with LegacyKeyToStable first.
func legacyVersionOf(old string) int64 {
	parts := strings.Split(old, "/")
	if len(parts) < 3 {
		return 0
	}
	v, _ := strconv.ParseInt(parts[2], 10, 64)
	return v
}

// MigrateLegacyLayout migrates the on-disk session tree from the per-version
// layout ({agent}/{typeid}/{versionTS}/root.jsonl) to the stable layout
// ({agent}/{typeid}/root.jsonl). For each session: the newest version's
// root.jsonl becomes the live file, older versions' roots become in-place
// archives (root.<stamp>.jsonl), archives and child/branch files move up
// beside them, and branch_meta parent keys inside child files are rewritten
// to the stable form. Empty version directories are removed.
//
// Idempotent: sessions without numeric version subdirectories are skipped.
// Returns the number of sessions migrated.
func (s *Store) MigrateLegacyLayout() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agents, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read sessions dir: %w", err)
	}

	migrated := 0
	for _, agent := range agents {
		if !agent.IsDir() {
			continue
		}
		agentDir := filepath.Join(s.dir, agent.Name())
		typeDirs, err := os.ReadDir(agentDir)
		if err != nil {
			continue
		}
		for _, td := range typeDirs {
			if !td.IsDir() {
				continue
			}
			sessionDir := filepath.Join(agentDir, td.Name())
			ok, err := migrateLegacySession(sessionDir)
			if err != nil {
				return migrated, fmt.Errorf("migrate %s/%s: %w", agent.Name(), td.Name(), err)
			}
			if ok {
				migrated++
				log.Infof("session", "migrated legacy session layout: %s/%s", agent.Name(), td.Name())
			}
		}
	}
	return migrated, nil
}

// migrateLegacySession flattens one session's version directories into
// sessionDir. Returns false when the session has no version directories
// (already migrated or newly created).
func migrateLegacySession(sessionDir string) (bool, error) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return false, err
	}

	var versions []int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if v, err := strconv.ParseInt(e.Name(), 10, 64); err == nil {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return false, nil
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	newest := versions[len(versions)-1]

	for i, v := range versions {
		versionDir := filepath.Join(sessionDir, strconv.FormatInt(v, 10))
		files, err := os.ReadDir(versionDir)
		if err != nil {
			return true, err
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			src := filepath.Join(versionDir, f.Name())
			name := f.Name()

			// An older version's live root becomes an in-place archive.
			// Archive stamps mean "archived at": a version's history ends
			// the moment the NEXT version began, so the stamp comes from
			// the superseding version's timestamp — matching what
			// nextArchivePath records for post-migration rotations.
			if v != newest && (name == "root.jsonl" || name == "root.jsonl.gz") {
				stamp := timeutil.FormatFilename(time.Unix(versions[i+1], 0))
				name = strings.Replace(name, "root", "root."+stamp, 1)
			}

			dst := clashFreePath(filepath.Join(sessionDir, name))
			if err := os.Rename(src, dst); err != nil {
				return true, fmt.Errorf("move %s: %w", src, err)
			}
			if isChildSessionFile(filepath.Base(dst)) {
				if err := rewriteBranchMetaParent(dst); err != nil {
					return true, fmt.Errorf("rewrite branch meta in %s: %w", dst, err)
				}
			}
		}
		// Version dir should be empty now; remove it (non-fatal if not).
		if err := os.Remove(versionDir); err != nil {
			log.Warnf("session", "legacy migration: remove %s: %v", versionDir, err)
		}
	}
	return true, nil
}

// isChildSessionFile reports whether a filename is a live child session file
// (b<ts>.jsonl or i<ts>.jsonl, no archive suffix) whose branch_meta line may
// need its parent key rewritten.
func isChildSessionFile(name string) bool {
	if !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	stem := strings.TrimSuffix(name, ".jsonl")
	if strings.Contains(stem, ".") || len(stem) < 2 {
		return false
	}
	if stem[0] != 'b' && stem[0] != 'i' {
		return false
	}
	_, err := strconv.ParseInt(stem[1:], 10, 64)
	return err == nil
}

// clashFreePath returns path, or path with a numeric suffix inserted before
// the extension if a file already exists there.
func clashFreePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := ".jsonl"
	if strings.HasSuffix(path, ".jsonl.gz") {
		ext = ".jsonl.gz"
	}
	stem := strings.TrimSuffix(path, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s.%d%s", stem, n, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// rewriteBranchMetaParent rewrites a child file's first-line branch_meta
// parent_key from the legacy to the stable form, atomically (temp + rename).
// No-op when the first line is not a branch_meta or the parent key is
// already stable.
func rewriteBranchMetaParent(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	if !scanner.Scan() {
		return scanner.Err()
	}
	first := scanner.Bytes()

	var meta BranchMeta
	if json.Unmarshal(first, &meta) != nil || meta.Type != "branch_meta" {
		return nil
	}
	stable, isLegacy := LegacyKeyToStable(meta.ParentKey)
	if !isLegacy {
		return nil
	}
	meta.ParentKey = stable
	newFirst, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	info, err := f.Stat()
	if err != nil {
		return err
	}
	tmp, err := os.OpenFile(path+".migrate-tmp", os.O_RDWR|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(newFirst, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	for scanner.Scan() {
		if _, err := tmp.Write(append(scanner.Bytes(), '\n')); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(path+".migrate-tmp", path)
}

// migrateLegacyStateDB re-keys state.db rows written under legacy session
// keys. Runs inside NewSessionIndex, after the schema is initialised.
//
//   - session_metadata: keys lose the version segment; when several versions
//     of the same session have rows for the same metadata key, the newest
//     version wins (this carries cc_resume_id forward so live chats resume
//     their CC conversation).
//   - chat_metadata: legacy 'session_key' rows become 'registered' ownership
//     rows (the key itself is now derived); an app-platform row whose value
//     is an independent-session adoption is rewritten instead of dropped.
//   - agent_metadata: facet:*/discord_facet:* values and tmux_owned/
//     tmux_watches JSON blobs are re-keyed.
//   - session_index: legacy rows are deleted and the last_clean_shutdown
//     marker cleared, so startup takes the rebuild path against the migrated
//     files.
func migrateLegacyStateDB(db *sql.DB) {
	// Old-schema DBs predate the structured columns; add them so the new
	// queries work. (Fresh DBs get them from CREATE TABLE.)
	for _, ddl := range []string{
		`ALTER TABLE session_index ADD COLUMN last_activity_at TEXT`,
		`ALTER TABLE session_index ADD COLUMN last_reflection TEXT`,
		`ALTER TABLE session_index ADD COLUMN agent_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE session_index ADD COLUMN chat_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE session_index ADD COLUMN is_root INTEGER NOT NULL DEFAULT 0`,
	} {
		_, _ = db.Exec(ddl) // "duplicate column" on migrated DBs — ignored
	}

	migrateLegacySessionMetadata(db)
	migrateLegacyChatMetadata(db)
	migrateLegacyAgentMetadata(db)
	migrateLegacySessionIndexRows(db)
}

// migrateLegacySessionMetadata re-keys session_metadata rows, newest version
// winning on conflict.
func migrateLegacySessionMetadata(db *sql.DB) {
	rows, err := db.Query(`SELECT DISTINCT session_key FROM session_metadata`)
	if err != nil {
		log.Errorf("session", "legacy migration: session_metadata scan: %v", err)
		return
	}
	type rekey struct {
		old     string
		stable  string
		version int64
	}
	var rekeys []rekey
	for rows.Next() {
		var sk string
		if rows.Scan(&sk) != nil {
			continue
		}
		if stable, ok := LegacyKeyToStable(sk); ok {
			rekeys = append(rekeys, rekey{old: sk, stable: stable, version: legacyVersionOf(sk)})
		}
	}
	_ = rows.Close()
	if len(rekeys) == 0 {
		return
	}

	// Ascending by version so the newest version's rows land last (REPLACE
	// semantics → newest wins).
	sort.Slice(rekeys, func(i, j int) bool { return rekeys[i].version < rekeys[j].version })
	for _, rk := range rekeys {
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO session_metadata (session_key, key, value)
			 SELECT ?, key, value FROM session_metadata WHERE session_key = ?`,
			rk.stable, rk.old,
		); err != nil {
			log.Errorf("session", "legacy migration: rekey %s: %v", rk.old, err)
			continue
		}
		_, _ = db.Exec(`DELETE FROM session_metadata WHERE session_key = ?`, rk.old)
	}
	log.Infof("session", "legacy migration: re-keyed session_metadata for %d session key(s)", len(rekeys))
}

// migrateLegacyChatMetadata converts legacy 'session_key' rows to
// 'registered' ownership rows, preserving app adoption overrides.
func migrateLegacyChatMetadata(db *sql.DB) {
	rows, err := db.Query(`SELECT agent_id, platform, chat_id, value FROM chat_metadata WHERE key = 'session_key'`)
	if err != nil {
		log.Errorf("session", "legacy migration: chat_metadata scan: %v", err)
		return
	}
	type chatRow struct {
		agent, platform, value string
		chatID                 int64
	}
	var legacy []chatRow
	for rows.Next() {
		var r chatRow
		if rows.Scan(&r.agent, &r.platform, &r.chatID, &r.value) != nil {
			continue
		}
		stable, isLegacy := LegacyKeyToStable(r.value)
		if !isLegacy {
			// Already-stable app adoption overrides stay as they are.
			continue
		}
		r.value = stable
		legacy = append(legacy, r)
	}
	_ = rows.Close()
	if len(legacy) == 0 {
		return
	}

	for _, r := range legacy {
		// Ownership registration replaces the stored key (keys derive now).
		if r.platform != "" {
			_, _ = db.Exec(
				`INSERT OR REPLACE INTO chat_metadata (agent_id, platform, chat_id, key, value) VALUES (?, ?, ?, 'registered', 'true')`,
				r.agent, r.platform, r.chatID,
			)
		}
		// An app conversation adopted onto an independent session keeps the
		// (re-keyed) override; chat-type values are fully derivable — drop.
		if r.platform == "app" && strings.HasPrefix(strings.SplitN(r.value, "/", 2)[1], "i") {
			_, _ = db.Exec(
				`UPDATE chat_metadata SET value = ? WHERE agent_id = ? AND platform = ? AND chat_id = ? AND key = 'session_key'`,
				r.value, r.agent, r.platform, r.chatID,
			)
		} else {
			_, _ = db.Exec(
				`DELETE FROM chat_metadata WHERE agent_id = ? AND platform = ? AND chat_id = ? AND key = 'session_key'`,
				r.agent, r.platform, r.chatID,
			)
		}
	}
	log.Infof("session", "legacy migration: converted %d chat_metadata session_key row(s) to registrations", len(legacy))
}

// migrateLegacyAgentMetadata re-keys facet bindings and tmux ownership maps.
func migrateLegacyAgentMetadata(db *sql.DB) {
	rows, err := db.Query(
		`SELECT agent_id, key, value FROM agent_metadata
		 WHERE key LIKE 'facet:%' OR key LIKE 'discord_facet:%' OR key IN ('tmux_owned', 'tmux_watches')`,
	)
	if err != nil {
		log.Errorf("session", "legacy migration: agent_metadata scan: %v", err)
		return
	}
	type update struct{ agent, key, value string }
	var updates []update
	for rows.Next() {
		var agent, key, value string
		if rows.Scan(&agent, &key, &value) != nil {
			continue
		}
		switch {
		case strings.HasPrefix(key, "facet:") || strings.HasPrefix(key, "discord_facet:"):
			if stable, ok := LegacyKeyToStable(value); ok {
				updates = append(updates, update{agent, key, stable})
			}
		case key == "tmux_owned":
			var owned map[string]string
			if json.Unmarshal([]byte(value), &owned) != nil {
				continue
			}
			changed := false
			for name, sk := range owned {
				if stable, ok := LegacyKeyToStable(sk); ok {
					owned[name] = stable
					changed = true
				}
			}
			if changed {
				if data, err := json.Marshal(owned); err == nil {
					updates = append(updates, update{agent, key, string(data)})
				}
			}
		case key == "tmux_watches":
			var watches []map[string]any
			if json.Unmarshal([]byte(value), &watches) != nil {
				continue
			}
			changed := false
			for _, w := range watches {
				if sk, ok := w["agent_session_key"].(string); ok {
					if stable, isLegacy := LegacyKeyToStable(sk); isLegacy {
						w["agent_session_key"] = stable
						changed = true
					}
				}
			}
			if changed {
				if data, err := json.Marshal(watches); err == nil {
					updates = append(updates, update{agent, key, string(data)})
				}
			}
		}
	}
	_ = rows.Close()

	for _, u := range updates {
		_, _ = db.Exec(`UPDATE agent_metadata SET value = ? WHERE agent_id = ? AND key = ?`, u.value, u.agent, u.key)
	}
	if len(updates) > 0 {
		log.Infof("session", "legacy migration: re-keyed %d agent_metadata row(s)", len(updates))
	}
}

// migrateLegacySessionIndexRows re-keys legacy session_index rows to their
// stable form — PRESERVING last_activity_at and last_reflection, which the
// default-chat routing tiebreak and the reflection guard order by (wiping
// them made the post-migration default pick arbitrary: the discord-misroute
// bug) — and clears the clean-shutdown marker so startup still rebuilds
// file-backed rows from the migrated files. Backend-session rows (empty
// file_path) survive the rebuild with their re-keyed activity intact.
// Where several versions of one session have rows, the newest version wins.
func migrateLegacySessionIndexRows(db *sql.DB) {
	rows, err := db.Query(
		`SELECT session_key, file_path, created_at, COALESCE(last_activity_at, ''), COALESCE(last_reflection, ''),
		        COALESCE(parent_session_key, ''), session_type, status
		 FROM session_index`)
	if err != nil {
		log.Errorf("session", "legacy migration: session_index scan: %v", err)
		return
	}
	type row struct {
		key, filePath, created, activity, reflection, parent, sessType, status string
		version                                                                int64
		stable                                                                 string
	}
	var legacy []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.key, &r.filePath, &r.created, &r.activity, &r.reflection, &r.parent, &r.sessType, &r.status) != nil {
			continue
		}
		if stable, ok := LegacyKeyToStable(r.key); ok {
			r.stable = stable
			r.version = legacyVersionOf(r.key)
			legacy = append(legacy, r)
		}
	}
	_ = rows.Close()
	if len(legacy) == 0 {
		return
	}

	// Ascending by version so the newest version's row lands last (REPLACE →
	// newest wins).
	sort.Slice(legacy, func(i, j int) bool { return legacy[i].version < legacy[j].version })
	for _, r := range legacy {
		parent := r.parent
		if stable, ok := LegacyKeyToStable(parent); ok {
			parent = stable
		}
		agentID, chatID, isRoot := keyColumns(r.stable)
		// Unknown activity degrades to creation time — never NULL, so the
		// most-recently-active ordering stays total.
		if r.activity == "" {
			r.activity = r.created
		}
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO session_index
			   (session_key, file_path, created_at, last_activity_at, last_reflection, parent_session_key, session_type, status, agent_id, chat_id, is_root)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.stable, r.filePath, r.created, r.activity, nullableString(r.reflection),
			nullableString(parent), r.sessType, r.status, agentID, chatID, isRoot,
		); err != nil {
			log.Errorf("session", "legacy migration: re-key index row %s: %v", r.key, err)
			continue
		}
		_, _ = db.Exec(`DELETE FROM session_index WHERE session_key = ?`, r.key)
	}
	// The re-keyed file_path values point at the OLD layout; force the
	// rebuild path so file-backed rows reconcile against the migrated files
	// (RebuildIndex preserves backend rows and their activity).
	_, _ = db.Exec(`DELETE FROM system_state WHERE key = 'last_clean_shutdown'`)
	log.Infof("session", "legacy migration: re-keyed %d legacy session_index row(s); file-backed rows will reconcile from disk", len(legacy))
}

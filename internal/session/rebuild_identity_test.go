package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/provider"
	"foci/internal/timeutil"
)

// wireIndexLikeGateway mirrors the gateway's session-event handler
// (cmd/foci-gw/sessions_init.go): a created session is upserted with the
// event's identity, and a compaction archive gets its own row keyed and
// timed by ArchiveIndexIdentity.
func wireIndexLikeGateway(t *testing.T, store *Store, idx *SessionIndex) {
	t.Helper()
	store.OnSessionEvent(func(e SessionEvent) {
		switch e.Status {
		case SessionStatusActive:
			idx.Upsert(SessionIndexEntry{
				SessionKey:       e.Key,
				FilePath:         e.FilePath,
				CreatedAt:        e.CreatedAt,
				ParentSessionKey: e.ParentKey,
				SessionType:      e.Type,
				Status:           SessionStatusActive,
			})
		case SessionStatusCompacted:
			if e.ArchivePath == "" {
				return
			}
			key, createdAt, err := ArchiveIndexIdentity(store.dir, e.ArchivePath)
			if err != nil {
				t.Errorf("ArchiveIndexIdentity(%s): %v", e.ArchivePath, err)
				return
			}
			idx.Upsert(SessionIndexEntry{
				SessionKey:       key,
				FilePath:         e.ArchivePath,
				CreatedAt:        createdAt,
				ParentSessionKey: e.Key,
				SessionType:      e.Type,
				Status:           SessionStatusCompacted,
			})
		}
	})
}

type identityRow struct {
	createdAt, activityAt time.Time
	parent                string
	typ                   SessionType
}

func readIdentity(t *testing.T, idx *SessionIndex, key string) identityRow {
	t.Helper()
	e, err := idx.Get(key)
	if err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	return identityRow{createdAt: e.CreatedAt, activityAt: e.LastActivityAt, parent: e.ParentSessionKey, typ: e.SessionType}
}

// #3184: a rebuild after an unclean start must give a gzipped session's row
// the created_at, session_type and parent_session_key foci wrote when it
// first created the row — not the gzip time, "unknown" and NULL. It must
// also keep the row's last_activity_at, which the gz file's mtime cannot
// reproduce.
func TestRebuild_GzippedSessionKeepsIdentity(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)
	wireIndexLikeGateway(t, store, idx)

	root := "bot/c100"
	if err := store.TestAppend(root, msg("user", "hello")); err != nil {
		t.Fatal(err)
	}
	// A compaction leaves an archive file root.<ts>.jsonl with its own row.
	if err := store.TestReplace(root, []provider.Message{msg("user", "summary")}); err != nil {
		t.Fatal(err)
	}
	archiveKey := ""
	entries, _ := os.ReadDir(filepath.Join(dir, "bot", "c100"))
	for _, de := range entries {
		if isArchiveFile(de.Name()) {
			archiveKey = root + "/" + strings.TrimSuffix(de.Name(), ".jsonl")
		}
	}
	if archiveKey == "" {
		t.Fatal("no compaction archive file")
	}
	branch, err := store.CreateBranchWithOptions(root, BranchOptions{BranchType: "reflection"})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{root, archiveKey, branch}

	want := map[string]identityRow{}
	for _, k := range keys {
		want[k] = readIdentity(t, idx, k)
	}
	if want[branch].typ != SessionTypeReflection || want[branch].parent != root {
		t.Fatalf("setup: branch row = %+v", want[branch])
	}
	if want[archiveKey].parent != root || want[archiveKey].typ != SessionTypeChat {
		t.Fatalf("setup: archive row = %+v", want[archiveKey])
	}

	// Idle both sessions and let the sweep gzip them (the root only once
	// its branch is archived, so sweep twice).
	past := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	backdateSession(t, store, idx, branch, past)
	backdateSession(t, store, idx, root, past)
	for i := 0; i < 2; i++ {
		if _, err := ArchiveSweep(store, idx, 24*time.Hour, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range keys {
		want[k] = readIdentity(t, idx, k) // activity now backdated
	}
	// The sweep runs days after creation in production: give every gz
	// file a gzip time far from its creation.
	gzTime := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	var gzFiles int
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".jsonl.gz") {
			gzFiles++
			if err := os.Chtimes(p, gzTime, gzTime); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if gzFiles != 3 {
		t.Fatalf("setup: %d gz files, want 3 (root, archive, branch)", gzFiles)
	}

	if _, err := idx.Rebuild(store); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// A fresh index (every row lost) must derive the same identity from
	// disk alone.
	fresh := tempIndex(t)
	if _, err := fresh.Rebuild(store); err != nil {
		t.Fatalf("fresh Rebuild: %v", err)
	}

	for _, k := range keys {
		w := want[k]
		for name, got := range map[string]identityRow{"rebuilt": readIdentity(t, idx, k), "fresh": readIdentity(t, fresh, k)} {
			// A branch key holds whole seconds of the clock read just
			// before the creation event's: allow that one tick.
			tol := time.Duration(0)
			if k == branch {
				tol = time.Second
			}
			if d := got.createdAt.Sub(w.createdAt); d < -tol || d > tol {
				t.Errorf("%s %s created_at = %s, want %s", name, k, timeutil.Format(got.createdAt), timeutil.Format(w.createdAt))
			}
			if got.typ != w.typ {
				t.Errorf("%s %s session_type = %q, want %q", name, k, got.typ, w.typ)
			}
			if got.parent != w.parent {
				t.Errorf("%s %s parent = %q, want %q", name, k, got.parent, w.parent)
			}
		}
		// Requirement 2: the rebuild keeps the row's last_activity_at; the
		// gz mtime (gzip time) must not replace it.
		if got := readIdentity(t, idx, k); !got.activityAt.Equal(w.activityAt) {
			t.Errorf("rebuilt %s last_activity_at = %s, want kept %s", k, timeutil.Format(got.activityAt), timeutil.Format(w.activityAt))
		}
	}
}

// A rebuild keeps a newer last_activity_at already in the row for a live
// (.jsonl) session, and takes the file's newer mtime over an older row.
func TestRebuild_LastActivityKeepsNewer(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	idx := tempIndex(t)
	wireIndexLikeGateway(t, store, idx)
	for _, k := range []string{"bot/c1", "bot/c2"} {
		if err := store.TestAppend(k, msg("user", "hi")); err != nil {
			t.Fatal(err)
		}
	}
	mtime := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, k := range []string{"bot/c1", "bot/c2"} {
		if err := os.Chtimes(mustSessionPath(t, store, k), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	newer := mtime.Add(30 * time.Minute)
	older := mtime.Add(-30 * time.Minute)
	idx.UpdateActivity("bot/c1", newer)
	idx.UpdateActivity("bot/c2", older)

	if _, err := idx.Rebuild(store); err != nil {
		t.Fatal(err)
	}
	if got := readIdentity(t, idx, "bot/c1").activityAt; !got.Equal(newer) {
		t.Errorf("c1 last_activity_at = %s, want the row's newer %s", timeutil.Format(got), timeutil.Format(newer))
	}
	if got := readIdentity(t, idx, "bot/c2").activityAt; !got.Equal(mtime) {
		t.Errorf("c2 last_activity_at = %s, want the file's newer mtime %s", timeutil.Format(got), timeutil.Format(mtime))
	}
}

func TestKeyTimestamp(t *testing.T) {
	cases := []struct {
		key  string
		want int64 // unix seconds; 0 = none
	}{
		{"clutch/c5970082313/b1784266842", 1784266842},
		{"clutch/c5970082313/i1784266842", 1784266842},
		{"clutch/c5970082313/b1784266842123456789", 1784266842},
		{"clutch/i1784266842", 1784266842},
		{"clutch/iresearch", 0},
		{"clutch/c5970082313", 0},          // a chat id is not a time
		{"clutch/c3050041760056152149", 0}, // nor a 19-digit one
		{"agent/clutch/cron/wake-1772095417", 1772095417},
		{"clutch/cron/memory-formation-1772095417", 1772095417},
		{"agent/clutch/spawn/spawn-1772095417560799267", 1772095417},
		{"agent/fotini/chat/5970082313", 0},
		{"agent/clutch/chat", 0},
		{"clutch/c1/b99", 0}, // implausible
	}
	for _, c := range cases {
		got, ok := keyTimestamp(c.key)
		if c.want == 0 {
			if ok {
				t.Errorf("keyTimestamp(%q) = %s, want none", c.key, got)
			}
			continue
		}
		if !ok || got.Unix() != c.want {
			t.Errorf("keyTimestamp(%q) = %v,%v, want %d", c.key, got, ok, c.want)
		}
	}
}

func TestArchiveSuffixTime(t *testing.T) {
	cases := []struct {
		key  string
		want string // RFC3339; "" = none
	}{
		{"bot/c1/root.2026-07-24T09-12-00+0100", "2026-07-24T09:12:00+01:00"},
		{"bot/c1/root.2026-03-04T02-30-00Z", "2026-03-04T02:30:00Z"},
		{"bot/c1/root.2026-03-04T02-30-00Z.2", "2026-03-04T02:30:00Z"},
		{"bot/c1/b1784266842.2026-07-17T19-55-57+0100", "2026-07-17T19:55:57+01:00"},
		{"agent/clutch/chat/5970082313.7", ""},
		{"bot/c1/b1784266842.2", ""},
	}
	for _, c := range cases {
		got, ok := archiveSuffixTime(c.key)
		if c.want == "" {
			if ok {
				t.Errorf("archiveSuffixTime(%q) = %s, want none", c.key, got)
			}
			continue
		}
		w, _ := time.Parse(time.RFC3339, c.want)
		if !ok || !got.Equal(w) {
			t.Errorf("archiveSuffixTime(%q) = %v,%v, want %s", c.key, got, ok, c.want)
		}
	}
}

// The sweep's gz file keeps the source's mtime (its last append), so a
// rebuild of a row it has never seen reads the last activity, not the gzip
// time (#3184).
func TestArchiveSweep_GzKeepsSourceMtime(t *testing.T) {
	store := NewStore(t.TempDir())
	idx := tempIndex(t)
	if err := store.TestAppend("bot/c100", msg("user", "hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Rebuild(store); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	backdateSession(t, store, idx, "bot/c100", past)
	if n, err := ArchiveSweep(store, idx, 24*time.Hour, nil); err != nil || n != 1 {
		t.Fatalf("ArchiveSweep = %d, %v; want 1 archived", n, err)
	}
	fi, err := os.Stat(mustSessionPath(t, store, "bot/c100") + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(past) {
		t.Errorf("gz mtime = %s, want the source's %s", fi.ModTime(), past)
	}
}

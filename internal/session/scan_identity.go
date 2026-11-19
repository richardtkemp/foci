package session

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"foci/internal/timeutil"
)

// The identity a rebuild derives for a file must be the identity foci wrote
// into the row when it first created it (#3184: Dick, 2026-11-19, Q121/Q122).
// Every value below comes from the file's name or its first line, never from
// its mtime alone: the archive sweep gzips files in batches days after their
// creation, so a gz file's mtime is the gzip time.

// legacyKeyTS matches the last segment of a pre-SessionKey legacy key that
// carries its creation time: wake-<s>, background-<s>, memory-formation-<s>,
// keepalive-<s>, consolidation-<s>, mb-<s>, spawn-<ns>.
var legacyKeyTS = regexp.MustCompile(`^[a-z][a-z-]*-(\d+)$`)

// earliestKeyTS bounds what keyTimestamp accepts: foci did not exist before
// it, so a smaller number is not a creation time.
var earliestKeyTS = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// keyTimestamp returns the creation time a session key encodes: a child's
// b<ts>/i<ts> (unix seconds, or nanoseconds for a batch key), an anonymous
// independent root's i<ts>, or a legacy key's trailing -<ts>. A chat id
// (c<id>) is never read as a time. ok is false when the key holds none.
func keyTimestamp(key string) (time.Time, bool) {
	var digits string
	if k, err := ParseSessionKey(key); err == nil {
		switch {
		case k.ChildType != 0:
			digits = strconv.FormatInt(k.ChildTS, 10)
		case k.Type == 'i':
			digits = k.ID
		}
	} else {
		segs := strings.Split(key, "/")
		if m := legacyKeyTS.FindStringSubmatch(segs[len(segs)-1]); m != nil {
			digits = m[1]
		}
	}
	return unixFromDigits(digits)
}

// unixFromDigits reads an all-digit unix time by its length: 10 digits are
// seconds, 13 milliseconds, 16 microseconds, 19 nanoseconds. Anything else,
// or a time before 2020 or more than a day ahead, is not a timestamp.
func unixFromDigits(s string) (time.Time, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return time.Time{}, false
	}
	var t time.Time
	switch len(s) {
	case 10:
		t = time.Unix(v, 0)
	case 13:
		t = time.UnixMilli(v)
	case 16:
		t = time.UnixMicro(v)
	case 19:
		t = time.Unix(0, v)
	default:
		return time.Time{}, false
	}
	if t.Before(earliestKeyTS) || t.After(time.Now().Add(24*time.Hour)) {
		return time.Time{}, false
	}
	return t.Truncate(time.Second).In(timeutil.Location()), true
}

// archiveSuffixLayout parses nextArchivePath's suffix: FormatFilename
// ("+0100") and the older UTC form ("Z").
const archiveSuffixLayout = "2006-01-02T15-04-05Z0700"

// archiveSuffixTime returns the time in a compaction/reset archive key's
// suffix (root.<ts>, root.<ts>.<n>, b<ts>.<ts>). nextArchivePath takes it
// from the clock at the archive's creation, the moment the gateway writes
// the archive row. ok is false for the legacy counter-only suffix (.<n>).
func archiveSuffixTime(archiveKey string) (time.Time, bool) {
	segs := strings.Split(archiveKey, "/")
	parts := strings.Split(segs[len(segs)-1], ".")
	for i := len(parts) - 1; i >= 1; i-- {
		if t, err := time.Parse(archiveSuffixLayout, parts[i]); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ArchiveIndexIdentity returns the index key and created_at of a
// compaction/reset archive file under storeDir. The gateway writes the
// archive's row with these when it creates the archive, and a rebuild
// derives the same values from disk, so the two agree (#3184).
func ArchiveIndexIdentity(storeDir, archivePath string) (string, time.Time, error) {
	rel, err := filepath.Rel(storeDir, archivePath)
	if err != nil {
		return "", time.Time{}, err
	}
	if strings.HasPrefix(rel, "..") {
		return "", time.Time{}, fmt.Errorf("archive %s is outside the session store %s", archivePath, storeDir)
	}
	key := pathToKey(filepath.ToSlash(strings.TrimSuffix(strings.TrimSuffix(rel, ".gz"), ".jsonl")))
	if t, ok := archiveSuffixTime(key); ok {
		return key, t, nil
	}
	return key, timeutil.Now(), nil
}

// fileIdentity is what a session file's first line says about its row.
type fileIdentity struct {
	createdAt time.Time // zero when the line holds no creation time
	parent    string
	branch    bool
	typ       string // branch_meta.branch_type
}

// readFileIdentity reads a session file's first line (through gzip for a .gz
// file): a session_meta line gives the creation time foci wrote with the
// row; a branch_meta line gives the parent and branch type. An unreadable or
// unrecognised line gives a zero identity.
func readFileIdentity(path string) fileIdentity {
	line, _, err := readFirstLine(path, false)
	if err != nil || len(line) == 0 {
		return fileIdentity{}
	}
	var head struct {
		Type       string `json:"type"`
		CreatedAt  string `json:"created_at"`
		ParentKey  string `json:"parent_key"`
		BranchType string `json:"branch_type"`
	}
	if json.Unmarshal(line, &head) != nil {
		return fileIdentity{}
	}
	switch head.Type {
	case "session_meta":
		if t, err := time.Parse(time.RFC3339, head.CreatedAt); err == nil {
			return fileIdentity{createdAt: t}
		}
	case "branch_meta":
		return fileIdentity{parent: head.ParentKey, branch: true, typ: head.BranchType}
	}
	return fileIdentity{}
}

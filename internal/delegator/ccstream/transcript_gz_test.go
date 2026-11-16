package ccstream

import (
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"foci/internal/delegator"
	"foci/internal/log"
)

// gzipLikeArchiveSweep compresses path to path.gz and removes path, as
// session.ArchiveSweep does to a delegated session's transcript (#3134).
func gzipLikeArchiveSweep(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path+gzSuffix, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// TestStart_ResumeRestoresGzippedTranscript is the #3134 regression: a
// session whose transcript the archive sweep gzipped resumes with its
// history. Start puts <id>.jsonl back before CC is launched (CC 2.1.325 says
// "No conversation found" for a .jsonl.gz alone) and before the cost
// baseline is read, so the baseline is the restored history's.
func TestStart_ResumeRestoresGzippedTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workDir := filepath.Join(home, "work")
	path, err := ccTranscriptPath(nil, workDir, probeSID)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, path, costStateT1)
	want, _ := os.ReadFile(path)
	gzipLikeArchiveSweep(t, path)

	b := startWithFakeClaude(t, workDir, probeSID, nil)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("transcript not restored for --resume: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("restored transcript = %q, want %q", got, want)
	}
	if _, err := os.Stat(path + gzSuffix); !os.IsNotExist(err) {
		t.Errorf("gzipped copy left behind (stat err %v)", err)
	}
	if lg := b.ledger.Load(); lg == nil || lg.book.baseline["claude-haiku-4-5"].OutputTokens != 195 {
		t.Errorf("cost baseline not read from the restored transcript")
	}
}

// TestForkSession_FromGzippedParent: a parent whose transcript was gzipped
// can still be forked (a nested depth fan-out from a leaf idle past the
// sweep, #3134).
func TestForkSession_FromGzippedParent(t *testing.T) {
	home, parentPath := writeParentTranscript(t)
	t.Setenv("HOME", home)
	gzipLikeArchiveSweep(t, parentPath)

	b := &Backend{}
	res, err := b.ForkSession(context.Background(), delegator.ForkRequest{
		ParentSessionID: testParentUUID, WorkDir: testWorkDir,
	})
	if err != nil {
		t.Fatalf("ForkSession from a gzipped parent: %v", err)
	}
	if _, err := os.Stat(parentPath); err != nil {
		t.Errorf("parent transcript not restored: %v", err)
	}
	newPath := filepath.Join(filepath.Dir(parentPath), res.SessionID+".jsonl")
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("fork not written: %v", err)
	}
}

// TestCleanupSession_RemovesGzippedTranscript is the #3134 leak: an
// ephemeral transcript the archive sweep gzipped before the daily GC ran must
// be deleted too, or the GC stamps it swept and it stays on disk forever.
func TestCleanupSession_RemovesGzippedTranscript(t *testing.T) {
	home, parentPath := writeParentTranscript(t)
	t.Setenv("HOME", home)
	gzipLikeArchiveSweep(t, parentPath)

	b := &Backend{}
	if err := b.CleanupSession(context.Background(), delegator.CleanupRequest{
		SessionID: testParentUUID, WorkDir: testWorkDir,
	}); err != nil {
		t.Fatalf("CleanupSession: %v", err)
	}
	if _, err := os.Stat(parentPath + gzSuffix); !os.IsNotExist(err) {
		t.Errorf("gzipped transcript not deleted (stat err %v)", err)
	}
}

// TestRestoreGzippedTranscript_LiveFileWins: a plain transcript is never
// replaced by an older gzipped copy, and nothing at all is a no-op.
func TestRestoreGzippedTranscript_LiveFileWins(t *testing.T) {
	lg := log.NewComponentLogger("test")
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")

	if err := restoreGzippedTranscript(path, lg); err != nil {
		t.Fatalf("no file at all: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("restore created a transcript from nothing")
	}

	writeTranscript(t, path, `{"type":"user","n":1}`)
	gzipLikeArchiveSweep(t, path)
	writeTranscript(t, path, `{"type":"user","n":2}`)
	if err := restoreGzippedTranscript(path, lg); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != `{"type":"user","n":2}`+"\n" {
		t.Errorf("live transcript replaced: %q", got)
	}
}

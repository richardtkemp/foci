package ccstream

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"foci/internal/log"
)

// gzSuffix is what foci's archive sweep (session.ArchiveSweep) appends to a
// transcript it compresses: <id>.jsonl becomes <id>.jsonl.gz.
const gzSuffix = ".gz"

// restoreGzippedTranscript puts a CC transcript the archive sweep gzipped
// back where Claude Code reads it (#3134). The sweep compresses the file a
// delegated session's index row names, which for a CC session is its
// transcript, once it has been idle for sessions.archive_after (24h by
// default). CC reads only <id>.jsonl: `claude --resume <id>` with only
// <id>.jsonl.gz on disk exits with "No conversation found with session ID"
// (verified on CC 2.1.325), and a fork would find no parent.
//
// It does nothing when path exists or no <path>.gz does. The copy is
// decompressed under a temporary name and published by hard link, which
// fails if path appeared meanwhile (a concurrent restore, or CC itself), so
// a live transcript is never overwritten. Only then is the .gz removed.
func restoreGzippedTranscript(path string, lg *log.ComponentLogger) error {
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil // present, or not ours to judge: CC reports what it finds
	}
	gzPath := path + gzSuffix
	gf, err := os.Open(gzPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open gzipped transcript: %w", err)
	}
	defer func() { _ = gf.Close() }()
	info, err := gf.Stat()
	if err != nil {
		return fmt.Errorf("stat gzipped transcript: %w", err)
	}
	gr, err := gzip.NewReader(gf)
	if err != nil {
		return fmt.Errorf("read gzipped transcript %s: %w", filepath.Base(gzPath), err)
	}
	defer func() { _ = gr.Close() }()

	// Beside the transcript (the hard link needs one filesystem), under a
	// name that does not end in .jsonl, so no transcript reader or sweep
	// picks it up, and unique to this call (O_EXCL), so two restores never
	// share it.
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("restore transcript: %w", err)
	}
	tmpPath := path + ".restore-" + hex.EncodeToString(rnd[:])
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("restore transcript: %w", err)
	}
	defer func() { _ = os.Remove(tmpPath) }()
	// #nosec G110 -- foci's own archived transcript, not untrusted input
	if _, err := io.Copy(tmp, gr); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("decompress %s: %w", filepath.Base(gzPath), err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync restored transcript: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close restored transcript: %w", err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil // someone else put it back first; theirs is live
		}
		return fmt.Errorf("publish restored transcript: %w", err)
	}
	if err := os.Remove(gzPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		lg.Warnf("restored transcript %s; remove %s: %v", filepath.Base(path), filepath.Base(gzPath), err)
	}
	lg.Infof("restored gzipped transcript %s for resume", filepath.Base(path))
	return nil
}

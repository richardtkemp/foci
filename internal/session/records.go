package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"foci/internal/timeutil"
)

// Session files are JSONL: every record is written as one complete line
// (marshalled JSON + '\n') in a single write. Two consequences shape the
// readers below:
//
//   - A record has no size limit on the write side, so the readers impose
//     none either (#3065 F18): whatever Append accepted, Load must read back.
//     A capped bufio.Scanner made one valid >10 MiB message turn the whole
//     session into "token too long".
//   - A write cut off by a crash or power loss leaves the file without its
//     final '\n'. That missing newline is the one demonstrable sign of a
//     torn tail (#3065 F13); repairTornTailUnlocked acts on it and on
//     nothing else, so interior corruption still fails loudly.

// forEachLine calls fn with every line of r (without its '\n'), with no
// length limit. A final line with no trailing newline is still delivered.
func forEachLine(r io.Reader, fn func(line []byte) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if fnErr := fn(bytes.TrimSuffix(line, []byte{'\n'})); fnErr != nil {
				return fnErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// repairTornTailUnlocked makes a live session file end on a record boundary
// before it is read or appended to. Caller must hold s.mu.
//
// It acts only when the file's last byte is not '\n' — the sign of a write
// cut off part way (every record is written with its newline in one write):
//
//   - the unterminated last line is a complete JSON object: only the '\n' was
//     lost. The newline is restored, so the next append starts a new line
//     instead of gluing two records into one corrupt line.
//   - otherwise the line is a torn record. Its bytes are kept in a sidecar
//     file (<path>.torn-<ts>, which no session walker reads) and the live
//     file is truncated back to the end of the last complete record, so the
//     intact history before it loads again.
//
// A malformed line that IS newline-terminated is interior corruption: it is
// left alone, and Load reports it as an error.
func (s *Store) repairTornTailUnlocked(key, path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open session %s for tail check: %w", key, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat session %s: %w", key, err)
	}
	size := info.Size()
	if size == 0 || !info.Mode().IsRegular() {
		return nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return fmt.Errorf("read tail of session %s: %w", key, err)
	}
	if last[0] == '\n' {
		return nil
	}

	start, err := lastLineStart(f, size)
	if err != nil {
		return fmt.Errorf("find last record of session %s: %w", key, err)
	}
	tail := make([]byte, size-start)
	if _, err := f.ReadAt(tail, start); err != nil {
		return fmt.Errorf("read last record of session %s: %w", key, err)
	}

	if len(tail) > 0 && tail[0] == '{' && json.Valid(tail) {
		if _, err := f.WriteAt([]byte{'\n'}, size); err != nil {
			return fmt.Errorf("restore final newline of session %s: %w", key, err)
		}
		if err := f.Sync(); err != nil {
			return fmt.Errorf("sync session %s: %w", key, err)
		}
		sessionLog(key).Warnf("session %s: last record had lost its newline (interrupted write); newline restored", key)
		return nil
	}

	quarantine, err := s.writeQuarantine(path, tail)
	if err != nil {
		return fmt.Errorf("quarantine torn tail of session %s: %w", key, err)
	}
	if err := f.Truncate(start); err != nil {
		return fmt.Errorf("truncate torn tail of session %s: %w", key, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync session %s: %w", key, err)
	}
	sessionLog(key).Warnf("session %s: torn final record (%d bytes, interrupted write) moved to %s; %d bytes of intact history kept",
		key, len(tail), quarantine, start)
	return nil
}

// lastLineStart returns the offset just after the last '\n' in the first
// size bytes of f, or 0 if there is none.
func lastLineStart(f *os.File, size int64) (int64, error) {
	const chunk = 64 * 1024
	buf := make([]byte, chunk)
	end := size
	for end > 0 {
		n := int64(chunk)
		if end < n {
			n = end
		}
		off := end - n
		if _, err := f.ReadAt(buf[:n], off); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return off + int64(i) + 1, nil
		}
		end = off
	}
	return 0, nil
}

// writeQuarantine saves torn bytes next to the session file and returns the
// sidecar's path. The name does not end in .jsonl or .jsonl.gz, so session
// scans, the index rebuild, archive lookup and the sweep all ignore it.
func (s *Store) writeQuarantine(path string, data []byte) (string, error) {
	base := path + ".torn-" + timeutil.FormatFilename(timeutil.Now())
	candidate := base
	for n := 2; ; n++ {
		f, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, s.fileMode)
		if os.IsExist(err) {
			candidate = fmt.Sprintf("%s.%d", base, n)
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return "", err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return "", err
		}
		return candidate, f.Close()
	}
}

package session

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// ReadBranchParent returns the parent_key of a session file's first-line
// branch_meta. ok is false when the file has no branch_meta first line (a
// root file, an empty file). A .gz file is read through gzip.
func ReadBranchParent(path string) (parent string, ok bool, err error) {
	first, _, err := readFirstLine(path, false)
	if err != nil {
		return "", false, err
	}
	var meta BranchMeta
	if json.Unmarshal(first, &meta) != nil || meta.Type != "branch_meta" {
		return "", false, nil
	}
	return meta.ParentKey, true, nil
}

// RewriteBranchParent rewrites the parent_key of a session file's first-line
// branch_meta through rewrite, which returns the new key and whether to
// change it. Every other line is kept byte for byte. The write is atomic
// (temp file + rename); a .gz file stays gzip; the file's mode and mtime are
// kept, so an index rebuild from disk reads the same activity time. Reports
// whether the file changed; a file without a branch_meta first line, or one
// rewrite declines, is left untouched.
func RewriteBranchParent(path string, rewrite func(parent string) (string, bool)) (bool, error) {
	first, rest, err := readFirstLine(path, true)
	if err != nil {
		return false, err
	}
	var meta BranchMeta
	if json.Unmarshal(first, &meta) != nil || meta.Type != "branch_meta" {
		return false, nil
	}
	next, change := rewrite(meta.ParentKey)
	if !change || next == meta.ParentKey {
		return false, nil
	}
	meta.ParentKey = next
	newFirst, err := json.Marshal(meta)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}

	var body bytes.Buffer
	body.Write(newFirst)
	body.WriteByte('\n')
	body.Write(rest)
	data := body.Bytes()
	if strings.HasSuffix(path, ".gz") {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		if _, err := zw.Write(data); err != nil {
			return false, err
		}
		if err := zw.Close(); err != nil {
			return false, err
		}
		data = gz.Bytes()
	}

	tmp := path + ".migrate-tmp"
	if err := os.WriteFile(tmp, data, info.Mode().Perm()); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Chmod(tmp, info.Mode().Perm()); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// readFirstLine returns a session file's first line (without its newline)
// and, when withRest is set, every byte after that newline. A .gz file is
// read through gzip.
func readFirstLine(path string, withRest bool) (first, rest []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}
	br := bufio.NewReaderSize(r, 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	first = bytes.TrimSuffix(line, []byte("\n"))
	if withRest {
		if rest, err = io.ReadAll(br); err != nil {
			return nil, nil, err
		}
	}
	return first, rest, nil
}

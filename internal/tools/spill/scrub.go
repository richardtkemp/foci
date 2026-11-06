package spill

import (
	"errors"
	"io"
	"os"
)

const (
	scrubChunkBytes = 1 << 20
	// scrubOverlapBytes must be at least the longest secret value: it is the tail
	// of each chunk held back so a value split across two reads is still found.
	// The longest values in use are PEM private keys of a few KB.
	scrubOverlapBytes = 64 << 10
)

// ScrubFile rewrites a spill file with redact applied, in bounded memory (a
// shell command's output has no total cap). A spill file reaches disk unscrubbed,
// and it may be handed to an agent or the app. On an error the file is unchanged,
// and so still raw: the caller must not hand it on.
func ScrubFile(path string, redact func(string) string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	// Beside the original (a spill file's name is already unique), so the rename
	// below stays on one filesystem.
	tmp, err := os.OpenFile(path+".scrub", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op after the rename
	if err := scrubStream(in, tmp, redact); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func scrubStream(in io.Reader, out io.Writer, redact func(string) string) error {
	buf := make([]byte, scrubChunkBytes)
	carry := ""
	for {
		n, readErr := io.ReadFull(in, buf)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		text := redact(carry + string(buf[:n]))
		if readErr != nil { // the last chunk
			_, err := io.WriteString(out, text)
			return err
		}
		keep := min(scrubOverlapBytes, len(text))
		if _, err := io.WriteString(out, text[:len(text)-keep]); err != nil {
			return err
		}
		carry = text[len(text)-keep:]
	}
}

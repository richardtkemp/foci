package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/procx"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/go-shiori/go-readability"
)

// Convertible MIME types and their required tools.
const (
	mimeDocx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimePptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	mimeHTML = "text/html"
	mimeCSV  = "text/csv"
	mimeTXT  = "text/plain"
)

// convertResult holds the output of a document conversion attempt.
type convertResult struct {
	Text string // converted text content
	Err  string // user-facing error message (empty on success)
}

// convertDocument converts document data to plain text based on MIME type.
// savedPath is the on-disk path to the file (needed for external tool conversion).
// Returns the converted text or a user-facing error message.
func convertDocument(data []byte, mimeType, savedPath string) convertResult {
	mimeType = platform.NormalizeMIME(mimeType)
	switch mimeType {
	case mimeCSV, mimeTXT:
		return convertResult{Text: string(data)}
	case mimeHTML:
		return convertHTML(data)
	case mimeDocx:
		return convertWithPandoc(savedPath, "docx")
	case mimePptx:
		return convertWithPandoc(savedPath, "pptx")
	case mimeXlsx:
		return convertXlsx(savedPath)
	default:
		return convertResult{Err: fmt.Sprintf("Unsupported document type: %s", mimeType)}
	}
}

// convertHTML extracts readable content from HTML using readability,
// then converts to markdown. Reuses the same libraries as web_fetch.
func convertHTML(data []byte) convertResult {
	var htmlContent string
	article, err := readability.FromReader(bytes.NewReader(data), nil)
	if err == nil && strings.TrimSpace(article.Content) != "" {
		htmlContent = article.Content
	} else {
		htmlContent = string(data)
	}

	md, err := htmltomarkdown.ConvertString(htmlContent)
	if err != nil {
		if article.TextContent != "" {
			return convertResult{Text: article.TextContent}
		}
		return convertResult{Text: string(data)}
	}
	return convertResult{Text: md}
}

// convertTimeout is the maximum time allowed for external document conversion tools.
const convertTimeout = 30 * time.Second

// maxConvertOutputBytes caps the text output of an external conversion tool. A
// small office file can decompress into gigabytes of text (a zip bomb), so the
// converter's stdout is bounded and the subprocess killed on overflow rather
// than buffered unbounded into memory. A var (not const) so tests can lower it.
// (P2-7.)
var maxConvertOutputBytes = 16 << 20 // 16 MiB

// limitedBuffer accumulates writer output up to a byte cap. On the first write
// that would exceed the cap it stores what fits, sets overflowed, and invokes
// onOverflow once (used to cancel the conversion subprocess); subsequent writes
// are swallowed. It always reports full consumption so the os/exec output
// copier does not treat the cap as an I/O error.
type limitedBuffer struct {
	buf        bytes.Buffer
	max        int
	onOverflow func()
	overflowed bool
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	if lb.overflowed {
		return len(p), nil
	}
	if lb.buf.Len()+len(p) > lb.max {
		if remaining := lb.max - lb.buf.Len(); remaining > 0 {
			lb.buf.Write(p[:remaining])
		}
		lb.overflowed = true
		if lb.onOverflow != nil {
			lb.onOverflow()
		}
		return len(p), nil
	}
	return lb.buf.Write(p)
}

// runBounded runs cmd capturing stdout up to maxConvertOutputBytes. If the
// process emits more it is cancelled (via cancel) and overflowed is true. The
// os/exec output copier runs in its own goroutine but cmd.Run waits for it, so
// reading the returned buffers/flag after Run is race-free.
func runBounded(cmd *exec.Cmd, cancel context.CancelFunc) (stdout, stderr string, overflowed bool, err error) {
	lb := &limitedBuffer{max: maxConvertOutputBytes, onOverflow: cancel}
	var errBuf bytes.Buffer
	cmd.Stdout = lb
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return lb.buf.String(), errBuf.String(), lb.overflowed, err
}

// convertWithPandoc converts a document file to plain text using pandoc.
// Runs pandoc directly rather than pre-checking LookPath, which can fail
// spuriously in some process environments even when pandoc is installed.
func convertWithPandoc(path, format string) convertResult {
	ctx, cancel := context.WithTimeout(context.Background(), convertTimeout)
	defer cancel()
	cmd := procx.Spawn(ctx, "pandoc", "-f", format, "-t", "plain", "--wrap=none", path)
	stdout, stderr, overflowed, err := runBounded(cmd, cancel)
	if overflowed {
		return convertResult{Err: fmt.Sprintf(".%s conversion produced more than %d MB of text — refusing (possible zip bomb)", format, maxConvertOutputBytes/(1<<20))}
	}
	if err != nil {
		if isExecNotFound(err) {
			return convertResult{Err: fmt.Sprintf("Need pandoc to read .%s files. Install: https://pandoc.org/installing.html", format)}
		}
		log.Debugf("convert", "pandoc failed (PATH=%s): %v — stderr: %s", os.Getenv("PATH"), err, strings.TrimSpace(stderr))
		return convertResult{Err: fmt.Sprintf("pandoc conversion failed: %s", strings.TrimSpace(stderr))}
	}
	return convertResult{Text: stdout}
}

// isExecNotFound returns true if the error indicates the executable was not found.
func isExecNotFound(err error) bool {
	var notFound *exec.Error
	return errors.As(err, &notFound) && errors.Is(notFound.Err, exec.ErrNotFound)
}

// convertXlsx converts an xlsx file to CSV text using ssconvert (from gnumeric)
// or falls back to pandoc.
func convertXlsx(path string) convertResult {
	// Try ssconvert first (produces clean CSV output)
	ctx, cancel := context.WithTimeout(context.Background(), convertTimeout)
	defer cancel()
	cmd := procx.Spawn(ctx, "ssconvert", "--export-type=Gnumeric_stf:stf_csv", path, "fd://1")
	stdout, _, overflowed, err := runBounded(cmd, cancel)
	if overflowed {
		return convertResult{Err: fmt.Sprintf("xlsx conversion produced more than %d MB of text — refusing (possible zip bomb)", maxConvertOutputBytes/(1<<20))}
	}
	if err == nil && len(stdout) > 0 {
		return convertResult{Text: stdout}
	} else if err != nil && !isExecNotFound(err) {
		log.Debugf("convert", "ssconvert failed: %v", err)
	}

	// Fall back to pandoc
	result := convertWithPandoc(path, "xlsx")
	if result.Err != "" && strings.Contains(result.Err, "Need pandoc") {
		return convertResult{Err: "Need ssconvert (gnumeric) or pandoc to read .xlsx files"}
	}
	return result
}

// labelForMIME returns a human-readable label for a MIME type.
// Handles parameterized and legacy MIME types.
func labelForMIME(mime string) string {
	mime = platform.NormalizeMIME(mime)
	switch {
	case mime == mimeDocx:
		return "DOCX"
	case mime == mimeXlsx:
		return "XLSX"
	case mime == mimePptx:
		return "PPTX"
	case mime == mimeHTML:
		return "HTML"
	case mime == mimeCSV:
		return "CSV"
	case mime == mimeTXT:
		return "Text"
	case mime == "application/pdf":
		return "PDF"
	case strings.HasPrefix(mime, "image/"):
		return "Image"
	}
	return "Document"
}

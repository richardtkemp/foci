package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"foci/internal/log"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/go-shiori/go-readability"
)

// legacyMIMEMap maps legacy MIME types to their modern convertible equivalents.
// This is defense-in-depth — the telegram layer also normalizes, but non-Telegram
// entry points should also work correctly.
var legacyMIMEMap = map[string]string{
	"application/msword":                                                          mimeDocx,
	"application/vnd.ms-excel":                                                    mimeXlsx,
	"application/vnd.ms-powerpoint":                                               mimePptx,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.template":      mimeDocx,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.template":         mimeXlsx,
	"application/vnd.openxmlformats-officedocument.presentationml.template":        mimePptx,
	"application/vnd.openxmlformats-officedocument.presentationml.slideshow":       mimePptx,
	"application/vnd.ms-word.document.macroEnabled.12":                             mimeDocx,
	"application/vnd.ms-excel.sheet.macroEnabled.12":                               mimeXlsx,
	"application/vnd.ms-powerpoint.presentation.macroEnabled.12":                   mimePptx,
}

// normalizeMIME strips parameters (e.g. "; charset=utf-8") and maps legacy
// MIME types to their modern equivalents.
func normalizeMIME(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mapped, ok := legacyMIMEMap[mime]; ok {
		return mapped
	}
	return mime
}

// Convertible MIME types and their required tools.
const (
	mimeDocx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXlsx = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimePptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	mimeHTML = "text/html"
	mimeCSV  = "text/csv"
	mimeTXT  = "text/plain"
)

// isConvertibleMIME returns true if the MIME type can be converted to text
// for LLM consumption. Handles parameterized and legacy MIME types.
func isConvertibleMIME(mime string) bool {
	switch normalizeMIME(mime) {
	case mimeDocx, mimeXlsx, mimePptx, mimeHTML, mimeCSV, mimeTXT:
		return true
	}
	return false
}

// convertResult holds the output of a document conversion attempt.
type convertResult struct {
	Text string // converted text content
	Err  string // user-facing error message (empty on success)
}

// convertDocument converts document data to plain text based on MIME type.
// savedPath is the on-disk path to the file (needed for external tool conversion).
// Returns the converted text or a user-facing error message.
func convertDocument(data []byte, mimeType, savedPath string) convertResult {
	mimeType = normalizeMIME(mimeType)
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

// convertWithPandoc converts a document file to plain text using pandoc.
// Runs pandoc directly rather than pre-checking LookPath, which can fail
// spuriously in some process environments even when pandoc is installed.
func convertWithPandoc(path, format string) convertResult {
	cmd := exec.Command("pandoc", "-f", format, "-t", "plain", "--wrap=none", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if isExecNotFound(err) {
			return convertResult{Err: fmt.Sprintf("Need pandoc to read .%s files. Install: https://pandoc.org/installing.html", format)}
		}
		log.Debugf("convert", "pandoc failed (PATH=%s): %v — stderr: %s", os.Getenv("PATH"), err, strings.TrimSpace(stderr.String()))
		return convertResult{Err: fmt.Sprintf("pandoc conversion failed: %s", strings.TrimSpace(stderr.String()))}
	}
	return convertResult{Text: stdout.String()}
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
	cmd := exec.Command("ssconvert", "--export-type=Gnumeric_stf:stf_csv", path, "fd://1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil && stdout.Len() > 0 {
		return convertResult{Text: stdout.String()}
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
	mime = normalizeMIME(mime)
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

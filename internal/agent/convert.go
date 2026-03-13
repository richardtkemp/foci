package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

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

// convertWithPandoc converts a document file to plain text using pandoc.
func convertWithPandoc(path, format string) convertResult {
	if _, err := exec.LookPath("pandoc"); err != nil {
		return convertResult{Err: fmt.Sprintf("Need pandoc to read .%s files. Install: https://pandoc.org/installing.html", format)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), convertTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pandoc", "-f", format, "-t", "plain", "--wrap=none", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return convertResult{Err: fmt.Sprintf("pandoc conversion failed: %s", strings.TrimSpace(stderr.String()))}
	}
	return convertResult{Text: stdout.String()}
}

// convertXlsx converts an xlsx file to CSV text using ssconvert (from gnumeric)
// or falls back to pandoc.
func convertXlsx(path string) convertResult {
	// Try ssconvert first (produces clean CSV output)
	if ssconvert, err := exec.LookPath("ssconvert"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), convertTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, ssconvert, "--export-type=Gnumeric_stf:stf_csv", path, "fd://1")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil && stdout.Len() > 0 {
			return convertResult{Text: stdout.String()}
		}
	}

	// Fall back to pandoc
	if _, err := exec.LookPath("pandoc"); err == nil {
		return convertWithPandoc(path, "xlsx")
	}

	return convertResult{Err: "Need ssconvert (gnumeric) or pandoc to read .xlsx files"}
}

// labelForMIME returns a human-readable label for a MIME type.
func labelForMIME(mime string) string {
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

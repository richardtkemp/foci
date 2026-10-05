package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"foci/internal/platform"
	"foci/internal/procx"
	"foci/internal/tempdir"
	"foci/internal/tools"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
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

// convertHTML extracts readable content from HTML using web_fetch's
// readability config (tools.ParseArticle, which keeps list-only sections —
// #2011/#2049), then converts to markdown.
func convertHTML(data []byte) convertResult {
	var htmlContent string
	article, err := tools.ParseArticle(bytes.NewReader(data), nil)
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
	// Trusted: foci converting an inbound attachment. pandoc runs on bytes a
	// remote sender chose, so the binary itself must not be agent-substitutable.
	cmd := procx.Spawn(ctx, procx.Trusted, "pandoc", "-f", format, "-t", "plain", "--wrap=none", path)
	stdout, stderr, overflowed, err := runBounded(cmd, cancel)
	if overflowed {
		return convertResult{Err: fmt.Sprintf(".%s conversion produced more than %d MB of text — refusing (possible zip bomb)", format, maxConvertOutputBytes/(1<<20))}
	}
	if err != nil {
		if isExecNotFound(err) {
			return convertResult{Err: fmt.Sprintf("Need pandoc to read .%s files. Install: https://pandoc.org/installing.html", format)}
		}
		convertLog.Debugf("pandoc failed (PATH=%s): %v — stderr: %s", os.Getenv("PATH"), err, strings.TrimSpace(stderr))
		return convertResult{Err: fmt.Sprintf("pandoc conversion failed: %s", strings.TrimSpace(stderr))}
	}
	return convertResult{Text: stdout}
}

// isExecNotFound returns true if the error indicates the executable was not found.
func isExecNotFound(err error) bool {
	var notFound *exec.Error
	return errors.As(err, &notFound) && errors.Is(notFound.Err, exec.ErrNotFound)
}

// sofficeTimeout bounds a LibreOffice conversion. Longer than convertTimeout:
// each run builds a fresh user profile (see convertXlsxSoffice), which alone
// takes a couple of seconds on an idle host.
const sofficeTimeout = 90 * time.Second

// xlsxSheet is one exported worksheet: its name (when known) and the CSV file
// the converter wrote for it.
type xlsxSheet struct {
	name string
	path string
}

// convertXlsx converts a spreadsheet to CSV text, one section per sheet. It
// tries ssconvert (gnumeric) first, then LibreOffice. pandoc is not tried: it
// has no xlsx reader (#2171).
func convertXlsx(path string) convertResult {
	res, found := convertXlsxWith(path, "ssconvert", ssconvertArgs)
	if found && res.Err == "" {
		return res
	}
	if found {
		convertLog.Debugf("ssconvert failed (%s); trying soffice", res.Err)
	}
	res2, found2 := convertXlsxWith(path, "soffice", sofficeArgs)
	switch {
	case found2:
		return res2
	case found:
		return res
	}
	return convertResult{Err: "Need ssconvert (gnumeric) or LibreOffice (soffice) to read .xlsx files"}
}

// ssconvertArgs exports every sheet (-S) to outDir as <index>-<name>.csv.
func ssconvertArgs(path, outDir, _ string) []string {
	return []string{"-S", "--export-type=Gnumeric_stf:stf_csv", path, filepath.Join(outDir, "%n-%s.csv")}
}

// sofficeArgs exports every sheet to outDir as CSV. The 12th CSV filter token
// (-1) asks LibreOffice 7.2+ for one file per sheet; older versions export the
// first sheet only. profileDir isolates the run: LibreOffice locks its user
// profile, so two conversions sharing one would collide.
func sofficeArgs(path, outDir, profileDir string) []string {
	return []string{
		"-env:UserInstallation=file://" + profileDir,
		"--headless", "--norestore", "--nolockcheck",
		"--convert-to", "csv:Text - txt - csv (StarCalc):44,34,76,1,,0,false,true,false,false,false,-1",
		"--outdir", outDir, path,
	}
}

// convertXlsxWith runs one converter into a private temp dir and gathers the
// CSV files it wrote. found is false when the tool is not installed.
func convertXlsxWith(path, tool string, args func(path, outDir, profileDir string) []string) (res convertResult, found bool) {
	work, err := tempdir.Mkdir("xlsx-convert-*")
	if err != nil {
		return convertResult{Err: fmt.Sprintf("xlsx conversion: temp dir: %v", err)}, true
	}
	defer func() { _ = os.RemoveAll(work) }()
	outDir, profileDir := filepath.Join(work, "out"), filepath.Join(work, "profile")
	if err := os.Mkdir(outDir, 0o700); err != nil {
		return convertResult{Err: fmt.Sprintf("xlsx conversion: %v", err)}, true
	}

	timeout := convertTimeout
	if tool == "soffice" {
		timeout = sofficeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Trusted: as convertWithPandoc — foci's own document machinery.
	cmd := procx.Spawn(ctx, procx.Trusted, tool, args(path, outDir, profileDir)...)
	// soffice is a launcher script over soffice.bin: kill the whole process
	// group (Spawn sets Setpgid), and stop waiting on pipes a straggler holds.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	var dirOverflow atomic.Bool
	done := make(chan struct{})
	go watchDirSize(outDir, maxConvertOutputBytes, done, func() { dirOverflow.Store(true); cancel() })
	stdout, stderr, overflowed, err := runBounded(cmd, cancel)
	close(done)

	if isExecNotFound(err) {
		return convertResult{}, false
	}
	if overflowed || dirOverflow.Load() {
		return xlsxTooBig(), true
	}
	if err != nil {
		convertLog.Debugf("%s failed: %v — stderr: %s", tool, err, strings.TrimSpace(stderr))
		if ctx.Err() == context.DeadlineExceeded {
			return convertResult{Err: fmt.Sprintf("%s xlsx conversion timed out after %s", tool, timeout)}, true
		}
		return convertResult{Err: fmt.Sprintf("%s xlsx conversion failed (%v): %s", tool, err, strings.TrimSpace(stderr))}, true
	}

	sheets := xlsxSheets(stdout, outDir)
	if len(sheets) == 0 {
		return convertResult{Err: fmt.Sprintf("%s xlsx conversion produced no output: %s", tool, strings.TrimSpace(stdout+" "+stderr))}, true
	}
	return joinXlsxSheets(sheets), true
}

func xlsxTooBig() convertResult {
	return convertResult{Err: fmt.Sprintf("xlsx conversion produced more than %d MB of text — refusing (possible zip bomb)", maxConvertOutputBytes/(1<<20))}
}

// xlsxDirPollInterval is how often watchDirSize re-measures the output dir.
const xlsxDirPollInterval = 50 * time.Millisecond

// watchDirSize calls onOverflow once if the files in dir grow past max bytes,
// polling until done closes. The converters write to files, not stdout, so
// runBounded's stdout cap cannot stop a zip bomb on its own.
func watchDirSize(dir string, max int, done <-chan struct{}, onOverflow func()) {
	t := time.NewTicker(xlsxDirPollInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if dirBytes(dir) > int64(max) {
				onOverflow()
				return
			}
		}
	}
}

func dirBytes(dir string) int64 {
	entries, _ := os.ReadDir(dir)
	var n int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			n += info.Size()
		}
	}
	return n
}

// xlsxSheets lists the CSV files a converter left in outDir, in workbook
// order. soffice reports each as "Writing sheet <name> -> <path>"; ssconvert's
// files are named <index>-<name>.csv. Any CSV neither accounts for (e.g. an
// older LibreOffice's single <base>.csv) is appended in name order.
func xlsxSheets(stdout, outDir string) []xlsxSheet {
	var sheets []xlsxSheet
	seen := map[string]bool{}
	sep := " -> " + outDir + string(filepath.Separator)
	for _, line := range strings.Split(stdout, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Writing sheet ")
		if !ok {
			continue
		}
		i := strings.LastIndex(rest, sep)
		if i < 0 {
			continue
		}
		p := filepath.Join(outDir, filepath.Base(rest[i+len(sep):]))
		if !seen[p] {
			seen[p] = true
			sheets = append(sheets, xlsxSheet{name: rest[:i], path: p})
		}
	}

	entries, _ := os.ReadDir(outDir)
	type indexed struct {
		n int
		xlsxSheet
	}
	var rest []indexed
	for _, e := range entries {
		p := filepath.Join(outDir, e.Name())
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".csv") || seen[p] {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".csv")
		item := indexed{n: -1, xlsxSheet: xlsxSheet{path: p}}
		if num, name, ok := strings.Cut(base, "-"); ok {
			if n, err := strconv.Atoi(num); err == nil {
				item.n, item.name = n, name
			}
		}
		rest = append(rest, item)
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].n != rest[j].n {
			return rest[i].n < rest[j].n
		}
		return rest[i].path < rest[j].path
	})
	for _, r := range rest {
		sheets = append(sheets, r.xlsxSheet)
	}
	return sheets
}

// joinXlsxSheets reads the sheet CSVs into one text, each under a
// "--- Sheet: <name> ---" header when there is more than one, refusing past
// maxConvertOutputBytes in total.
func joinXlsxSheets(sheets []xlsxSheet) convertResult {
	var b strings.Builder
	for i, sh := range sheets {
		data, err := os.ReadFile(sh.path)
		if err != nil {
			return convertResult{Err: fmt.Sprintf("xlsx conversion: reading sheet output: %v", err)}
		}
		if b.Len()+len(data) > maxConvertOutputBytes {
			return xlsxTooBig()
		}
		if len(sheets) > 1 {
			if i > 0 {
				b.WriteString("\n")
			}
			name := sh.name
			if name == "" {
				name = strconv.Itoa(i + 1)
			}
			fmt.Fprintf(&b, "--- Sheet: %s ---\n", name)
		}
		b.Write(data)
	}
	return convertResult{Text: b.String()}
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

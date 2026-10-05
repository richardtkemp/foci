package agent

import (
	"archive/zip"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/platform"
	"foci/internal/procx"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

// TestLimitedBufferCapsAndCancels proves limitedBuffer stops accumulating at
// its byte cap and fires onOverflow exactly once (used to kill the conversion
// subprocess), so a zip-bomb expansion can't be buffered into memory. (P2-7.)
func TestLimitedBufferCapsAndCancels(t *testing.T) {
	var cancels int
	lb := &limitedBuffer{max: 10, onOverflow: func() { cancels++ }}

	n, err := lb.Write([]byte("12345"))
	if err != nil || n != 5 {
		t.Fatalf("under-cap write: n=%d err=%v", n, err)
	}
	if lb.overflowed {
		t.Fatal("should not overflow under cap")
	}
	// 5 + 10 exceeds the cap of 10.
	n, err = lb.Write([]byte("6789012345"))
	if err != nil || n != 10 {
		t.Fatalf("over-cap write should report full consumption: n=%d err=%v", n, err)
	}
	if !lb.overflowed {
		t.Error("should be overflowed after exceeding cap")
	}
	if lb.buf.Len() != 10 {
		t.Errorf("buffered = %d bytes, want capped at 10", lb.buf.Len())
	}
	lb.Write([]byte("more")) // further writes are swallowed
	if cancels != 1 {
		t.Errorf("onOverflow fired %d times, want exactly 1", cancels)
	}
}

// TestConvertBoundedKillsRunaway proves a converter emitting unbounded output
// (a proxy for a zip bomb) is capped and the subprocess killed promptly rather
// than OOMing the gateway. (P2-7.)
func TestConvertBoundedKillsRunaway(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	orig := maxConvertOutputBytes
	maxConvertOutputBytes = 4096
	defer func() { maxConvertOutputBytes = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := procx.Spawn(ctx, procx.Trusted, "sh", "-c", "while true; do printf 'spamspamspamspam'; done")
	_, _, overflowed, _ := runBounded(cmd, cancel)
	if !overflowed {
		t.Error("expected overflow + kill on runaway output")
	}
}

func TestConvertCSV(t *testing.T) {
	// Verifies that CSV documents pass through as plain text
	// with no external tool dependency.
	data := []byte("name,age\nAlice,30\nBob,25")
	result := convertDocument(data, mimeCSV, "/tmp/test.csv")
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Text != string(data) {
		t.Errorf("CSV text = %q, want %q", result.Text, string(data))
	}
}

func TestConvertPlainText(t *testing.T) {
	// Verifies that text/plain documents pass through unchanged.
	data := []byte("Hello, world!")
	result := convertDocument(data, mimeTXT, "/tmp/test.txt")
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Text != "Hello, world!" {
		t.Errorf("text = %q", result.Text)
	}
}

func TestConvertHTML(t *testing.T) {
	// Verifies that HTML documents are converted to markdown
	// using readability extraction.
	html := []byte(`<html><body>
		<article>
			<h1>Test Article</h1>
			<p>This is a paragraph with <strong>bold</strong> text.</p>
		</article>
	</body></html>`)
	result := convertDocument(html, mimeHTML, "/tmp/test.html")
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Text == "" {
		t.Fatal("expected non-empty text from HTML conversion")
	}
	// The output should contain some text from the original HTML
	if !strings.Contains(result.Text, "paragraph") && !strings.Contains(result.Text, "bold") {
		t.Errorf("converted HTML doesn't contain expected content: %q", result.Text)
	}
}

// TestConvertHTMLKeepsListOnlySections proves an HTML attachment goes through
// web_fetch's readability config (#2049): on the #2011 Lever page, default
// readability drops the heading-plus-bullets "What We Value" section.
func TestConvertHTMLKeepsListOnlySections(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "tools", "testdata", "webfetch_corpus", "lever_palantir_fdae.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	result := convertDocument(data, mimeHTML, "/tmp/test.html")
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if !strings.Contains(result.Text, "Engineering mindset") {
		t.Errorf("list-only section dropped from converted HTML:\n%s", result.Text)
	}
}

func TestConvertHTMLMinimal(t *testing.T) {
	// Verifies that even minimal/broken HTML returns something.
	html := []byte("<p>Just a paragraph</p>")
	result := convertDocument(html, mimeHTML, "/tmp/test.html")
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if !strings.Contains(result.Text, "paragraph") {
		t.Errorf("expected 'paragraph' in output, got %q", result.Text)
	}
}

func TestConvertDocxNoPandoc(t *testing.T) {
	// Override PATH to ensure pandoc isn't found
	t.Setenv("PATH", "")
	result := convertDocument([]byte("fake"), mimeDocx, "/tmp/test.docx")
	if result.Err == "" {
		t.Fatal("expected error when pandoc not installed")
	}
	if !strings.Contains(result.Err, "pandoc") {
		t.Errorf("error should mention pandoc: %q", result.Err)
	}
}

func TestConvertPptxNoPandoc(t *testing.T) {
	// Verifies that pptx conversion returns a helpful
	// error message when pandoc is not installed.
	t.Setenv("PATH", "")
	result := convertDocument([]byte("fake"), mimePptx, "/tmp/test.pptx")
	if result.Err == "" {
		t.Fatal("expected error when pandoc not installed")
	}
	if !strings.Contains(result.Err, "pandoc") {
		t.Errorf("error should mention pandoc: %q", result.Err)
	}
}

func TestConvertXlsxNoTools(t *testing.T) {
	// Verifies that xlsx conversion returns a helpful
	// error message when neither ssconvert nor soffice is installed.
	t.Setenv("PATH", "")
	result := convertDocument([]byte("fake"), mimeXlsx, "/tmp/test.xlsx")
	if result.Err == "" {
		t.Fatal("expected error when conversion tools not installed")
	}
	if !strings.Contains(result.Err, "ssconvert") || !strings.Contains(result.Err, "soffice") {
		t.Errorf("error should mention required tools: %q", result.Err)
	}
}

// writeTestXlsx writes a minimal two-sheet workbook ("Alpha", "Beta") to path.
func writeTestXlsx(t *testing.T, path string) {
	t.Helper()
	sheet := func(rows ...[2]string) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
		for i, r := range rows {
			fmt.Fprintf(&b, `<row r="%d"><c r="A%d" t="inlineStr"><is><t>%s</t></is></c><c r="B%d" t="inlineStr"><is><t>%s</t></is></c></row>`, i+1, i+1, r[0], i+1, r[1])
		}
		b.WriteString(`</sheetData></worksheet>`)
		return b.String()
	}
	const rels = `http://schemas.openxmlformats.org/officeDocument/2006/relationships`
	files := []struct{ name, body string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/><Override PartName="/xl/worksheets/sheet2.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="` + rels + `/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="` + rels + `"><sheets><sheet name="Alpha" sheetId="1" r:id="rId1"/><sheet name="Beta" sheetId="2" r:id="rId2"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="` + rels + `/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="` + rels + `/worksheet" Target="worksheets/sheet2.xml"/></Relationships>`},
		{"xl/worksheets/sheet1.xml", sheet([2]string{"name", "qty"}, [2]string{"apple", "three"})},
		{"xl/worksheets/sheet2.xml", sheet([2]string{"month", "total"}, [2]string{"Jan", "forty-two"})},
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, file := range files {
		w, err := zw.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(file.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestConvertXlsxSofficeAllSheets is the #2171 repro: on a host with
// LibreOffice but no ssconvert (and a pandoc that cannot read xlsx), an xlsx
// must convert, with every sheet present. The blob has no extension, as app
// blobs did before #2171.
func TestConvertXlsxSofficeAllSheets(t *testing.T) {
	if _, err := procx.LookPath(procx.Trusted, "soffice"); err != nil {
		t.Skip("soffice not installed")
	}
	if _, err := procx.LookPath(procx.Trusted, "ssconvert"); err == nil {
		t.Skip("ssconvert installed: it would be used instead of soffice")
	}
	// LibreOffice binds its IPC socket in /tmp whatever TMPDIR says; the
	// sealed test run (scripts/seal-test.sh) cannot write there.
	probe := fmt.Sprintf("/tmp/foci-soffice-probe-%d.sock", os.Getpid())
	l, err := net.Listen("unix", probe)
	if err != nil {
		t.Skipf("cannot bind a socket in /tmp (sealed test run?): %v", err)
	}
	_ = l.Close()
	_ = os.Remove(probe)
	path := filepath.Join(t.TempDir(), "01JBLOBWITHNOEXTENSION0000")
	writeTestXlsx(t, path)

	result := convertDocument(nil, mimeXlsx, path)
	if result.Err != "" {
		t.Fatalf("conversion failed: %s", result.Err)
	}
	want := "--- Sheet: Alpha ---\nname,qty\napple,three\n\n--- Sheet: Beta ---\nmonth,total\nJan,forty-two\n"
	if result.Text != want {
		t.Errorf("text = %q\nwant   %q", result.Text, want)
	}
}

// fakeTool writes an executable sh script named name into dir.
func fakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestConvertXlsxSsconvertFailureFallsBackToSoffice pins the ruling's order
// (#2171): ssconvert first, soffice when it fails; sheets come back in the
// order soffice reports them, not filename order.
func TestConvertXlsxSsconvertFailureFallsBackToSoffice(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "ssconvert", "echo 'ssconvert: cannot read' >&2; exit 1\n")
	// Args: -env:... --headless --norestore --nolockcheck --convert-to <f> --outdir <dir> <path>
	fakeTool(t, bin, "soffice", `out=$8
printf 'b,2\n' > "$out/x-Second.csv"
printf 'a,1\n' > "$out/x-First.csv"
echo "Writing sheet Zed -> $out/x-Second.csv"
echo "Writing sheet Ann -> $out/x-First.csv"
`)
	t.Setenv("PATH", bin)

	result := convertDocument(nil, mimeXlsx, "/nonexistent/in.xlsx")
	if result.Err != "" {
		t.Fatalf("conversion failed: %s", result.Err)
	}
	want := "--- Sheet: Zed ---\nb,2\n\n--- Sheet: Ann ---\na,1\n"
	if result.Text != want {
		t.Errorf("text = %q, want %q", result.Text, want)
	}
}

// TestConvertXlsxSsconvertPerSheetFiles: ssconvert -S names its files
// <index>-<sheet>.csv; they are ordered by index (10 after 2) and labelled.
func TestConvertXlsxSsconvertPerSheetFiles(t *testing.T) {
	bin := t.TempDir()
	// Args: -S --export-type=... <path> <outdir>/%n-%s.csv
	fakeTool(t, bin, "ssconvert", `out=${4%/*}
printf 'x\n' > "$out/10-Late-Sheet.csv"
printf 'y\n' > "$out/2-Early.csv"
`)
	t.Setenv("PATH", bin)

	result := convertDocument(nil, mimeXlsx, "/nonexistent/in.xlsx")
	if result.Err != "" {
		t.Fatalf("conversion failed: %s", result.Err)
	}
	want := "--- Sheet: Early ---\ny\n\n--- Sheet: Late-Sheet ---\nx\n"
	if result.Text != want {
		t.Errorf("text = %q, want %q", result.Text, want)
	}
}

// TestConvertXlsxSingleSheetHasNoHeader: one sheet is returned as bare CSV.
func TestConvertXlsxSingleSheetHasNoHeader(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "soffice", `printf 'only,1\n' > "$8/in.csv"
`)
	t.Setenv("PATH", bin)
	result := convertDocument(nil, mimeXlsx, "/nonexistent/in.xlsx")
	if result.Err != "" || result.Text != "only,1\n" {
		t.Errorf("result = %+v, want bare CSV", result)
	}
}

// TestConvertXlsxNoOutputIsAnError: soffice exits 0 even when it cannot load
// the input; no CSV written must surface as an error, not empty content.
func TestConvertXlsxNoOutputIsAnError(t *testing.T) {
	bin := t.TempDir()
	fakeTool(t, bin, "soffice", "echo 'Error: source file could not be loaded'\n")
	t.Setenv("PATH", bin)
	result := convertDocument(nil, mimeXlsx, "/nonexistent/in.xlsx")
	if !strings.Contains(result.Err, "could not be loaded") {
		t.Errorf("err = %q, want the soffice message", result.Err)
	}
}

// TestConvertXlsxCapsFileOutput: the converters write files, not stdout, so a
// zip bomb must be stopped by watching the output dir. The fake writes
// forever; the conversion must refuse and return, not fill the disk.
func TestConvertXlsxCapsFileOutput(t *testing.T) {
	orig := maxConvertOutputBytes
	maxConvertOutputBytes = 64 << 10
	defer func() { maxConvertOutputBytes = orig }()
	bin := t.TempDir()
	fakeTool(t, bin, "soffice", `while :; do printf 'spamspamspamspamspamspamspamspam' >> "$8/in.csv"; done
`)
	t.Setenv("PATH", bin)

	start := time.Now()
	result := convertDocument(nil, mimeXlsx, "/nonexistent/in.xlsx")
	if !strings.Contains(result.Err, "zip bomb") {
		t.Errorf("err = %q, want the zip-bomb refusal", result.Err)
	}
	if d := time.Since(start); d > sofficeTimeout/2 {
		t.Errorf("took %s: the runaway converter was not killed on overflow", d)
	}
}

func TestConvertUnsupportedMIME(t *testing.T) {
	// Verifies that unknown MIME types produce an error.
	result := convertDocument([]byte("data"), "application/zip", "/tmp/test.zip")
	if result.Err == "" {
		t.Fatal("expected error for unsupported MIME type")
	}
	if !strings.Contains(result.Err, "Unsupported") {
		t.Errorf("error = %q", result.Err)
	}
}

func TestIsConvertibleMIME(t *testing.T) {
	// Verifies that the MIME type detection is correct, including
	// parameterized and legacy MIME types.
	tests := []struct {
		mime string
		want bool
	}{
		{mimeDocx, true},
		{mimeXlsx, true},
		{mimePptx, true},
		{mimeHTML, true},
		{mimeCSV, true},
		{mimeTXT, true},
		{"application/pdf", false},
		{"image/jpeg", false},
		{"application/zip", false},
		// Parameterized MIME types
		{"text/html; charset=utf-8", true},
		{"text/plain; charset=us-ascii", true},
		{"text/csv; header=present", true},
		// Legacy Office MIME types
		{"application/msword", true},
		{"application/vnd.ms-excel", true},
		{"application/vnd.ms-powerpoint", true},
	}
	for _, tt := range tests {
		if got := platform.IsConvertibleDocMIME(tt.mime); got != tt.want {
			t.Errorf("platform.IsConvertibleDocMIME(%q) = %v, want %v", tt.mime, got, tt.want)
		}
	}
}

func TestLabelForMIME(t *testing.T) {
	// Verifies the human-readable labels for MIME types, including
	// parameterized and legacy MIME types.
	tests := []struct {
		mime string
		want string
	}{
		{mimeDocx, "DOCX"},
		{mimeXlsx, "XLSX"},
		{mimePptx, "PPTX"},
		{mimeHTML, "HTML"},
		{mimeCSV, "CSV"},
		{mimeTXT, "Text"},
		{"application/pdf", "PDF"},
		{"image/jpeg", "Image"},
		{"image/png", "Image"},
		{"application/zip", "Document"},
		// Parameterized MIME types
		{"text/html; charset=utf-8", "HTML"},
		{"text/csv; header=present", "CSV"},
		// Legacy Office MIME types
		{"application/msword", "DOCX"},
		{"application/vnd.ms-excel", "XLSX"},
		{"application/vnd.ms-powerpoint", "PPTX"},
	}
	for _, tt := range tests {
		if got := labelForMIME(tt.mime); got != tt.want {
			t.Errorf("labelForMIME(%q) = %q, want %q", tt.mime, got, tt.want)
		}
	}
}

func TestConvertHTMLWithCharsetParam(t *testing.T) {
	// Verifies that HTML conversion works when the MIME type includes
	// a charset parameter (e.g. "text/html; charset=utf-8").
	html := []byte("<p>Content with charset param</p>")
	result := convertDocument(html, "text/html; charset=utf-8", "/tmp/test.html")
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if !strings.Contains(result.Text, "Content with charset param") {
		t.Errorf("expected content in output, got %q", result.Text)
	}
}

func TestConvertLegacyDocMIME(t *testing.T) {
	// Verifies that legacy application/msword MIME type is mapped to docx
	// and routed to the pandoc converter (producing a helpful error when
	// pandoc is not installed).
	t.Setenv("PATH", "")
	result := convertDocument([]byte("fake"), "application/msword", "/tmp/test.doc")
	if result.Err == "" {
		t.Fatal("expected error when pandoc not installed")
	}
	if !strings.Contains(result.Err, "pandoc") {
		t.Errorf("error should mention pandoc: %q", result.Err)
	}
}

func TestConvertLegacyXlsxMIME(t *testing.T) {
	// Verifies that legacy application/vnd.ms-excel MIME type is mapped
	// to xlsx and routed to the xlsx converter.
	t.Setenv("PATH", "")
	result := convertDocument([]byte("fake"), "application/vnd.ms-excel", "/tmp/test.xls")
	if result.Err == "" {
		t.Fatal("expected error when conversion tools not installed")
	}
	if !strings.Contains(result.Err, "ssconvert") || !strings.Contains(result.Err, "soffice") {
		t.Errorf("error should mention required tools: %q", result.Err)
	}
}

func TestConvertPlainTextWithCharset(t *testing.T) {
	// Verifies that text/plain with charset parameter passes through unchanged.
	data := []byte("Plain text with charset")
	result := convertDocument(data, "text/plain; charset=us-ascii", "/tmp/test.txt")
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Text != "Plain text with charset" {
		t.Errorf("text = %q", result.Text)
	}
}

func TestConvertAttachmentToTextCSV(t *testing.T) {
	// Verifies that a CSV attachment is converted
	// to a text content block with a header and the file contents.
	ag := &Agent{MaxResultChars: 10000}
	att := platform.Attachment{
		MimeType:  mimeCSV,
		Data:      []byte("a,b\n1,2"),
		SavedPath: "/tmp/test.csv",
	}
	text := ag.convertAttachmentToText("test/session", att)
	if !strings.Contains(text, "[CSV document from: /tmp/test.csv]") {
		t.Errorf("missing header in: %q", text)
	}
	if !strings.Contains(text, "a,b\n1,2") {
		t.Errorf("missing CSV content in: %q", text)
	}
}

func TestConvertAttachmentToTextTruncation(t *testing.T) {
	// Verifies that large converted documents
	// are truncated with a note pointing to the saved file.
	ag := &Agent{MaxResultChars: 50}
	att := platform.Attachment{
		MimeType:  mimeTXT,
		Data:      []byte(strings.Repeat("x", 200)),
		SavedPath: "/tmp/bigfile.txt",
	}
	text := ag.convertAttachmentToText("test/session", att)
	if !strings.Contains(text, "truncated") {
		t.Errorf("expected truncation note in: %q", text)
	}
	if !strings.Contains(text, "/tmp/bigfile.txt") {
		t.Errorf("expected saved path in truncation note: %q", text)
	}
}

func TestConvertAttachmentToTextError(t *testing.T) {
	// Verifies that conversion errors produce
	// a user-facing message rather than crashing.
	t.Setenv("PATH", "")
	ag := &Agent{MaxResultChars: 10000}
	att := platform.Attachment{
		MimeType:  mimeDocx,
		Data:      []byte("fake"),
		SavedPath: "/tmp/test.docx",
	}
	text := ag.convertAttachmentToText("test/session", att)
	if !strings.Contains(text, "pandoc") {
		t.Errorf("expected pandoc mention in error: %q", text)
	}
	if !strings.Contains(text, "/tmp/test.docx") {
		t.Errorf("expected saved path in error: %q", text)
	}
}

func TestHandleMessageWithCSVAttachment(t *testing.T) {
	// Verifies the full pipeline: a CSV attachment
	// is converted to text and included as a content block in the API request.
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("I see CSV data."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:         client,
		Sessions:       store,
		Tools:          tools.NewRegistry(),
		Bootstrap:      bootstrap,
		Model:          "claude-haiku-4-5",
		MaxResultChars: 100000,
	}

	attachments := []platform.Attachment{
		{MimeType: mimeCSV, Data: []byte("name,value\nfoo,42"), SavedPath: "/tmp/data.csv"},
	}
	resp, err := ag.hmTestAttachments(context.Background(), "test/csv", []string{"Analyze this data"}, attachments)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "I see CSV data." {
		t.Errorf("response = %q", resp)
	}

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// Check: should have a text block for the converted CSV, a meta block, and a user text block
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]

	var hasCSV, hasUserText bool
	for _, b := range userMsg.Content {
		if b.Type == "text" && strings.Contains(b.Text, "name,value") && strings.Contains(b.Text, "[CSV document") {
			hasCSV = true
		}
		if b.Type == "text" && strings.Contains(b.Text, "Analyze this data") {
			hasUserText = true
		}
	}
	if !hasCSV {
		t.Error("missing CSV content block")
	}
	if !hasUserText {
		t.Error("missing user text block")
	}
}

func TestHandleMessageWithHTMLAttachment(t *testing.T) {
	// Verifies that HTML attachments are
	// converted to markdown text blocks.
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("I read the HTML."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:         client,
		Sessions:       store,
		Tools:          tools.NewRegistry(),
		Bootstrap:      bootstrap,
		Model:          "claude-haiku-4-5",
		MaxResultChars: 100000,
	}

	html := []byte("<html><body><p>Hello from HTML</p></body></html>")
	attachments := []platform.Attachment{
		{MimeType: mimeHTML, Data: html, SavedPath: "/tmp/page.html"},
	}
	resp, err := ag.hmTestAttachments(context.Background(), "test/ihtml", []string{"What does this say?"}, attachments)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "I read the HTML." {
		t.Errorf("response = %q", resp)
	}

	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]

	// Should have an HTML content block among the text blocks
	var hasHTML bool
	for _, b := range userMsg.Content {
		if b.Type == "text" && strings.Contains(b.Text, "[HTML document") {
			hasHTML = true
		}
	}
	if !hasHTML {
		t.Error("missing HTML content block")
	}
}

package agent

import (
	"context"
	"os/exec"
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
	cmd := procx.Spawn(ctx, "sh", "-c", "while true; do printf 'spamspamspamspam'; done")
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
	// error message when neither ssconvert nor pandoc is installed.
	t.Setenv("PATH", "")
	result := convertDocument([]byte("fake"), mimeXlsx, "/tmp/test.xlsx")
	if result.Err == "" {
		t.Fatal("expected error when conversion tools not installed")
	}
	if !strings.Contains(result.Err, "ssconvert") && !strings.Contains(result.Err, "pandoc") {
		t.Errorf("error should mention required tools: %q", result.Err)
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
	if !strings.Contains(result.Err, "ssconvert") || !strings.Contains(result.Err, "pandoc") {
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

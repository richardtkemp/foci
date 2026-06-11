package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/config"
	"foci/internal/secrets"
)

// TestReadFile_RejectsOversized proves the read tool refuses a file whose stat
// size exceeds the configured limit *before* loading it (large-file OOM guard),
// while a file under the limit reads normally.
func TestReadFile_RejectsOversized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 2048)), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadTool(nil, "", 1024) // 1 KiB cap
	params, _ := json.Marshal(map[string]string{"path": big})
	if _, err := tool.Execute(context.Background(), params); err == nil {
		t.Fatal("expected error for oversized file, got nil")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want 'too large'", err)
	}

	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	pSmall, _ := json.Marshal(map[string]string{"path": small})
	if _, err := tool.Execute(context.Background(), pSmall); err != nil {
		t.Errorf("under-limit read failed: %v", err)
	}
}

// TestEditFile_RejectsOversized proves the edit tool applies the same size gate.
func TestEditFile_RejectsOversized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 2048)), 0644); err != nil {
		t.Fatal(err)
	}
	tool := NewEditTool(nil, "", nil, 0640, 1024)
	params, _ := json.Marshal(map[string]string{"path": big, "old_string": "x", "new_string": "y"})
	if _, err := tool.Execute(context.Background(), params); err == nil {
		t.Fatal("expected error for oversized file, got nil")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want 'too large'", err)
	}
}

func TestReadFile(t *testing.T) {
	// Verifies that the read tool returns file content with line numbers in the expected format.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("line one\nline two\nline three\n"), 0644)

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "line one") {
		t.Errorf("missing content in result: %q", result.Text)
	}
	// Should have line numbers
	if !strings.Contains(result.Text, "   1\t") {
		t.Errorf("missing line numbers in result: %q", result.Text)
	}
}

func TestReadDirectory(t *testing.T) {
	// Verifies that reading a directory path returns a listing with filenames and a trailing slash on subdirectories.
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "aaa.txt"), []byte("x"), 0644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]string{"path": dir})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "aaa.txt") {
		t.Errorf("missing file in listing: %q", result.Text)
	}
	if !strings.Contains(result.Text, "subdir/") {
		t.Errorf("missing trailing slash on directory: %q", result.Text)
	}
}

func TestReadDirectoryEmpty(t *testing.T) {
	// Verifies that reading an empty directory returns a clear "(empty directory)" message rather than blank output.
	t.Parallel()
	dir := t.TempDir()

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]string{"path": dir})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "(empty directory)") {
		t.Errorf("expected empty directory message: %q", result.Text)
	}
}

func TestReadFileMissing(t *testing.T) {
	// Verifies that reading a path that does not exist returns an error rather than empty output.
	t.Parallel()
	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]string{"path": "/nonexistent/file.txt"})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestWriteFile(t *testing.T) {
	// Verifies that the write tool creates a file with the given content and confirms success in the result text.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "output.txt")

	tool := NewWriteTool(nil, "", nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"path":    path,
		"content": "hello world",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Wrote") {
		t.Errorf("result = %q", result.Text)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "hello world" {
		t.Errorf("file content = %q", string(data))
	}
}

func TestWriteFileOverwrite(t *testing.T) {
	// Verifies that writing to an existing file replaces its content completely.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	os.WriteFile(path, []byte("old content"), 0644)

	tool := NewWriteTool(nil, "", nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"path":    path,
		"content": "new content",
	})

	tool.Execute(context.Background(), params)

	data, _ := os.ReadFile(path)
	if string(data) != "new content" {
		t.Errorf("file content = %q, want %q", string(data), "new content")
	}
}

func TestEditFile(t *testing.T) {
	// Verifies that the edit tool replaces a unique occurrence of old_string with new_string in a file.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	os.WriteFile(path, []byte("hello world, hello"), 0644)

	tool := NewEditTool(nil, "", nil, 0640, 0)

	// "hello world" is unique, should work
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "hello world",
		"new_string": "goodbye world",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "Edited") {
		t.Errorf("result = %q", result.Text)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "goodbye world, hello" {
		t.Errorf("file content = %q", string(data))
	}
}

func TestEditFileNotFound(t *testing.T) {
	// Verifies that attempting to replace a string that doesn't exist in the file returns a "not found" error.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	os.WriteFile(path, []byte("foo bar baz"), 0644)

	tool := NewEditTool(nil, "", nil, 0640, 0)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "nonexistent string",
		"new_string": "replacement",
	})

	_, err := tool.Execute(context.Background(), params)
	requireError(t, err, "not found")
}

func TestEditFileNonUnique(t *testing.T) {
	// Verifies that attempting to replace a string that appears more than once returns an error reporting the count.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	os.WriteFile(path, []byte("aaa bbb aaa"), 0644)

	tool := NewEditTool(nil, "", nil, 0640, 0)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "aaa",
		"new_string": "ccc",
	})

	_, err := tool.Execute(context.Background(), params)
	requireError(t, err, "2 times")
}

func TestEditFileMissing(t *testing.T) {
	// Verifies that editing a file path that does not exist returns an error.
	t.Parallel()
	tool := NewEditTool(nil, "", nil, 0640, 0)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       "/nonexistent/file.txt",
		"old_string": "x",
		"new_string": "y",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEditFileSyntaxValidToValid(t *testing.T) {
	// Verifies that editing a syntactically valid file to remain valid succeeds without warnings.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	os.WriteFile(path, []byte(`{"key": "old"}`), 0644)

	tool := NewEditTool(nil, "", nil, 0640, 0)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "old",
		"new_string": "new",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("valid→valid edit should succeed: %v", err)
	}
	if !strings.Contains(result.Text, "Edited") {
		t.Errorf("result = %q", result.Text)
	}

	data, _ := os.ReadFile(path)
	if string(data) != `{"key": "new"}` {
		t.Errorf("file = %q", string(data))
	}
}

func TestEditFileSyntaxValidToInvalid(t *testing.T) {
	// Verifies that an edit that would introduce a syntax error into a valid file is rejected, leaving the file unchanged.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	os.WriteFile(path, []byte(`{"key": "value"}`), 0644)

	tool := NewEditTool(nil, "", nil, 0640, 0)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": `"value"}`,
		"new_string": `"value"`,  // removes closing brace
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("valid→invalid edit should be rejected")
	}
	if !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("error = %q, want syntax error mention", err.Error())
	}

	// File should be unchanged
	data, _ := os.ReadFile(path)
	if string(data) != `{"key": "value"}` {
		t.Errorf("file was modified despite rejection: %q", string(data))
	}
}

func TestEditFileSyntaxInvalidToValid(t *testing.T) {
	// Verifies that fixing a pre-existing syntax error succeeds and produces a warning about the original errors.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	os.WriteFile(path, []byte(`{"key": "value"`), 0644)  // missing closing brace

	tool := NewEditTool(nil, "", nil, 0640, 0)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": `"value"`,
		"new_string": `"value"}`,  // fixes the JSON
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("invalid→valid edit should succeed: %v", err)
	}
	if !strings.Contains(result.Text, "Warning") || !strings.Contains(result.Text, "syntax errors") {
		t.Errorf("expected warning about pre-existing syntax errors, got: %q", result.Text)
	}
}

func TestEditFileSyntaxInvalidToInvalid(t *testing.T) {
	// Verifies that editing an already-invalid file to remain invalid still proceeds, with a warning but no hard error.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	os.WriteFile(path, []byte(`{"key": bad}`), 0644)  // already invalid

	tool := NewEditTool(nil, "", nil, 0640, 0)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "bad",
		"new_string": "worse",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("invalid→invalid edit should proceed: %v", err)
	}
	if !strings.Contains(result.Text, "Warning") {
		t.Errorf("expected warning, got: %q", result.Text)
	}
}

func TestEditFileNoSyntaxCheckForUnknownExt(t *testing.T) {
	// Verifies that files with unrecognised extensions (e.g. .txt) skip syntax validation entirely, producing no warnings.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello"), 0644)

	tool := NewEditTool(nil, "", nil, 0640, 0)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "hello",
		"new_string": "world",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("txt edit should succeed: %v", err)
	}
	if strings.Contains(result.Text, "Warning") {
		t.Errorf("no warning expected for .txt: %q", result.Text)
	}
}

func TestReadLargeFile(t *testing.T) {
	// Verifies that reading a file exceeding the line limit produces a truncation notice indicating how many lines remain.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")

	var sb strings.Builder
	for i := 0; i < 2500; i++ {
		sb.WriteString("line content here\n")
	}
	os.WriteFile(path, []byte(sb.String()), 0644)

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "remaining") {
		t.Error("expected truncation notice for large file")
	}
}

func TestReadFileOffset(t *testing.T) {
	// Verify offset returns lines starting from the given line number.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aaa\nbbb\nccc\nddd\neee\n"), 0644)

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]interface{}{"path": path, "offset": 3})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(result.Text, "aaa") || strings.Contains(result.Text, "bbb") {
		t.Errorf("offset=3 should skip first 2 lines: %q", result.Text)
	}
	if !strings.Contains(result.Text, "ccc") {
		t.Errorf("offset=3 should include line 3: %q", result.Text)
	}
	// Line numbers should reflect the original file
	if !strings.Contains(result.Text, "3\tccc") {
		t.Errorf("line numbers should match original file positions: %q", result.Text)
	}
}

func TestReadFileLimit(t *testing.T) {
	// Verify limit caps the number of lines returned.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aaa\nbbb\nccc\nddd\neee\n"), 0644)

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]interface{}{"path": path, "limit": 2})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "aaa") || !strings.Contains(result.Text, "bbb") {
		t.Errorf("limit=2 should include first 2 lines: %q", result.Text)
	}
	if strings.Contains(result.Text, "ccc") {
		t.Errorf("limit=2 should not include line 3: %q", result.Text)
	}
	if !strings.Contains(result.Text, "remaining") {
		t.Errorf("should show remaining lines notice: %q", result.Text)
	}
}

func TestReadFileOffsetAndLimit(t *testing.T) {
	// Verify offset and limit work together to return a window of lines.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aaa\nbbb\nccc\nddd\neee\n"), 0644)

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]interface{}{"path": path, "offset": 2, "limit": 2})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(result.Text, "aaa") {
		t.Errorf("should not include line before offset: %q", result.Text)
	}
	if !strings.Contains(result.Text, "bbb") || !strings.Contains(result.Text, "ccc") {
		t.Errorf("should include lines 2-3: %q", result.Text)
	}
	if strings.Contains(result.Text, "ddd") {
		t.Errorf("should not include lines beyond limit: %q", result.Text)
	}
}

func TestReadFileOffsetPastEnd(t *testing.T) {
	// Verify offset past end of file returns informative message.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("aaa\nbbb\n"), 0644)

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]interface{}{"path": path, "offset": 100})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result.Text, "past end") {
		t.Errorf("expected past-end message: %q", result.Text)
	}
}

// loadTestStore creates a secrets store with the default blocked paths.
func loadTestStore(t *testing.T) *secrets.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.toml")
	os.WriteFile(path, []byte("[test]\nkey = \"val\"\n"), 0644)
	s, err := secrets.Load(path)
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	return s
}

func TestBlockedPathsAccessDenied(t *testing.T) {
	// All operations on blocked paths should return "access denied".
	t.Parallel()
	store := loadTestStore(t)
	tests := []struct {
		name   string
		tool   *Tool
		params interface{}
	}{
		{"read secrets.toml", NewReadTool(store, "", 0), map[string]string{"path": "secrets.toml"}},
		{"read secrets.toml full path", NewReadTool(store, "", 0), map[string]string{"path": "/home/user/config/secrets.toml"}},
		{"read /proc/self/environ", NewReadTool(store, "", 0), map[string]string{"path": "/proc/self/environ"}},
		{"write secrets.toml", NewWriteTool(store, "", nil, 0640), map[string]interface{}{"path": "secrets.toml", "content": "malicious"}},
		{"edit secrets.toml", NewEditTool(store, "", nil, 0640, 0), map[string]interface{}{"path": "secrets.toml", "old_string": "old", "new_string": "new"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, _ := json.Marshal(tt.params)
			_, err := tt.tool.Execute(context.Background(), params)
			requireError(t, err, "access denied")
		})
	}
}

func TestReadPDF(t *testing.T) {
	// Verifies that reading a .pdf file returns a text description and an ExtraBlocks document block with base64-encoded PDF data.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pdf")
	// Write a small fake PDF (doesn't need to be valid PDF for this test)
	pdfData := []byte("%PDF-1.4 fake content")
	os.WriteFile(path, pdfData, 0644)

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Text should describe the PDF
	if !strings.Contains(result.Text, "[PDF: test.pdf") {
		t.Errorf("text should mention PDF filename, got: %q", result.Text)
	}
	if !strings.Contains(result.Text, "bytes]") {
		t.Errorf("text should mention byte count, got: %q", result.Text)
	}

	// ExtraBlocks should contain a document block
	if len(result.ExtraBlocks) != 1 {
		t.Fatalf("expected 1 extra block, got %d", len(result.ExtraBlocks))
	}
	block := result.ExtraBlocks[0]
	if block.Type != "document" {
		t.Errorf("block.Type = %q, want document", block.Type)
	}
	if block.Source == nil {
		t.Fatal("block.Source is nil")
	}
	if block.Source.MimeType != "application/pdf" {
		t.Errorf("block.Source.MimeType = %q", block.Source.MimeType)
	}
	if block.Source.Data == "" {
		t.Error("block.Source.Data is empty")
	}
}

func TestReadPDFCaseInsensitive(t *testing.T) {
	// Verifies that PDF detection is case-insensitive, so .PDF files are treated the same as .pdf.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "report.PDF")
	os.WriteFile(path, []byte("%PDF-1.4"), 0644)

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.ExtraBlocks) != 1 {
		t.Fatalf("uppercase .PDF should still be detected, got %d extra blocks", len(result.ExtraBlocks))
	}
	if result.ExtraBlocks[0].Type != "document" {
		t.Errorf("block.Type = %q, want document", result.ExtraBlocks[0].Type)
	}
}

// --- resolveAndValidatePath tests ---

func TestResolveAndValidatePath_RelativeInside(t *testing.T) {
	// Verifies that a relative path inside the base directory resolves correctly to an absolute path under it.
	t.Parallel()
	dir := t.TempDir()
	got, err := resolveAndValidatePath("sub/file.txt", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dir, "sub", "file.txt")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveAndValidatePath_Rejected(t *testing.T) {
	// Verifies that absolute paths and path traversal attempts (../) are rejected with appropriate errors.
	t.Parallel()
	dir := t.TempDir()
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{"absolute path", "/etc/passwd", "absolute paths not allowed"},
		{"dotdot traversal", "../../../etc/passwd", "path traversal"},
		{"dotdot in middle", "sub/../../outside", "path traversal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveAndValidatePath(tt.path, dir)
			requireError(t, err, tt.wantErr)
		})
	}
}

func TestResolveAndValidatePath_SymlinkEscape(t *testing.T) {
	// Verifies that a symlink inside the base directory pointing outside is detected and rejected as path traversal.
	t.Parallel()
	dir := t.TempDir()
	// Create a symlink inside baseDir that points outside
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0644)
	os.Symlink(outside, filepath.Join(dir, "escape"))

	_, err := resolveAndValidatePath("escape/secret.txt", dir)
	if err == nil {
		t.Fatal("expected error for symlink escape")
	}
	if !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestResolveAndValidatePath_SymlinkEscapeNewFile(t *testing.T) {
	// Verifies that a symlink escape is blocked even when the target file does not yet exist (write-path protection).
	t.Parallel()
	dir := t.TempDir()
	// Symlink to outside dir — target file doesn't exist yet
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(dir, "link"))

	_, err := resolveAndValidatePath("link/newfile.txt", dir)
	if err == nil {
		t.Fatal("expected error for symlink escape on new file")
	}
	if !strings.Contains(err.Error(), "path traversal") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestResolveAndValidatePath_SymlinkInsideOK(t *testing.T) {
	// Verifies that a symlink whose resolved target remains inside the base directory is allowed.
	t.Parallel()
	dir := t.TempDir()
	// Symlink within baseDir is fine
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "file.txt"), []byte("ok"), 0644)
	os.Symlink(sub, filepath.Join(dir, "link"))

	got, err := resolveAndValidatePath("link/file.txt", dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should resolve to the real path inside dir
	if !strings.HasPrefix(got, dir) {
		t.Errorf("resolved path %q not under base %q", got, dir)
	}
}

func TestResolveAndValidatePath_EmptyBaseDir(t *testing.T) {
	// Verifies that when no base directory is configured, paths are passed through without sandbox enforcement.
	t.Parallel()
	got, err := resolveAndValidatePath("/any/path", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/any/path" {
		t.Errorf("got %q, want passthrough", got)
	}
}

// --- Isolated tool end-to-end tests ---

func TestIsolatedReadInside(t *testing.T) {
	// Verifies that the isolated read tool can read a file that exists within the sandbox directory.
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello\n"), 0644)

	tool := NewIsolatedReadTool(nil, dir)
	params, _ := json.Marshal(map[string]string{"path": "ok.txt"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read inside should succeed: %v", err)
	}
	if !strings.Contains(result.Text, "hello") {
		t.Errorf("result = %q", result.Text)
	}
}

func TestIsolatedReadEscapeBlocked(t *testing.T) {
	// Verifies that the isolated read tool blocks path traversal attempts that would escape the sandbox.
	t.Parallel()
	dir := t.TempDir()
	tool := NewIsolatedReadTool(nil, dir)
	params, _ := json.Marshal(map[string]string{"path": "../../../etc/hostname"})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestIsolatedWriteInside(t *testing.T) {
	// Verifies that the isolated write tool can create a file inside the sandbox and the content is correct.
	t.Parallel()
	dir := t.TempDir()
	tool := NewIsolatedWriteTool(nil, dir, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"path":    "output.txt",
		"content": "written",
	})
	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("write inside should succeed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "output.txt"))
	if string(data) != "written" {
		t.Errorf("content = %q", string(data))
	}
}

func TestIsolatedWriteEscapeBlocked(t *testing.T) {
	// Verifies that the isolated write tool rejects paths that would write outside the sandbox via traversal.
	t.Parallel()
	dir := t.TempDir()
	tool := NewIsolatedWriteTool(nil, dir, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"path":    "../../escape.txt",
		"content": "malicious",
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for path traversal write")
	}
}

func TestIsolatedEditEscapeBlocked(t *testing.T) {
	// Verifies that editing via a symlink that points outside the sandbox is blocked and leaves the target file unmodified.
	t.Parallel()
	dir := t.TempDir()
	// Create a file outside the base dir
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "target.txt"), []byte("original"), 0644)
	// Symlink to it
	os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(dir, "link.txt"))

	tool := NewIsolatedEditTool(nil, dir, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       "link.txt",
		"old_string": "original",
		"new_string": "modified",
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for symlink escape edit")
	}
	// Verify file was not modified
	data, _ := os.ReadFile(filepath.Join(outside, "target.txt"))
	if string(data) != "original" {
		t.Errorf("file was modified despite rejection: %q", string(data))
	}
}

func TestReadAllowedWithStore(t *testing.T) {
	// Verifies that a non-blocked file can still be read when a secrets store is configured.
	t.Parallel()
	store := loadTestStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "allowed.txt")
	os.WriteFile(path, []byte("safe content\n"), 0644)

	tool := NewReadTool(store, "", 0)
	params, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("allowed read should succeed: %v", err)
	}
	if !strings.Contains(result.Text, "safe content") {
		t.Errorf("result = %q, want safe content", result.Text)
	}
}

// --- Config blocked paths tests ---

func TestWriteBlockedByConfig(t *testing.T) {
	// Verifies that writing to a config-blocked directory returns the rebuke message and does not create the file.
	t.Parallel()
	dir := t.TempDir()
	blocked := []config.BlockedPath{
		{Path: dir, Rebuke: "Don't write here, use tmux instead."},
	}
	tool := NewWriteTool(nil, "", blocked, 0640)
	path := filepath.Join(dir, "file.go")
	params, _ := json.Marshal(map[string]interface{}{
		"path":    path,
		"content": "package main",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("config blocked path should return nil error, got: %v", err)
	}
	if result.Text != "Don't write here, use tmux instead." {
		t.Errorf("result = %q, want rebuke message", result.Text)
	}
	// File should not exist
	if _, err := os.Stat(path); err == nil {
		t.Error("file was created despite blocked path")
	}
}

func TestWriteNotBlockedByConfig(t *testing.T) {
	// Verifies that writing to a directory not in the blocked list succeeds normally.
	t.Parallel()
	blockedDir := t.TempDir()
	writeDir := t.TempDir()
	blocked := []config.BlockedPath{
		{Path: blockedDir, Rebuke: "nope"},
	}
	tool := NewWriteTool(nil, "", blocked, 0640)
	path := filepath.Join(writeDir, "ok.txt")
	params, _ := json.Marshal(map[string]interface{}{
		"path":    path,
		"content": "allowed",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("non-blocked write should succeed: %v", err)
	}
	if !strings.Contains(result.Text, "Wrote") {
		t.Errorf("result = %q, want Wrote", result.Text)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "allowed" {
		t.Errorf("content = %q", string(data))
	}
}

func TestEditBlockedByConfig(t *testing.T) {
	// Verifies that editing a file in a config-blocked directory returns the rebuke message and leaves the file unchanged.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.go")
	os.WriteFile(path, []byte("old content"), 0644)

	blocked := []config.BlockedPath{
		{Path: dir, Rebuke: "Use claude via tmux for this workspace."},
	}
	tool := NewEditTool(nil, "", blocked, 0640, 0)
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "old content",
		"new_string": "new content",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("config blocked path should return nil error, got: %v", err)
	}
	if result.Text != "Use claude via tmux for this workspace." {
		t.Errorf("result = %q, want rebuke message", result.Text)
	}
	// File should be unchanged
	data, _ := os.ReadFile(path)
	if string(data) != "old content" {
		t.Errorf("file was modified despite blocked path: %q", string(data))
	}
}

func TestBlockedPathPrefixMatching(t *testing.T) {
	// /naughty blocks /naughty/sub/file.go but not /not-naughty/file.go
	t.Parallel()
	naughty := t.TempDir() // e.g. /tmp/xxx
	notNaughty := t.TempDir()

	blocked := []config.BlockedPath{
		{Path: naughty, Rebuke: "blocked"},
	}

	// Write to naughty subdir — should be blocked
	tool := NewWriteTool(nil, "", blocked, 0640)
	sub := filepath.Join(naughty, "sub")
	os.MkdirAll(sub, 0755)
	params, _ := json.Marshal(map[string]interface{}{
		"path":    filepath.Join(sub, "file.go"),
		"content": "x",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "blocked" {
		t.Errorf("subdir should be blocked, got: %q", result.Text)
	}

	// Write to not-naughty — should succeed
	params, _ = json.Marshal(map[string]interface{}{
		"path":    filepath.Join(notNaughty, "file.go"),
		"content": "ok",
	})
	result, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("not-naughty write failed: %v", err)
	}
	if !strings.Contains(result.Text, "Wrote") {
		t.Errorf("not-naughty should succeed, got: %q", result.Text)
	}
}

func TestExpandTilde(t *testing.T) {
	// Verifies that ~ and ~/path are expanded to the user's home directory, while other paths pass through unchanged.
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home dir: %v", err)
	}

	tests := []struct {
		input string
		want  string
	}{
		{"~", home},
		{"~/config/foci.toml", filepath.Join(home, "config/foci.toml")},
		{"~/", home},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"~user/path", "~user/path"}, // only bare ~ is expanded
		{"", ""},
	}
	for _, tt := range tests {
		got := expandTilde(tt.input)
		if got != tt.want {
			t.Errorf("expandTilde(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestReadFileTildePath(t *testing.T) {
	// Verifies that the read tool expands ~ in paths, resolving to the user's home directory.
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home dir: %v", err)
	}

	// Create a temp file in home dir for testing
	path := filepath.Join(home, ".foci-test-tilde-"+t.Name())
	os.WriteFile(path, []byte("tilde test content\n"), 0644)
	t.Cleanup(func() { os.Remove(path) })

	tool := NewReadTool(nil, "", 0)
	params, _ := json.Marshal(map[string]string{"path": "~/.foci-test-tilde-" + t.Name()})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute with ~ path: %v", err)
	}
	if !strings.Contains(result.Text, "tilde test content") {
		t.Errorf("result = %q, want tilde test content", result.Text)
	}
}

func TestWorkspaceResolution(t *testing.T) {
	// Tests that read/write/edit resolve relative paths against the workspace directory,
	// not the process cwd. Creates a file in a temp "workspace" dir and verifies that
	// tools can access it via a relative path when workspace is set.
	t.Parallel()
	workspace := t.TempDir()
	os.MkdirAll(filepath.Join(workspace, "subdir"), 0755)
	os.WriteFile(filepath.Join(workspace, "subdir", "hello.txt"), []byte("workspace file\n"), 0644)

	// Read with relative path should resolve against workspace
	readTool := NewReadTool(nil, workspace, 0)
	params, _ := json.Marshal(map[string]string{"path": "subdir/hello.txt"})
	result, err := readTool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read relative path: %v", err)
	}
	if !strings.Contains(result.Text, "workspace file") {
		t.Errorf("expected workspace file content, got: %q", result.Text)
	}

	// Write with relative path should resolve against workspace
	writeTool := NewWriteTool(nil, workspace, nil, 0640)
	params, _ = json.Marshal(map[string]interface{}{"path": "subdir/new.txt", "content": "written via workspace"})
	_, err = writeTool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("write relative path: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(workspace, "subdir", "new.txt"))
	if string(data) != "written via workspace" {
		t.Errorf("file content = %q", string(data))
	}

	// Edit with relative path should resolve against workspace
	editTool := NewEditTool(nil, workspace, nil, 0640, 0)
	params, _ = json.Marshal(map[string]interface{}{
		"path": "subdir/hello.txt", "old_string": "workspace file", "new_string": "edited file",
	})
	_, err = editTool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("edit relative path: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(workspace, "subdir", "hello.txt"))
	if !strings.Contains(string(data), "edited file") {
		t.Errorf("file content after edit = %q", string(data))
	}

	// Absolute paths should still work even with workspace set
	absPath := filepath.Join(workspace, "subdir", "hello.txt")
	params, _ = json.Marshal(map[string]string{"path": absPath})
	result, err = readTool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("read absolute path with workspace set: %v", err)
	}
	if !strings.Contains(result.Text, "edited file") {
		t.Errorf("expected edited content via absolute path, got: %q", result.Text)
	}
}

func TestWriteNoBlockedPaths(t *testing.T) {
	// Verifies that write works normally when the blocked paths list is nil (no restrictions configured).
	t.Parallel()
	dir := t.TempDir()
	tool := NewWriteTool(nil, "", nil, 0640)
	path := filepath.Join(dir, "file.txt")
	params, _ := json.Marshal(map[string]interface{}{
		"path":    path,
		"content": "hello",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("write with nil blocked paths should succeed: %v", err)
	}
	if !strings.Contains(result.Text, "Wrote") {
		t.Errorf("result = %q", result.Text)
	}
}

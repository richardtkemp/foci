package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseMultipartRequest is a test helper that extracts form fields and file parts
// from a multipart/form-data request.
func parseMultipartRequest(t *testing.T, r *http.Request) (fields map[string]string, files map[string]struct {
	Filename string
	Content  []byte
}) {
	t.Helper()
	ct := r.Header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", ct, err)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	fields = make(map[string]string)
	files = make(map[string]struct {
		Filename string
		Content  []byte
	})
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		data, _ := io.ReadAll(part)
		if part.FileName() != "" {
			files[part.FormName()] = struct {
				Filename string
				Content  []byte
			}{part.FileName(), data}
		} else {
			fields[part.FormName()] = string(data)
		}
	}
	return
}

func TestHTTPRequestMultipartSingleFile(t *testing.T) {
	// Proves that a single file upload sends a valid multipart/form-data body with the correct field name, filename, and file contents.
	t.Parallel()
	var receivedFields map[string]string
	var receivedFiles map[string]struct {
		Filename string
		Content  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedFields, receivedFiles = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	// Create a temp file to upload
	tmpFile := filepath.Join(t.TempDir(), "test.txt")
	os.WriteFile(tmpFile, []byte("hello multipart"), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "document", "file_path": tmpFile},
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if f, ok := receivedFiles["document"]; !ok {
		t.Error("expected file part 'document'")
	} else {
		if string(f.Content) != "hello multipart" {
			t.Errorf("file content = %q", string(f.Content))
		}
		if f.Filename != "test.txt" {
			t.Errorf("filename = %q, want test.txt", f.Filename)
		}
	}
	if len(receivedFields) != 0 {
		t.Errorf("expected no form fields, got %v", receivedFields)
	}
}

func TestHTTPRequestMultipartFileAndFormFields(t *testing.T) {
	// Proves that form_fields and files are combined into a single multipart request with both the file part and all text fields received correctly.
	t.Parallel()
	var receivedFields map[string]string
	var receivedFiles map[string]struct {
		Filename string
		Content  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedFields, receivedFiles = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	tmpFile := filepath.Join(t.TempDir(), "photo.jpg")
	os.WriteFile(tmpFile, []byte{0xFF, 0xD8, 0xFF}, 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "photo", "file_path": tmpFile},
		},
		"form_fields": map[string]string{
			"chat_id": "12345",
			"caption": "a nice photo",
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
	if _, ok := receivedFiles["photo"]; !ok {
		t.Error("expected file part 'photo'")
	}
	if receivedFields["chat_id"] != "12345" {
		t.Errorf("chat_id = %q", receivedFields["chat_id"])
	}
	if receivedFields["caption"] != "a nice photo" {
		t.Errorf("caption = %q", receivedFields["caption"])
	}
}

func TestHTTPRequestMultipartMultipleFiles(t *testing.T) {
	// Proves that uploading two files in a single request sends both as separate multipart parts with correct names and content.
	t.Parallel()
	var receivedFiles map[string]struct {
		Filename string
		Content  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, receivedFiles = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	file1 := filepath.Join(dir, "a.txt")
	file2 := filepath.Join(dir, "b.txt")
	os.WriteFile(file1, []byte("file-a"), 0644)
	os.WriteFile(file2, []byte("file-b"), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "file1", "file_path": file1},
			{"field_name": "file2", "file_path": file2},
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(receivedFiles) != 2 {
		t.Errorf("expected 2 files, got %d", len(receivedFiles))
	}
	if string(receivedFiles["file1"].Content) != "file-a" {
		t.Errorf("file1 content = %q", string(receivedFiles["file1"].Content))
	}
	if string(receivedFiles["file2"].Content) != "file-b" {
		t.Errorf("file2 content = %q", string(receivedFiles["file2"].Content))
	}
}

func TestHTTPRequestMultipartBodyAndFilesConflict(t *testing.T) {
	// Proves that specifying both "body" and "files" is rejected with a mutually exclusive error, since they would produce conflicting request bodies.
	t.Parallel()
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil, 0640)

	// body + files
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"body":   "some body",
		"files": []map[string]string{
			{"field_name": "f", "file_path": "/tmp/x"},
		},
	})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for body + files")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartFileMissing(t *testing.T) {
	// Proves that referencing a nonexistent file path in the files list returns an error rather than silently skipping the upload.
	t.Parallel()
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "doc", "file_path": "/nonexistent/file.txt"},
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartFileTooLarge(t *testing.T) {
	// Proves that a file exceeding the 50MB upload limit is rejected with an error message mentioning the size cap.
	t.Parallel()
	// We can't easily make a real 50MB file in tests, so test the buildMultipartBody directly
	dir := t.TempDir()
	largePath := filepath.Join(dir, "large.bin")

	// Create a sparse file that's 51MB
	f, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(51 * 1024 * 1024)
	f.Close()

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "doc", "file_path": largePath},
		},
	})

	_, err = tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for oversized file")
	}
	if !strings.Contains(err.Error(), "50MB") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartFormFieldsSecrets(t *testing.T) {
	// Proves that secret templates in form_fields are resolved before sending, so the server receives the actual secret value rather than the template string.
	t.Parallel()
	var receivedFields map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedFields, _ = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	store := writeTestSecrets(t, fmt.Sprintf(`
[custom]
bot_token = "secret-bot-token-123"
allowed_hosts = ["%s"]
allowed_in_body = ["bot_token"]
`, srv.Listener.Addr().(*net.TCPAddr).IP.String()))

	tmpFile := filepath.Join(t.TempDir(), "doc.pdf")
	os.WriteFile(tmpFile, []byte("pdf-content"), 0644)

	tool := NewHTTPRequestTool(store, nil, "", 0, 50*1024*1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "document", "file_path": tmpFile},
		},
		"form_fields": map[string]string{
			"chat_id": "12345",
			"token":   "{{secret:custom.bot_token}}",
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if receivedFields["token"] != "secret-bot-token-123" {
		t.Errorf("token = %q, want resolved secret", receivedFields["token"])
	}
	if receivedFields["chat_id"] != "12345" {
		t.Errorf("chat_id = %q", receivedFields["chat_id"])
	}
}

func TestHTTPRequestMultipartFilenameOverride(t *testing.T) {
	// Proves that the "filename" field overrides the actual file's basename in the multipart Content-Disposition, so the server sees the custom name.
	t.Parallel()
	var receivedFiles map[string]struct {
		Filename string
		Content  []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, receivedFiles = parseMultipartRequest(t, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	tmpFile := filepath.Join(t.TempDir(), "ugly-temp-name-123.bin")
	os.WriteFile(tmpFile, []byte("data"), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "file", "file_path": tmpFile, "filename": "pretty-name.pdf"},
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if f, ok := receivedFiles["file"]; !ok {
		t.Error("expected file part")
	} else if f.Filename != "pretty-name.pdf" {
		t.Errorf("filename = %q, want pretty-name.pdf", f.Filename)
	}
}

func TestHTTPRequestFormFieldsWithoutFiles(t *testing.T) {
	// Proves that specifying form_fields without any files is rejected, since form_fields only makes sense in a multipart context.
	t.Parallel()
	tool := NewHTTPRequestTool(nil, nil, "", 0, 50*1024*1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"form_fields": map[string]string{
			"key": "value",
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for form_fields without files")
	}
	if !strings.Contains(err.Error(), "form_fields requires files") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartCustomSizeLimit(t *testing.T) {
	// Proves that a custom 1KB size limit is enforced and a 2KB file is rejected with an "exceeds" error.
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "small.bin")
	os.WriteFile(filePath, make([]byte, 2*1024), 0644) // 2KB file

	// With a 1KB limit, this should fail
	tool := NewHTTPRequestTool(nil, nil, "", 0, 1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    "http://example.com",
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "doc", "file_path": filePath},
		},
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for file exceeding custom limit")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v", err)
	}
}

func TestHTTPRequestMultipartCustomSizeLimitAllows(t *testing.T) {
	// Proves that a generous 100MB size limit allows a small file to upload successfully without error.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "small.bin")
	os.WriteFile(filePath, []byte("data"), 0644)

	tool := NewHTTPRequestTool(nil, nil, "", 0, 100*1024*1024, nil, 0640)
	params, _ := json.Marshal(map[string]interface{}{
		"url":    srv.URL,
		"method": "POST",
		"files": []map[string]string{
			{"field_name": "doc", "file_path": filePath},
		},
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result.Text, "HTTP 200") {
		t.Errorf("expected HTTP 200: %s", result.Text)
	}
}

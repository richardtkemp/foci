package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("line one\nline two\nline three\n"), 0644)

	tool := NewReadTool()
	params, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "line one") {
		t.Errorf("missing content in result: %q", result)
	}
	// Should have line numbers
	if !strings.Contains(result, "   1\t") {
		t.Errorf("missing line numbers in result: %q", result)
	}
}

func TestReadFileMissing(t *testing.T) {
	tool := NewReadTool()
	params, _ := json.Marshal(map[string]string{"path": "/nonexistent/file.txt"})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.txt")

	tool := NewWriteTool()
	params, _ := json.Marshal(map[string]interface{}{
		"path":    path,
		"content": "hello world",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Wrote") {
		t.Errorf("result = %q", result)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "hello world" {
		t.Errorf("file content = %q", string(data))
	}
}

func TestWriteFileOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	os.WriteFile(path, []byte("old content"), 0644)

	tool := NewWriteTool()
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
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	os.WriteFile(path, []byte("hello world, hello"), 0644)

	tool := NewEditTool()

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
	if !strings.Contains(result, "Edited") {
		t.Errorf("result = %q", result)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "goodbye world, hello" {
		t.Errorf("file content = %q", string(data))
	}
}

func TestEditFileNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	os.WriteFile(path, []byte("foo bar baz"), 0644)

	tool := NewEditTool()
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "nonexistent string",
		"new_string": "replacement",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for not-found string")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestEditFileNonUnique(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	os.WriteFile(path, []byte("aaa bbb aaa"), 0644)

	tool := NewEditTool()
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "aaa",
		"new_string": "ccc",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for non-unique string")
	}
	if !strings.Contains(err.Error(), "2 times") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestEditFileMissing(t *testing.T) {
	tool := NewEditTool()
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
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	os.WriteFile(path, []byte(`{"key": "old"}`), 0644)

	tool := NewEditTool()
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "old",
		"new_string": "new",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("valid→valid edit should succeed: %v", err)
	}
	if !strings.Contains(result, "Edited") {
		t.Errorf("result = %q", result)
	}

	data, _ := os.ReadFile(path)
	if string(data) != `{"key": "new"}` {
		t.Errorf("file = %q", string(data))
	}
}

func TestEditFileSyntaxValidToInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	os.WriteFile(path, []byte(`{"key": "value"}`), 0644)

	tool := NewEditTool()
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
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	os.WriteFile(path, []byte(`{"key": "value"`), 0644)  // missing closing brace

	tool := NewEditTool()
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": `"value"`,
		"new_string": `"value"}`,  // fixes the JSON
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("invalid→valid edit should succeed: %v", err)
	}
	if !strings.Contains(result, "Warning") || !strings.Contains(result, "syntax errors") {
		t.Errorf("expected warning about pre-existing syntax errors, got: %q", result)
	}
}

func TestEditFileSyntaxInvalidToInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	os.WriteFile(path, []byte(`{"key": bad}`), 0644)  // already invalid

	tool := NewEditTool()
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "bad",
		"new_string": "worse",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("invalid→invalid edit should proceed: %v", err)
	}
	if !strings.Contains(result, "Warning") {
		t.Errorf("expected warning, got: %q", result)
	}
}

func TestEditFileNoSyntaxCheckForUnknownExt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello"), 0644)

	tool := NewEditTool()
	params, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"old_string": "hello",
		"new_string": "world",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("txt edit should succeed: %v", err)
	}
	if strings.Contains(result, "Warning") {
		t.Errorf("no warning expected for .txt: %q", result)
	}
}

func TestReadLargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")

	var sb strings.Builder
	for i := 0; i < 2500; i++ {
		sb.WriteString("line content here\n")
	}
	os.WriteFile(path, []byte(sb.String()), 0644)

	tool := NewReadTool()
	params, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "truncated") {
		t.Error("expected truncation notice for large file")
	}
}

package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// #3077: a body written to disk by http_request (save_to, or the auto-save of
// a binary response) gets the same echo protection as the inline result. Text
// is scrubbed. Binary data cannot be scrubbed without corrupting it, so a
// binary body that holds a secret value is refused and no file is written.

const saveRedactSecret = "sk-save-redact-secret-value"

func saveEchoServer(t *testing.T, contentType string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPRequestSaveToScrubsText(t *testing.T) {
	t.Parallel()
	body := "token=" + saveRedactSecret + "\nagain " + saveRedactSecret + "\n"
	want := "token=[REDACTED]\nagain [REDACTED]\n"
	srv := saveEchoServer(t, "text/plain", []byte(body))
	tool := spillScrubTool(t, saveRedactSecret)

	t.Run("text", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "saved.txt")
		params, _ := json.Marshal(map[string]any{"url": srv.URL, "save_to": dest})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read saved file: %v", err)
		}
		if string(got) != want {
			t.Errorf("saved file = %q, want %q", got, want)
		}
		if !strings.Contains(result.Text, "Saved "+strconv.Itoa(len(want))+" bytes") {
			t.Errorf("result %q does not report the bytes written", result.Text)
		}
	})
	t.Run("json", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "saved.txt")
		params, _ := json.Marshal(map[string]any{"url": srv.URL, "save_to": dest})
		result, err := tool.Execute(WithOutputHints(context.Background(), OutputHints{Format: OutputFormatJSON}), params)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		var out struct {
			SavedBytes int `json:"saved_bytes"`
		}
		if err := json.Unmarshal([]byte(result.Text), &out); err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, _ := os.ReadFile(dest)
		if string(got) != want || out.SavedBytes != len(want) {
			t.Errorf("saved %q (%d bytes reported), want %q", got, out.SavedBytes, want)
		}
	})
	// Control: the inline path already scrubbed before #3077.
	t.Run("inline control", func(t *testing.T) {
		params, _ := json.Marshal(map[string]any{"url": srv.URL})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.Text != want {
			t.Errorf("inline = %q, want %q", result.Text, want)
		}
	})
}

func TestHTTPRequestSaveFromJSONPathScrubsExtractedText(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(map[string]any{"data": map[string]any{"key": "k=" + saveRedactSecret}})
	srv := saveEchoServer(t, "application/json", body)
	tool := spillScrubTool(t, saveRedactSecret)
	dest := filepath.Join(t.TempDir(), "out.txt")
	params, _ := json.Marshal(map[string]any{"url": srv.URL, "save_to": dest, "save_from_json_path": "data.key"})
	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != "k=[REDACTED]" {
		t.Errorf("saved file = %q, want %q", got, "k=[REDACTED]")
	}
}

// binaryWith returns bytes that are not valid UTF-8, optionally holding s.
func binaryWith(s string) []byte {
	b := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe, 0x00}
	b = append(b, s...)
	return append(b, 0x00, 0xc3, 0x28, 0xff)
}

func TestHTTPRequestBinarySaveKeptExact(t *testing.T) {
	t.Parallel()
	body := binaryWith("no secret here")
	srv := saveEchoServer(t, "image/png", body)
	tool := spillScrubTool(t, saveRedactSecret)

	t.Run("save_to", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "img.png")
		params, _ := json.Marshal(map[string]any{"url": srv.URL, "save_to": dest})
		if _, err := tool.Execute(context.Background(), params); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		got, _ := os.ReadFile(dest)
		if string(got) != string(body) {
			t.Errorf("saved %x, want the exact body %x", got, body)
		}
	})
	t.Run("auto-save", func(t *testing.T) {
		params, _ := json.Marshal(map[string]any{"url": srv.URL})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		i := strings.LastIndex(result.Text, " to ")
		if i < 0 {
			t.Fatalf("no saved path in %q", result.Text)
		}
		path := strings.TrimSpace(result.Text[i+4:])
		t.Cleanup(func() { _ = os.Remove(path) })
		got, _ := os.ReadFile(path)
		if string(got) != string(body) {
			t.Errorf("auto-saved %x, want the exact body %x", got, body)
		}
	})
}

func TestHTTPRequestBinarySaveWithSecretRefused(t *testing.T) {
	t.Parallel()
	body := binaryWith(saveRedactSecret)

	t.Run("save_to", func(t *testing.T) {
		srv := saveEchoServer(t, "image/png", body)
		tool := spillScrubTool(t, saveRedactSecret)
		dest := filepath.Join(t.TempDir(), "img.png")
		params, _ := json.Marshal(map[string]any{"url": srv.URL, "save_to": dest})
		_, err := tool.Execute(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "secret") {
			t.Fatalf("Execute err = %v, want a refusal naming the secret match", err)
		}
		if strings.Contains(err.Error(), saveRedactSecret) {
			t.Errorf("the refusal holds the secret value: %v", err)
		}
		if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
			t.Errorf("save_to file exists after the refusal (stat err %v)", serr)
		}
	})
	t.Run("auto-save", func(t *testing.T) {
		srv := saveEchoServer(t, "application/octet-stream", body)
		tmp := t.TempDir()
		store := writeTestSecrets(t, "[custom]\napi_key = \""+saveRedactSecret+"\"\nallowed_hosts = [\"127.0.0.1\"]\n")
		tool := NewHTTPRequestTool(store, nil, tmp, func() int { return 0 }, func() int64 { return 50 << 20 }, func() int64 { return 4 << 20 }, func() int64 { return 0 }, nil, 0640)
		params, _ := json.Marshal(map[string]any{"url": srv.URL})
		_, err := tool.Execute(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "secret") {
			t.Fatalf("Execute err = %v, want a refusal naming the secret match", err)
		}
		left, _ := filepath.Glob(filepath.Join(tmp, "http-*"))
		if len(left) != 0 {
			t.Errorf("auto-save left files behind: %v", left)
		}
	})
	t.Run("decoded data URI", func(t *testing.T) {
		uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(body)
		js, _ := json.Marshal(map[string]any{"image": uri})
		srv := saveEchoServer(t, "application/json", js)
		tool := spillScrubTool(t, saveRedactSecret)
		dest := filepath.Join(t.TempDir(), "img.png")
		params, _ := json.Marshal(map[string]any{"url": srv.URL, "save_to": dest, "save_from_json_path": "image"})
		_, err := tool.Execute(context.Background(), params)
		if err == nil || !strings.Contains(err.Error(), "secret") {
			t.Fatalf("Execute err = %v, want a refusal naming the secret match", err)
		}
		if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
			t.Errorf("save_to file exists after the refusal (stat err %v)", serr)
		}
	})
}

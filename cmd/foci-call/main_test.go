package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary builds the foci-call binary for testing.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "foci-call")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Dir(mustAbs(t, "main.go"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build foci-call: %v\n%s", err, out)
	}
	return bin
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// startTestServer creates a unix socket that responds to one request.
func startTestServer(t *testing.T, handler func(req string) string) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64*1024)
		n, _ := conn.Read(buf)
		resp := handler(strings.TrimSpace(string(buf[:n])))
		fmt.Fprintf(conn, "%s\n", resp)
	}()

	return sockPath
}

func TestFociCallSuccess(t *testing.T) {
	bin := buildBinary(t)
	sockPath := startTestServer(t, func(req string) string {
		resp, _ := json.Marshal(map[string]string{"result": "hello world"})
		return string(resp)
	})

	cmd := exec.Command(bin, `{"tool":"test","params":{}}`)
	cmd.Env = append(os.Environ(), "FOCI_SOCK="+sockPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("foci-call failed: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "hello world" {
		t.Errorf("output = %q, want %q", string(out), "hello world")
	}
}

func TestFociCallError(t *testing.T) {
	bin := buildBinary(t)
	sockPath := startTestServer(t, func(req string) string {
		resp, _ := json.Marshal(map[string]string{"error": "something failed"})
		return string(resp)
	})

	cmd := exec.Command(bin, `{"tool":"test","params":{}}`)
	cmd.Env = append(os.Environ(), "FOCI_SOCK="+sockPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code")
	}
	if !strings.Contains(string(out), "something failed") {
		t.Errorf("output = %q, want error message", string(out))
	}
}

func TestFociCallNoSocket(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, `{"tool":"test","params":{}}`)
	cmd.Env = []string{} // no FOCI_SOCK
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code")
	}
	if !strings.Contains(string(out), "FOCI_SOCK") {
		t.Errorf("output = %q, want FOCI_SOCK error", string(out))
	}
}

func TestFociCallNoArgs(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "FOCI_SOCK=/tmp/test.sock")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code")
	}
	if !strings.Contains(string(out), "usage") {
		t.Errorf("output = %q, want usage message", string(out))
	}
}

func TestFociCallInvalidJSON(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, `{not valid}`)
	cmd.Env = append(os.Environ(), "FOCI_SOCK=/tmp/test.sock")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit code")
	}
	if !strings.Contains(string(out), "invalid JSON") {
		t.Errorf("output = %q, want invalid JSON error", string(out))
	}
}

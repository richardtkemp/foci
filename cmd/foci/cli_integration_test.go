package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCLIIntegration verifies all major CLI commands work end-to-end against a mock gateway.
// It tests async vs sync modes, flag parsing, and error handling for various command combinations.
func TestCLIIntegration(t *testing.T) {
	server := mockGateway()
	defer server.Close()

	// Set the address to point at our mock
	addr := strings.TrimPrefix(server.URL, "http://")

	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		// Default mode is async — returns "queued"
		{"send", []string{"send", "hello"}, "queued", false},
		{"branch", []string{"branch"}, "queued", false},
		{"wake alias", []string{"wake"}, "queued", false},

		// --sync returns full response
		{"send --sync", []string{"send", "--sync", "hello"}, "echo: hello", false},
		{"send --wait", []string{"send", "--wait", "hello"}, "echo: hello", false},
		{"branch --sync", []string{"branch", "--sync"}, "wake ok", false},
		{"branch --wait", []string{"branch", "--wait"}, "wake ok", false},

		// Explicit --async
		{"send --async", []string{"send", "--async", "hello"}, "queued", false},
		{"send --no-wait", []string{"send", "--no-wait", "hello"}, "queued", false},
		{"branch --async", []string{"branch", "--async"}, "queued", false},
		{"branch --no-wait", []string{"branch", "--no-wait"}, "queued", false},

		{"status", []string{"status"}, "status: idle", false},
		{"ping", []string{"ping"}, "pong", false},
		{"command", []string{"command", "/ping"}, "pong", false},
		// eval is always sync
		{"eval", []string{"eval", "ls -la"}, "echo: Run this command", false},

		// -a flag (space-separated) — use --sync to get content-based responses
		{"send with -a", []string{"send", "--sync", "-a", "research", "hello"}, "[research] echo: hello", false},
		{"branch with -a", []string{"branch", "--sync", "-a", "research"}, "[research] wake ok", false},
		{"wake alias with -a", []string{"wake", "--sync", "-a", "research"}, "[research] wake ok", false},
		{"branch with -a and text", []string{"branch", "--sync", "-a", "research", "check news"}, "[research] wake ok", false},
		{"wake alias with -a and text", []string{"wake", "--sync", "-a", "research", "check news"}, "[research] wake ok", false},
		{"status with -a", []string{"status", "-a", "research"}, "[research] status: idle", false},
		{"eval with -a", []string{"eval", "-a", "research", "ls"}, "[research] echo: Run this command", false},
		{"command with -a", []string{"command", "-a", "research", "/ping"}, "[research] pong", false},
		{"ping with -a", []string{"ping", "-a", "research"}, "[research] pong", false},

		// --agent flag (space-separated)
		{"send with --agent", []string{"send", "--sync", "--agent", "main", "hello"}, "[main] echo: hello", false},

		// --agent=value form
		{"send with --agent=val", []string{"send", "--sync", "--agent=scout", "hello"}, "[scout] echo: hello", false},

		// -a=value form
		{"send with -a=val", []string{"send", "--sync", "-a=scout", "hello"}, "[scout] echo: hello", false},

		// -s/--session flag
		{"send with -s", []string{"send", "--sync", "-s", "research", "hello"}, "(session:research) echo: hello", false},
		{"send with --session", []string{"send", "--sync", "--session", "feature1", "hello"}, "(session:feature1) echo: hello", false},
		{"send with -s=value", []string{"send", "--sync", "-s=branch1", "hello"}, "(session:branch1) echo: hello", false},
		{"send with --session=value", []string{"send", "--sync", "--session=testing", "hello"}, "(session:testing) echo: hello", false},

		// -a and -s flags together
		{"send with -a and -s", []string{"send", "--sync", "-a", "clutch", "-s", "research", "hello"}, "(session:research) [clutch] echo: hello", false},
		{"send with -s and -a", []string{"send", "--sync", "-s", "feature", "-a", "clutch", "hello"}, "(session:feature) [clutch] echo: hello", false},
		{"send with -a= and -s=", []string{"send", "--sync", "-a=clutch", "-s=main", "hello"}, "(session:main) [clutch] echo: hello", false},

		// Flag after positional args
		{"send flag after text", []string{"send", "--sync", "hello", "-a", "research"}, "[research] echo: hello", false},

		// --no-compact flag for branch
		{"branch with --no-compact", []string{"branch", "--sync", "--no-compact"}, "wake ok (no_compact)", false},
		{"branch with --no-compact and text", []string{"branch", "--sync", "--no-compact", "morning check"}, "wake ok (no_compact)", false},
		{"branch with -a and --no-compact", []string{"branch", "--sync", "-a", "research", "--no-compact"}, "[research] wake ok (no_compact)", false},

		// --if-active flag for send
		{"send with --if-active", []string{"send", "--sync", "--if-active", "8h", "hello"}, "(if_active:8h) echo: hello", false},
		{"send with --if-active=", []string{"send", "--sync", "--if-active=30m", "hello"}, "(if_active:30m) echo: hello", false},
		{"send with -a and --if-active", []string{"send", "--sync", "-a", "clutch", "--if-active", "4h", "hello"}, "(if_active:4h) [clutch] echo: hello", false},

		// --if-active flag for branch
		{"branch with --if-active", []string{"branch", "--sync", "--if-active", "12h", "do work"}, "(if_active:12h) wake ok", false},
		{"branch with --if-active=", []string{"branch", "--sync", "--if-active=6h"}, "(if_active:6h) wake ok", false},
		{"branch with -a and --if-active", []string{"branch", "--sync", "-a", "research", "--if-active", "8h"}, "(if_active:8h) [research] wake ok", false},
		{"branch with --if-active and --no-compact", []string{"branch", "--sync", "--if-active", "8h", "--no-compact"}, "(if_active:8h) wake ok (no_compact)", false},

		// --if-inactive flag for send
		{"send with --if-inactive", []string{"send", "--sync", "--if-inactive", "30m", "hello"}, "(if_inactive:30m) echo: hello", false},
		{"send with --if-inactive=", []string{"send", "--sync", "--if-inactive=1h", "hello"}, "(if_inactive:1h) echo: hello", false},

		// --if-inactive flag for branch
		{"branch with --if-inactive", []string{"branch", "--sync", "--if-inactive", "30m", "keepalive"}, "(if_inactive:30m) wake ok", false},
		{"branch with --if-inactive=", []string{"branch", "--sync", "--if-inactive=45m"}, "(if_inactive:45m) wake ok", false},
		{"branch with --if-inactive and --oneshot", []string{"branch", "--sync", "--if-inactive", "30m", "--oneshot", "check emails"}, "(if_inactive:30m) wake ok (no_compact)", false},

		// Error cases: unknown agent returns HTTP 400, exit non-zero
		{"send unknown agent", []string{"send", "-a", "nonexistent", "hello"}, "unknown agent", true},
		{"branch unknown agent", []string{"branch", "-a", "nonexistent"}, "unknown agent", true},
	}

	// Build the CLI binary once
	binPath := t.TempDir() + "/foci"
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(binPath, tt.args...)
			cmd.Env = append(os.Environ(), "CLOD_ADDR="+addr)

			out, err := cmd.CombinedOutput()
			output := strings.TrimSpace(string(out))

			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v\noutput: %s", err, output)
			}
			if !strings.Contains(output, tt.want) {
				t.Errorf("output %q does not contain %q", output, tt.want)
			}
		})
	}
}

// TestCLIMessageFile verifies -mt and -mf flags work correctly, including file reading and conflicts.
func TestCLIMessageFile(t *testing.T) {
	server := mockGateway()
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	// Create temp file with message contents
	msgFile := t.TempDir() + "/msg.md"
	os.WriteFile(msgFile, []byte("hello from file"), 0644)

	// Build the CLI binary
	binPath := t.TempDir() + "/foci"
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"send -mf", []string{"send", "--sync", "-mf", msgFile}, "echo: hello from file", false},
		{"send --message-file", []string{"send", "--sync", "--message-file", msgFile}, "echo: hello from file", false},
		{"send -mf=value", []string{"send", "--sync", "-mf=" + msgFile}, "echo: hello from file", false},
		{"send -mf with -a", []string{"send", "--sync", "-a", "research", "-mf", msgFile}, "[research] echo: hello from file", false},
		{"branch -mf", []string{"branch", "--sync", "-mf", msgFile}, "wake ok", false},
		{"branch --oneshot -mf", []string{"branch", "--sync", "--oneshot", "-mf", msgFile}, "wake ok (no_compact)", false},

		// Error: both -mt and -mf
		{"send -mt and -mf", []string{"send", "-mt", "text", "-mf", msgFile}, "cannot specify both", true},
		// Error: missing file
		{"send -mf missing", []string{"send", "-mf", "/nonexistent/file.md"}, "reading message file", true},
		// Error: branch -mt and -mf
		{"branch -mt and -mf", []string{"branch", "-mt", "text", "-mf", msgFile}, "cannot specify both", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(binPath, tt.args...)
			cmd.Env = append(os.Environ(), "CLOD_ADDR="+addr)

			out, err := cmd.CombinedOutput()
			output := strings.TrimSpace(string(out))

			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v\noutput: %s", err, output)
			}
			if !strings.Contains(output, tt.want) {
				t.Errorf("output %q does not contain %q", output, tt.want)
			}
		})
	}
}

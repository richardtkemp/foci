package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMultiballCommand(t *testing.T) {
	// Verifies multiball fork invokes the callback and returns session info.
	forked := false
	cmd := NewMultiballCommand(func(ctx context.Context) (string, error) {
		forked = true
		return "Forked to @testbot (session: agent:main:multiball:mb-1)", nil
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !forked {
		t.Error("fork function not called")
	}
	if !strings.Contains(result, "@testbot") {
		t.Errorf("expected bot name in result, got %q", result)
	}
}

func TestMultiballCommandError(t *testing.T) {
	// Verifies error handling when fork fails.
	cmd := NewMultiballCommand(func(ctx context.Context) (string, error) {
		return "", fmt.Errorf("no secondary bots configured")
	})

	_, err := cmd.Execute(context.Background(), "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no secondary bots") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestManaCommand(t *testing.T) {
	// Verifies mana/custom resource command displays status correctly.
	tests := []struct {
		name       string
		cmdName    string
		manaFn     func(context.Context) (string, error)
		wantResult string
	}{
		{
			name:    "default mana name",
			cmdName: "mana",
			manaFn: func(ctx context.Context) (string, error) {
				return "mana: 75% remaining", nil
			},
			wantResult: "mana: 75% remaining",
		},
		{
			name:    "custom name juice",
			cmdName: "juice",
			manaFn: func(ctx context.Context) (string, error) {
				return "juice: 50% remaining", nil
			},
			wantResult: "juice: 50% remaining",
		},
		{
			name:    "custom name credits",
			cmdName: "credits",
			manaFn: func(ctx context.Context) (string, error) {
				return "credits: 10% remaining", nil
			},
			wantResult: "credits: 10% remaining",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := NewManaCommand(tt.cmdName, tt.manaFn)
			if cmd.Name != tt.cmdName {
				t.Errorf("cmd.Name = %q, want %q", cmd.Name, tt.cmdName)
			}
			result, err := cmd.Execute(context.Background(), "")
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result != tt.wantResult {
				t.Errorf("result = %q, want %q", result, tt.wantResult)
			}
		})
	}
}

func TestManaCommandDescription(t *testing.T) {
	// Verifies description includes the command name.
	cmd := NewManaCommand("juice", func(ctx context.Context) (string, error) {
		return "", nil
	})
	if !strings.Contains(cmd.Description, "juice") {
		t.Errorf("Description should contain 'juice', got %q", cmd.Description)
	}
}

func TestCompactCommand(t *testing.T) {
	// Verifies compact session operation.
	cmd := NewCompactCommand(func(ctx context.Context, dryRun bool) (int, error) {
		if dryRun {
			t.Error("expected dryRun=false for normal compact")
		}
		return 42, nil
	})

	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "42 messages") {
		t.Errorf("expected message count in result: %q", result)
	}
	if cmd.Category != "operations" {
		t.Errorf("category = %q, want operations", cmd.Category)
	}
}

func TestCompactCommandDryRun(t *testing.T) {
	// Verifies dry-run compact operation.
	cmd := NewCompactCommand(func(ctx context.Context, dryRun bool) (int, error) {
		if !dryRun {
			t.Error("expected dryRun=true for dry-run compact")
		}
		return 42, nil
	})

	result, err := cmd.Execute(context.Background(), "dry-run")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "Dry-run") {
		t.Errorf("expected dry-run message in result: %q", result)
	}
	if !strings.Contains(result, "42 messages") {
		t.Errorf("expected message count in result: %q", result)
	}
}

func TestCompactCommandError(t *testing.T) {
	// Verifies error handling when compact fails.
	cmd := NewCompactCommand(func(ctx context.Context, dryRun bool) (int, error) {
		return 0, fmt.Errorf("too few messages to compact (3)")
	})

	_, err := cmd.Execute(context.Background(), "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "too few") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestScriptCommand(t *testing.T) {
	// Verifies script command executes and captures output.
	cmd := NewScriptCommand("test", "test cmd", "echo hello from script", 10)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "hello from script" {
		t.Errorf("result = %q", result)
	}
}

func TestScriptCommandFailure(t *testing.T) {
	// Verifies script command captures stderr and exit code.
	cmd := NewScriptCommand("fail", "failing cmd", "echo oops >&2; exit 1", 10)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !strings.Contains(result, "oops") {
		t.Errorf("missing stderr in: %q", result)
	}
	if !strings.Contains(result, "Error:") {
		t.Errorf("missing Error in: %q", result)
	}
}

func TestScriptCommandTimeout(t *testing.T) {
	// Verifies script command times out correctly.
	cmd := NewScriptCommand("slow", "slow cmd", "sleep 60", 1)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !strings.Contains(result, "timed out") {
		t.Errorf("missing timeout message in: %q", result)
	}
}

func TestScriptCommandDefaultTimeout(t *testing.T) {
	// Verifies default timeout is applied when zero is passed.
	// Verify default timeout is applied (not 0)
	cmd := NewScriptCommand("test", "test", "echo ok", 0)
	result, _ := cmd.Execute(context.Background(), "")
	if result != "ok" {
		t.Errorf("result = %q", result)
	}
}

func TestLogCommand(t *testing.T) {
	// Verifies log command displays last N lines in code block.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0644)

	cmd := NewLogCommand(logPath)

	// Default: last 20, wrapped in code block
	result, _ := cmd.Execute(context.Background(), "")
	if !strings.HasPrefix(result, "```\n") || !strings.HasSuffix(result, "\n```") {
		t.Errorf("result not wrapped in code block:\n%s", result)
	}
	// Strip code block markers and check content
	inner := strings.TrimSuffix(strings.TrimPrefix(result, "```\n"), "\n```")
	resultLines := strings.Split(inner, "\n")
	if len(resultLines) != 20 {
		t.Errorf("got %d lines, want 20", len(resultLines))
	}
	if resultLines[0] != "line 10" {
		t.Errorf("first line = %q, want 'line 10'", resultLines[0])
	}

	// Custom: last 5
	result, _ = cmd.Execute(context.Background(), "5")
	inner = strings.TrimSuffix(strings.TrimPrefix(result, "```\n"), "\n```")
	resultLines = strings.Split(inner, "\n")
	if len(resultLines) != 5 {
		t.Errorf("got %d lines, want 5", len(resultLines))
	}
}

func TestErrorsCommand(t *testing.T) {
	// Verifies errors command filters by log level field, not message content.
	// INFO lines containing "ERROR" or "WARN" in their message body must NOT be included.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	content := strings.Join([]string{
		"2026-03-01T00:00:00Z INFO  [test] normal",
		"2026-03-01T00:00:01Z ERROR [test] bad thing",
		"2026-03-01T00:00:02Z INFO  [test] got ERROR response from API",
		"2026-03-01T00:00:03Z WARN  [test] warning",
		"2026-03-01T00:00:04Z INFO  [test] WARN string in message body",
		"2026-03-01T00:00:05Z INFO  [test] fine",
	}, "\n") + "\n"
	os.WriteFile(logPath, []byte(content), 0644)

	cmd := NewErrorsCommand(logPath)
	result, _ := cmd.Execute(context.Background(), "")

	if !strings.HasPrefix(result, "```\n") || !strings.HasSuffix(result, "\n```") {
		t.Errorf("result not wrapped in code block:\n%s", result)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(result, "```\n"), "\n```")
	lines := strings.Split(inner, "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (ERROR + WARN only):\n%s", len(lines), result)
	}
	if !strings.Contains(lines[0], "bad thing") {
		t.Errorf("line 0 should be the ERROR line: %q", lines[0])
	}
	if !strings.Contains(lines[1], "warning") {
		t.Errorf("line 1 should be the WARN line: %q", lines[1])
	}
}

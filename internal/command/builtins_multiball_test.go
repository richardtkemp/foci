package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMultiballCommand verifies multiball fork invokes the callback and returns session info.
func TestMultiballCommand(t *testing.T) {
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

// TestMultiballCommandError verifies error handling when fork fails.
func TestMultiballCommandError(t *testing.T) {
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

// TestManaCommand verifies mana/custom resource command displays status correctly.
func TestManaCommand(t *testing.T) {
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

// TestManaCommandDescription verifies description includes the command name.
func TestManaCommandDescription(t *testing.T) {
	cmd := NewManaCommand("juice", func(ctx context.Context) (string, error) {
		return "", nil
	})
	if !strings.Contains(cmd.Description, "juice") {
		t.Errorf("Description should contain 'juice', got %q", cmd.Description)
	}
}

// TestCompactCommand verifies compact session operation.
func TestCompactCommand(t *testing.T) {
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

// TestCompactCommandDryRun verifies dry-run compact operation.
func TestCompactCommandDryRun(t *testing.T) {
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

// TestCompactCommandError verifies error handling when compact fails.
func TestCompactCommandError(t *testing.T) {
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

// TestScriptCommand verifies script command executes and captures output.
func TestScriptCommand(t *testing.T) {
	cmd := NewScriptCommand("test", "test cmd", "echo hello from script", 10)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "hello from script" {
		t.Errorf("result = %q", result)
	}
}

// TestScriptCommandFailure verifies script command captures stderr and exit code.
func TestScriptCommandFailure(t *testing.T) {
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

// TestScriptCommandTimeout verifies script command times out correctly.
func TestScriptCommandTimeout(t *testing.T) {
	cmd := NewScriptCommand("slow", "slow cmd", "sleep 60", 1)
	result, err := cmd.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !strings.Contains(result, "timed out") {
		t.Errorf("missing timeout message in: %q", result)
	}
}

// TestScriptCommandDefaultTimeout verifies default timeout is applied when zero is passed.
func TestScriptCommandDefaultTimeout(t *testing.T) {
	// Verify default timeout is applied (not 0)
	cmd := NewScriptCommand("test", "test", "echo ok", 0)
	result, _ := cmd.Execute(context.Background(), "")
	if result != "ok" {
		t.Errorf("result = %q", result)
	}
}

// TestLogCommand verifies log command displays last N lines in code block.
func TestLogCommand(t *testing.T) {
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

// TestErrorsCommand verifies errors command filters and displays ERROR and WARN lines.
func TestErrorsCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	content := "2026-03-01T00:00:00Z INFO  [test] normal\n2026-03-01T00:00:01Z ERROR [test] bad thing\n2026-03-01T00:00:02Z INFO  [test] ok\n2026-03-01T00:00:03Z WARN  [test] warning\n2026-03-01T00:00:04Z INFO  [test] fine\n"
	os.WriteFile(logPath, []byte(content), 0644)

	cmd := NewErrorsCommand(logPath)
	result, _ := cmd.Execute(context.Background(), "")

	if !strings.HasPrefix(result, "```\n") || !strings.HasSuffix(result, "\n```") {
		t.Errorf("result not wrapped in code block:\n%s", result)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(result, "```\n"), "\n```")
	lines := strings.Split(inner, "\n")
	if len(lines) != 2 {
		t.Errorf("got %d lines, want 2:\n%s", len(lines), result)
	}
	if !strings.Contains(lines[0], "ERROR") {
		t.Errorf("line 0 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "WARN") {
		t.Errorf("line 1 = %q", lines[1])
	}
}

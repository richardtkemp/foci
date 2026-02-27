package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"foci/command"
)

func TestCommandWrapperNoArgs(t *testing.T) {
	cmd := &command.Command{
		Name:        "status",
		Description: "Show status",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "all good", nil
		},
	}

	tool := CreateCommandWrapperTool(cmd)

	if tool.Name != "status" {
		t.Errorf("tool.Name = %q, want %q", tool.Name, "status")
	}
	if !strings.Contains(tool.Description, "Show status") {
		t.Errorf("tool.Description = %q", tool.Description)
	}
	if !strings.Contains(tool.Description, "slash command") {
		t.Errorf("tool.Description missing 'slash command': %q", tool.Description)
	}

	params, _ := json.Marshal(map[string]interface{}{})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "all good" {
		t.Errorf("result = %q, want %q", result, "all good")
	}
}

func TestCommandWrapperWithArgs(t *testing.T) {
	var receivedArgs string
	cmd := &command.Command{
		Name:        "greet",
		Description: "Greet someone",
		Execute: func(ctx context.Context, args string) (string, error) {
			receivedArgs = args
			return "Hello, " + args, nil
		},
	}

	tool := CreateCommandWrapperTool(cmd)

	params, _ := json.Marshal(map[string]interface{}{
		"args": "World",
	})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedArgs != "World" {
		t.Errorf("receivedArgs = %q, want %q", receivedArgs, "World")
	}
	if result != "Hello, World" {
		t.Errorf("result = %q", result)
	}
}

func TestCommandWrapperError(t *testing.T) {
	cmd := &command.Command{
		Name:        "fail",
		Description: "Always fails",
		Execute: func(ctx context.Context, args string) (string, error) {
			return "", fmt.Errorf("something broke")
		},
	}

	tool := CreateCommandWrapperTool(cmd)

	params, _ := json.Marshal(map[string]interface{}{})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "command failed") {
		t.Errorf("error = %q, want 'command failed'", err.Error())
	}
	if !strings.Contains(err.Error(), "something broke") {
		t.Errorf("error = %q, want 'something broke'", err.Error())
	}
}

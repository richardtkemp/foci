package agent

import (
	"context"
	"strings"
	"testing"

	"foci/anthropic"
	"foci/session"
	"foci/tools"
	"foci/workspace"
)

func TestEnvironmentBlockPrepended(t *testing.T) {
	var receivedReq *anthropic.MessageRequest
	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	envText := "# Environment\n\nYou are running on **foci**.\n\n## Workspace\n- Workspace: /home/test\n- Agent ID: tester\n"

	ag := &Agent{
		Client:           client,
		Sessions:         store,
		Tools:            registry,
		Bootstrap:        bootstrap,
		Model:            "claude-haiku-4-5",
		EnvironmentBlock: envText,
	}

	_, err := ag.HandleMessage(context.Background(), "agent:test:env", "Hi")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if receivedReq == nil {
		t.Fatal("no API request captured")
	}

	blocks := receivedReq.System
	if len(blocks) == 0 {
		t.Fatal("no system blocks in request")
	}

	first := blocks[0]
	if first.Type != "text" {
		t.Errorf("first block type = %q, want text", first.Type)
	}
	if !strings.Contains(first.Text, "# Environment") {
		t.Errorf("first block should contain environment header, got: %s", first.Text[:min(len(first.Text), 100)])
	}
	if !strings.Contains(first.Text, "Workspace: /home/test") {
		t.Errorf("first block should contain workspace path")
	}
	if !strings.Contains(first.Text, "Agent ID: tester") {
		t.Errorf("first block should contain agent ID")
	}
}

func TestEnvironmentBlockOmittedWhenEmpty(t *testing.T) {
	var receivedReq *anthropic.MessageRequest
	server := mockServer(func(req *anthropic.MessageRequest) *anthropic.MessageResponse {
		receivedReq = req
		return &anthropic.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    anthropic.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
	defer server.Close()

	client := newTestClientWithBase(server.URL, "test-token")
	store := session.NewStore(t.TempDir())
	registry := tools.NewRegistry()
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})

	ag := &Agent{
		Client:           client,
		Sessions:         store,
		Tools:            registry,
		Bootstrap:        bootstrap,
		Model:            "claude-haiku-4-5",
		EnvironmentBlock: "", // disabled — simulates environment.enabled = false
	}

	_, err := ag.HandleMessage(context.Background(), "agent:test:noenv", "Hi")
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if receivedReq == nil {
		t.Fatal("no API request captured")
	}

	for _, block := range receivedReq.System {
		if strings.Contains(block.Text, "# Environment") {
			t.Error("environment block should not be present when EnvironmentBlock is empty")
		}
	}
}

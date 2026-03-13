package agent

import (
	"context"
	"strings"
	"testing"

	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestEnvironmentBlockPrepended(t *testing.T) {
	// Proves that when EnvironmentBlock is set, it is prepended as the first system block in every API request, including workspace path and agent ID.
	var receivedReq *provider.MessageRequest
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
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

	_, err := ag.HandleMessage(context.Background(), "test/ienv/1000000000", "Hi")
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
	// Proves that when EnvironmentBlock is empty (environment disabled), no environment system block is added to API requests.
	var receivedReq *provider.MessageRequest
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}
	})
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

	_, err := ag.HandleMessage(context.Background(), "test/inoenv/1000000000", "Hi")
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

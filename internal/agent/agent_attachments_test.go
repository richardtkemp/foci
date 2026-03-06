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

func TestHandleMessageWithAttachments(t *testing.T) {
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("I see a cat!"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	images := []Attachment{
		{MediaType: "image/jpeg", Data: []byte("fake-jpeg-data")},
	}
	resp, err := ag.HandleMessageWithAttachments(context.Background(), "test/iimg/1000000000", "What is this?", images)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "I see a cat!" {
		t.Errorf("response = %q", resp)
	}

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// Check the user message has image + text blocks
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	if len(userMsg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(userMsg.Content))
	}

	// First block should be image
	if userMsg.Content[0].Type != "image" {
		t.Errorf("content[0].Type = %q, want image", userMsg.Content[0].Type)
	}
	if userMsg.Content[0].Source == nil {
		t.Fatal("content[0].Source is nil")
	}
	if userMsg.Content[0].Source.MediaType != "image/jpeg" {
		t.Errorf("content[0].Source.MediaType = %q", userMsg.Content[0].Source.MediaType)
	}

	// Second block should be text with metadata + user text
	if userMsg.Content[1].Type != "text" {
		t.Errorf("content[1].Type = %q, want text", userMsg.Content[1].Type)
	}
	if !strings.Contains(userMsg.Content[1].Text, "What is this?") {
		t.Errorf("content[1].Text missing user text: %q", userMsg.Content[1].Text)
	}
	if !strings.Contains(userMsg.Content[1].Text, "[meta]") {
		t.Errorf("content[1].Text missing [meta]: %q", userMsg.Content[1].Text)
	}
}

func TestHandleMessageWithPDFAttachment(t *testing.T) {
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("I read the PDF."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	attachments := []Attachment{
		{MediaType: "application/pdf", Data: []byte("%PDF-1.4 fake")},
	}
	resp, err := ag.HandleMessageWithAttachments(context.Background(), "test/ipdf/1000000000", "Read this PDF", attachments)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "I read the PDF." {
		t.Errorf("response = %q", resp)
	}

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// Check the user message has document + text blocks
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	if len(userMsg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(userMsg.Content))
	}

	// First block should be document (not image)
	if userMsg.Content[0].Type != "document" {
		t.Errorf("content[0].Type = %q, want document", userMsg.Content[0].Type)
	}
	if userMsg.Content[0].Source == nil {
		t.Fatal("content[0].Source is nil")
	}
	if userMsg.Content[0].Source.MediaType != "application/pdf" {
		t.Errorf("content[0].Source.MediaType = %q", userMsg.Content[0].Source.MediaType)
	}
}

func TestHandleMessageWithPDFSavedPath(t *testing.T) {
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Got it!"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	attachments := []Attachment{
		{MediaType: "application/pdf", Data: []byte("%PDF-1.4"), SavedPath: "/tmp/docs/report.pdf"},
	}
	_, err := ag.HandleMessageWithAttachments(context.Background(), "test/ipdfsaved/1000000000", "Check this", attachments)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}

	// Check the text block contains PDF-specific saved path annotation
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	textBlock := userMsg.Content[len(userMsg.Content)-1]
	if !strings.Contains(textBlock.Text, "[PDF saved to: /tmp/docs/report.pdf]") {
		t.Errorf("text block should have PDF saved path annotation, got: %q", textBlock.Text)
	}
}

func TestHandleMessageWithAttachmentsNoText(t *testing.T) {
	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("I see an image."),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	images := []Attachment{
		{MediaType: "image/png", Data: []byte("fake-png-data")},
	}
	// Empty text — image only
	resp, err := ag.HandleMessageWithAttachments(context.Background(), "test/iimgonly/1000000000", "", images)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "I see an image." {
		t.Errorf("response = %q", resp)
	}
}

func TestHandleMessageDelegatesToWithImages(t *testing.T) {
	// Verify HandleMessage (text-only) still works correctly
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
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	ag.HandleMessage(context.Background(), "test/idelegate/1000000000", "Hello")

	// Text-only message should have exactly 1 content block (text)
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	if len(userMsg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(userMsg.Content))
	}
	if userMsg.Content[0].Type != "text" {
		t.Errorf("content[0].Type = %q, want text", userMsg.Content[0].Type)
	}
}

func TestHandleMessageWithAttachmentsSavedPath(t *testing.T) {
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("Got it!"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	images := []Attachment{
		{MediaType: "image/jpeg", Data: []byte("fake"), SavedPath: "/tmp/images/test.jpg"},
	}
	resp, err := ag.HandleMessageWithAttachments(context.Background(), "test/isavepath/1000000000", "What is this?", images)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "Got it!" {
		t.Errorf("response = %q", resp)
	}

	// Check the text block contains the saved path annotation
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	textBlock := userMsg.Content[len(userMsg.Content)-1]
	if !strings.Contains(textBlock.Text, "[Image saved to: /tmp/images/test.jpg]") {
		t.Errorf("text block missing saved path annotation: %q", textBlock.Text)
	}
	if !strings.Contains(textBlock.Text, "What is this?") {
		t.Errorf("text block missing user text: %q", textBlock.Text)
	}
}

func TestHandleMessageWithAttachmentsNoSavedPath(t *testing.T) {
	var receivedReq *provider.MessageRequest

	client := newTestClient(func(req *provider.MessageRequest) *provider.MessageResponse {
		receivedReq = req
		return &provider.MessageResponse{
			ID:         "msg_test",
			Type:       "message",
			Role:       "assistant",
			Content:    provider.TextContent("ok"),
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 10},
		}
	})
	store := session.NewStore(t.TempDir())
	bootstrap := workspace.NewBootstrap(t.TempDir(), []string{})
	ag := &Agent{
		Client:    client,
		Sessions:  store,
		Tools:     tools.NewRegistry(),
		Bootstrap: bootstrap,
		Model:     "claude-haiku-4-5",
	}

	images := []Attachment{
		{MediaType: "image/jpeg", Data: []byte("fake")},
	}
	ag.HandleMessageWithAttachments(context.Background(), "test/inosaved/1000000000", "Look", images)

	// Text block should NOT contain [Image saved to:] when SavedPath is empty
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	textBlock := userMsg.Content[len(userMsg.Content)-1]
	if strings.Contains(textBlock.Text, "[Image saved to:") {
		t.Errorf("text block should not have saved path annotation when SavedPath is empty: %q", textBlock.Text)
	}
}



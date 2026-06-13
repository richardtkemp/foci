package agent

import (
	"context"
	"strings"
	"testing"

	"foci/internal/platform"
	"foci/internal/provider"
	"foci/internal/session"
	"foci/internal/tools"
	"foci/internal/workspace"
)

func TestHandleMessageWithAttachments(t *testing.T) {
	// Proves that image attachments are sent as an image content block followed by
	// a text block containing the user's message and a [meta] prefix.
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

	images := []platform.Attachment{
		{MimeType: "image/jpeg", Data: []byte("fake-jpeg-data")},
	}
	resp, err := ag.hmTestAttachments(context.Background(), "test/iimg/1000000000", []string{"What is this?"}, images)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "I see a cat!" {
		t.Errorf("response = %q", resp)
	}

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// Check the user message has image block, meta block, and user text block
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]

	// First block should be image
	if userMsg.Content[0].Type != "image" {
		t.Errorf("content[0].Type = %q, want image", userMsg.Content[0].Type)
	}
	if userMsg.Content[0].Source == nil {
		t.Fatal("content[0].Source is nil")
	}
	if userMsg.Content[0].Source.MimeType != "image/jpeg" {
		t.Errorf("content[0].Source.MimeType = %q", userMsg.Content[0].Source.MimeType)
	}

	// Should have a meta block and a user text block among the text blocks
	var hasMeta, hasUserText bool
	for _, b := range userMsg.Content {
		if b.Type == "text" && strings.Contains(b.Text, "[meta]") {
			hasMeta = true
		}
		if b.Type == "text" && strings.Contains(b.Text, "What is this?") {
			hasUserText = true
		}
	}
	if !hasMeta {
		t.Error("missing [meta] text block")
	}
	if !hasUserText {
		t.Error("missing user text block")
	}
}

func TestHandleMessageWithPDFAttachment(t *testing.T) {
	// Proves that PDF attachments use a "document" content block rather than "image",
	// ensuring the correct block type is sent to the API for PDF MIME types.
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

	attachments := []platform.Attachment{
		{MimeType: "application/pdf", Data: []byte("%PDF-1.4 fake")},
	}
	resp, err := ag.hmTestAttachments(context.Background(), "test/ipdf/1000000000", []string{"Read this PDF"}, attachments)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "I read the PDF." {
		t.Errorf("response = %q", resp)
	}

	if receivedReq == nil {
		t.Fatal("no request received")
	}

	// Check the user message has document block
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]

	// First block should be document (not image)
	if userMsg.Content[0].Type != "document" {
		t.Errorf("content[0].Type = %q, want document", userMsg.Content[0].Type)
	}
	if userMsg.Content[0].Source == nil {
		t.Fatal("content[0].Source is nil")
	}
	if userMsg.Content[0].Source.MimeType != "application/pdf" {
		t.Errorf("content[0].Source.MimeType = %q", userMsg.Content[0].Source.MimeType)
	}
}

func TestHandleMessageWithPDFSavedPath(t *testing.T) {
	// Proves that when a PDF attachment has a SavedPath, the text block includes
	// a "[PDF saved to: ...]" annotation so the model knows where the file is stored.
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

	attachments := []platform.Attachment{
		{MimeType: "application/pdf", Data: []byte("%PDF-1.4"), SavedPath: "/tmp/docs/report.pdf"},
	}
	_, err := ag.hmTestAttachments(context.Background(), "test/ipdfsaved/1000000000", []string{"Check this"}, attachments)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}

	// Check that a text block contains PDF-specific saved path annotation
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	var found bool
	for _, b := range userMsg.Content {
		if b.Type == "text" && strings.Contains(b.Text, "[PDF saved to: /tmp/docs/report.pdf]") {
			found = true
		}
	}
	if !found {
		t.Error("no text block with PDF saved path annotation")
	}
}

func TestHandleMessageWithAttachmentsNoText(t *testing.T) {
	// Proves that an image-only message (empty user text) succeeds and returns the
	// assistant's response, confirming no panic or error when text is absent.
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

	images := []platform.Attachment{
		{MimeType: "image/png", Data: []byte("fake-png-data")},
	}
	// Empty text — image only
	resp, err := ag.hmTestAttachments(context.Background(), "test/iimgonly/1000000000", []string{""}, images)
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

	ag.hmTest(context.Background(), "test/idelegate/1000000000", "Hello")

	// Text-only message should have meta block + user text block
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	if len(userMsg.Content) < 2 {
		t.Fatalf("expected at least 2 content blocks, got %d", len(userMsg.Content))
	}
	// All blocks should be text
	for i, b := range userMsg.Content {
		if b.Type != "text" {
			t.Errorf("content[%d].Type = %q, want text", i, b.Type)
		}
	}
}

func TestHandleMessageWithAttachmentsSavedPath(t *testing.T) {
	// Proves that when an image attachment has a SavedPath, the text block includes
	// an "[Image saved to: ...]" annotation alongside the user's message text.
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

	images := []platform.Attachment{
		{MimeType: "image/jpeg", Data: []byte("fake"), SavedPath: "/tmp/images/test.jpg"},
	}
	resp, err := ag.hmTestAttachments(context.Background(), "test/isavepath/1000000000", []string{"What is this?"}, images)
	if err != nil {
		t.Fatalf("HandleMessageWithAttachments: %v", err)
	}
	if resp != "Got it!" {
		t.Errorf("response = %q", resp)
	}

	// Check for saved path annotation and user text in separate blocks
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	var hasSavedPath, hasUserText bool
	for _, b := range userMsg.Content {
		if b.Type == "text" && strings.Contains(b.Text, "[Image saved to: /tmp/images/test.jpg]") {
			hasSavedPath = true
		}
		if b.Type == "text" && strings.Contains(b.Text, "What is this?") {
			hasUserText = true
		}
	}
	if !hasSavedPath {
		t.Error("missing saved path annotation block")
	}
	if !hasUserText {
		t.Error("missing user text block")
	}
}

func TestHandleMessageWithAttachmentsNoSavedPath(t *testing.T) {
	// Proves that when an image attachment has no SavedPath, the text block does
	// NOT contain any "[Image saved to:]" annotation, keeping the message clean.
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

	images := []platform.Attachment{
		{MimeType: "image/jpeg", Data: []byte("fake")},
	}
	ag.hmTestAttachments(context.Background(), "test/inosaved/1000000000", []string{"Look"}, images)

	// No block should contain [Image saved to:] when SavedPath is empty
	userMsg := receivedReq.Messages[len(receivedReq.Messages)-1]
	for i, b := range userMsg.Content {
		if b.Type == "text" && strings.Contains(b.Text, "[Image saved to:") {
			t.Errorf("content[%d] should not have saved path annotation when SavedPath is empty: %q", i, b.Text)
		}
	}
}

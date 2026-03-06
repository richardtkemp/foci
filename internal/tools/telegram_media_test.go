package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestSendTelegramSendAsVideo verifies that send_as=video routes to SendVideo.
func TestSendTelegramSendAsVideo(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/clip.mp4",
		"send_as":   "video",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: video" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.videoCalls) != 1 || mock.videoCalls[0] != "/tmp/clip.mp4" {
		t.Errorf("videoCalls = %v", mock.videoCalls)
	}
	if len(mock.documentCalls) != 0 {
		t.Errorf("documentCalls should be empty, got %v", mock.documentCalls)
	}
}

// TestSendTelegramSendAsVoice verifies that send_as=voice routes to SendVoice for files.
func TestSendTelegramSendAsVoice(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/note.ogg",
		"send_as":   "voice",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: voice note" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.voiceCalls) != 1 {
		t.Errorf("voiceCalls = %v", mock.voiceCalls)
	}
}

// TestSendTelegramSendAsDocument verifies that send_as=document routes to SendDocument.
func TestSendTelegramSendAsDocument(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/report.pdf",
		"send_as":   "document",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: document" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.documentCalls) != 1 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

// TestSendTelegramSendAsDefaultIsDocument verifies that files default to document when send_as is omitted.
func TestSendTelegramSendAsDefaultIsDocument(t *testing.T) {
	// No send_as — should default to document
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/file.bin",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: document" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.documentCalls) != 1 {
		t.Errorf("documentCalls = %v", mock.documentCalls)
	}
}

// TestSendTelegramVideoError verifies that send errors for video are propagated.
func TestSendTelegramVideoError(t *testing.T) {
	mock := &mockTelegramSender{videoErr: fmt.Errorf("video too large")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/big.mp4",
		"send_as":   "video",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "video too large") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestSendTelegramVideoChatRouting verifies that videos are routed to chat-targeted method.
func TestSendTelegramVideoChatRouting(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/clip.mp4",
		"send_as":   "video",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.chatVideoCalls) != 1 {
		t.Fatalf("expected 1 chatVideoCall, got %d", len(mock.chatVideoCalls))
	}
	if mock.chatVideoCalls[0].chatID != 12345 {
		t.Errorf("chatID = %d, want 12345", mock.chatVideoCalls[0].chatID)
	}
	if len(mock.videoCalls) != 0 {
		t.Errorf("default SendVideo should not be called")
	}
}

// TestSendTelegramTextAndVideo verifies that text and video can be sent together.
func TestSendTelegramTextAndVideo(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"text":      "check this out",
		"file_path": "/tmp/clip.mp4",
		"send_as":   "video",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: text + video" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.textCalls) != 1 {
		t.Errorf("textCalls = %v", mock.textCalls)
	}
	if len(mock.videoCalls) != 1 {
		t.Errorf("videoCalls = %v", mock.videoCalls)
	}
}

// TestSendTelegramSendAsPhoto verifies that send_as=photo routes to SendPhoto.
func TestSendTelegramSendAsPhoto(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/image.jpg",
		"send_as":   "photo",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: photo" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.photoCalls) != 1 || mock.photoCalls[0] != "/tmp/image.jpg" {
		t.Errorf("photoCalls = %v", mock.photoCalls)
	}
}

// TestSendTelegramPhotoError verifies that send errors for photos are propagated.
func TestSendTelegramPhotoError(t *testing.T) {
	mock := &mockTelegramSender{photoErr: fmt.Errorf("image too large")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/huge.jpg",
		"send_as":   "photo",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "image too large") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestSendTelegramPhotoChatRouting verifies that photos are routed to chat-targeted method.
func TestSendTelegramPhotoChatRouting(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/image.jpg",
		"send_as":   "photo",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.chatPhotoCalls) != 1 || mock.chatPhotoCalls[0].chatID != 12345 {
		t.Errorf("chatPhotoCalls = %v", mock.chatPhotoCalls)
	}
	if len(mock.photoCalls) != 0 {
		t.Errorf("default SendPhoto should not be called")
	}
}

// TestSendTelegramSendAsAudio verifies that send_as=audio routes to SendAudio.
func TestSendTelegramSendAsAudio(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/song.mp3",
		"send_as":   "audio",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: audio" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.audioCalls) != 1 || mock.audioCalls[0] != "/tmp/song.mp3" {
		t.Errorf("audioCalls = %v", mock.audioCalls)
	}
}

// TestSendTelegramAudioError verifies that send errors for audio are propagated.
func TestSendTelegramAudioError(t *testing.T) {
	mock := &mockTelegramSender{audioErr: fmt.Errorf("bad format")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/bad.mp3",
		"send_as":   "audio",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad format") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestSendTelegramAudioChatRouting verifies that audio is routed to chat-targeted method.
func TestSendTelegramAudioChatRouting(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/song.mp3",
		"send_as":   "audio",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.chatAudioCalls) != 1 || mock.chatAudioCalls[0].chatID != 12345 {
		t.Errorf("chatAudioCalls = %v", mock.chatAudioCalls)
	}
	if len(mock.audioCalls) != 0 {
		t.Errorf("default SendAudio should not be called")
	}
}

// TestSendTelegramSendAsAnimation verifies that send_as=animation routes to SendAnimation.
func TestSendTelegramSendAsAnimation(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/funny.gif",
		"send_as":   "animation",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Sent: animation" {
		t.Errorf("result = %q", result.Text)
	}
	if len(mock.animationCalls) != 1 || mock.animationCalls[0] != "/tmp/funny.gif" {
		t.Errorf("animationCalls = %v", mock.animationCalls)
	}
}

// TestSendTelegramAnimationError verifies that send errors for animations are propagated.
func TestSendTelegramAnimationError(t *testing.T) {
	mock := &mockTelegramSender{animationErr: fmt.Errorf("gif corrupted")}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/bad.gif",
		"send_as":   "animation",
	})

	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "gif corrupted") {
		t.Errorf("error = %q", err.Error())
	}
}

// TestSendTelegramAnimationChatRouting verifies that animations are routed to chat-targeted method.
func TestSendTelegramAnimationChatRouting(t *testing.T) {
	mock := &mockTelegramSender{}
	tool := NewSendTelegramTool(func(string) TelegramSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "agent:fotini:chat:12345")
	params, _ := json.Marshal(map[string]interface{}{
		"file_path": "/tmp/funny.gif",
		"send_as":   "animation",
	})

	_, err := tool.Execute(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.chatAnimationCalls) != 1 || mock.chatAnimationCalls[0].chatID != 12345 {
		t.Errorf("chatAnimationCalls = %v", mock.chatAnimationCalls)
	}
	if len(mock.animationCalls) != 0 {
		t.Errorf("default SendAnimation should not be called")
	}
}

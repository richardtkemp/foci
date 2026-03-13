package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSendMessageToUserSendAsVideo(t *testing.T) {
	// Verifies that send_as=video routes the file to SendVideo rather than SendDocument.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserSendAsVoice(t *testing.T) {
	// Verifies that send_as=voice with a file path routes to SendVoice rather than SendDocument.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserSendAsDocument(t *testing.T) {
	// Verifies that an explicit send_as=document sends the file as a document.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserSendAsDefaultIsDocument(t *testing.T) {
	// Verifies that omitting send_as defaults to sending the file as a document.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserVideoError(t *testing.T) {
	// Verifies that send errors from the video sender are propagated back to the caller.
	t.Parallel()
	mock := &mockMessageSender{videoErr: fmt.Errorf("video too large")}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserVideoChatRouting(t *testing.T) {
	// Verifies that when a chat ID is present in the session key, videos are dispatched via SendVideoToChat rather than the default SendVideo.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c12345/1000")
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

func TestSendMessageToUserTextAndVideo(t *testing.T) {
	// Verifies that providing both text and a video file sends both independently and reports the combined result.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserSendAsPhoto(t *testing.T) {
	// Verifies that send_as=photo routes the file to SendPhoto rather than SendDocument.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserPhotoError(t *testing.T) {
	// Verifies that send errors from the photo sender are propagated back to the caller.
	t.Parallel()
	mock := &mockMessageSender{photoErr: fmt.Errorf("image too large")}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserPhotoChatRouting(t *testing.T) {
	// Verifies that when a chat ID is present in the session key, photos are dispatched via SendPhotoToChat rather than the default SendPhoto.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c12345/1000")
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

func TestSendMessageToUserSendAsAudio(t *testing.T) {
	// Verifies that send_as=audio routes the file to SendAudio rather than SendDocument.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserAudioError(t *testing.T) {
	// Verifies that send errors from the audio sender are propagated back to the caller.
	t.Parallel()
	mock := &mockMessageSender{audioErr: fmt.Errorf("bad format")}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserAudioChatRouting(t *testing.T) {
	// Verifies that when a chat ID is present in the session key, audio is dispatched via SendAudioToChat rather than the default SendAudio.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c12345/1000")
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

func TestSendMessageToUserSendAsAnimation(t *testing.T) {
	// Verifies that send_as=animation routes the file to SendAnimation rather than SendDocument.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserAnimationError(t *testing.T) {
	// Verifies that send errors from the animation sender are propagated back to the caller.
	t.Parallel()
	mock := &mockMessageSender{animationErr: fmt.Errorf("gif corrupted")}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

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

func TestSendMessageToUserAnimationChatRouting(t *testing.T) {
	// Verifies that when a chat ID is present in the session key, animations are dispatched via SendAnimationToChat rather than the default SendAnimation.
	t.Parallel()
	mock := &mockMessageSender{}
	tool := NewSendMessageToUserTool(func(string) MessageSender { return mock }, nil)

	ctx := WithSessionKey(context.Background(), "fotini/c12345/1000")
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

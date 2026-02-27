package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"foci/secrets/bitwarden"
)

// testBWExecutor is a mock executor for tool tests.
type testBWExecutor struct {
	listJSON string
	getMap   map[string]string
	getErr   error
}

func (m *testBWExecutor) Run(args ...string) (string, error) {
	if len(args) >= 1 && args[0] == "list" {
		return m.listJSON, nil
	}
	if len(args) >= 3 && args[0] == "get" && args[1] == "password" {
		if m.getErr != nil {
			return "", m.getErr
		}
		id := args[2]
		if val, ok := m.getMap[id]; ok {
			return val, nil
		}
		return "", fmt.Errorf("not found: %s", id)
	}
	return "", fmt.Errorf("unknown command: %v", args)
}

const testBWListJSON = `[
	{"id":"aaaa-1111","name":"GitHub API","folderId":"folder-dev","login":{"username":"bot-user","uris":[{"uri":"https://api.github.com"}]}},
	{"id":"bbbb-2222","name":"Slack Bot","folderId":"folder-ops","login":{"username":"slack-app","uris":[{"uri":"https://slack.com/api"}]}}
]`

func newTestBWStore(t *testing.T, mock *testBWExecutor) *bitwarden.Store {
	t.Helper()
	s := bitwarden.New(mock, 30*time.Minute)
	if err := s.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return s
}

func TestBitwardenSearchTool(t *testing.T) {
	mock := &testBWExecutor{listJSON: testBWListJSON}
	store := newTestBWStore(t, mock)
	tool := NewBitwardenSearchTool(store)

	params, _ := json.Marshal(map[string]string{"query": "github"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "GitHub API") {
		t.Errorf("result should contain item name: %s", result)
	}
	if !strings.Contains(result, "aaaa-1111") {
		t.Errorf("result should contain item ID: %s", result)
	}
	if !strings.Contains(result, "api.github.com") {
		t.Errorf("result should contain URI: %s", result)
	}
}

func TestBitwardenSearchEmpty(t *testing.T) {
	mock := &testBWExecutor{listJSON: testBWListJSON}
	store := newTestBWStore(t, mock)
	tool := NewBitwardenSearchTool(store)

	params, _ := json.Marshal(map[string]string{"query": "nonexistent-xyz"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "No matching") {
		t.Errorf("expected 'No matching' message: %s", result)
	}
}

func TestBitwardenUnlockTool(t *testing.T) {
	mock := &testBWExecutor{
		listJSON: testBWListJSON,
		getMap:   map[string]string{"aaaa-1111": "ghp_supersecret123"},
	}
	store := newTestBWStore(t, mock)
	tool := NewBitwardenUnlockTool(store)

	params, _ := json.Marshal(map[string]string{"id": "aaaa-1111"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(result, "Unlocked") {
		t.Errorf("result should confirm unlock: %s", result)
	}
	if !strings.Contains(result, "{{secret:bw.aaaa-1111}}") {
		t.Errorf("result should show template syntax: %s", result)
	}
}

func TestBitwardenUnlockDenied(t *testing.T) {
	mock := &testBWExecutor{
		listJSON: testBWListJSON,
		getErr:   fmt.Errorf("denied by aisudo"),
	}
	store := newTestBWStore(t, mock)
	tool := NewBitwardenUnlockTool(store)

	params, _ := json.Marshal(map[string]string{"id": "aaaa-1111"})
	_, err := tool.Execute(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for denied unlock")
	}
	if !strings.Contains(err.Error(), "denied by administrator") {
		t.Errorf("error should mention denial: %v", err)
	}
}

func TestBitwardenUnlockNeverReturnsValue(t *testing.T) {
	mock := &testBWExecutor{
		listJSON: testBWListJSON,
		getMap:   map[string]string{"aaaa-1111": "ghp_supersecret123"},
	}
	store := newTestBWStore(t, mock)
	tool := NewBitwardenUnlockTool(store)

	params, _ := json.Marshal(map[string]string{"id": "aaaa-1111"})
	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if strings.Contains(result, "ghp_supersecret123") {
		t.Error("tool result MUST NOT contain the actual secret value")
	}
}

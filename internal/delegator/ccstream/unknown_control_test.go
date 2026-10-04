package ccstream

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// #2168: a control_request subtype foci does not handle must reach the handler
// with its request_id, not be dropped in the reader.
func TestReaderUnknownControlRequestDispatched(t *testing.T) {
	t.Parallel()

	line := `{"type":"control_request","request_id":"req-9","request":{"subtype":"request_user_dialog","dialog_kind":"x"}}` + "\n"
	h := &mockHandler{}
	NewReader(strings.NewReader(line), h).Run(context.Background())

	if len(h.unknownCtl) != 1 || h.unknownCtl[0] != "req-9/request_user_dialog" {
		t.Fatalf("unknownCtl = %v, want [req-9/request_user_dialog]", h.unknownCtl)
	}
	if len(h.permissions) != 0 || len(h.elicitations) != 0 {
		t.Errorf("unknown subtype misrouted: permissions=%d elicitations=%d", len(h.permissions), len(h.elicitations))
	}
}

// #2168: the backend answers an unknown control_request with an error
// control_response for the same request_id, so CC's pending request fails
// instead of blocking the turn.
func TestBackendRefusesUnknownControlRequest(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	b := newTestBackend(&buf)
	line := `{"type":"control_request","request_id":"req-9","request":{"subtype":"some_future_subtype"}}`
	NewReader(strings.NewReader(""), b).dispatch([]byte(line))

	var got struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Error     string `json:"error"`
		} `json:"response"`
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("no control_response written for unknown control_request")
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if got.Type != "control_response" || got.Response.Subtype != "error" || got.Response.RequestID != "req-9" {
		t.Errorf("got %+v, want control_response/error for req-9", got)
	}
	if !strings.Contains(got.Response.Error, "some_future_subtype") {
		t.Errorf("error %q does not name the subtype", got.Response.Error)
	}
}

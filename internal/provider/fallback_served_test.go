package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The Served report on a response says which tuple served it, so callers can
// pair their post-request work (gate release, ledger booking) with the
// endpoint that actually answered. These tests pin the three report rules:
// own-client hop legs, reused-client empty legs, and chains naming the hop
// that succeeded.

func TestSend_ServedReportFallbackOwnClient(t *testing.T) {
	t.Parallel()
	// Proves a hop that answered through its own client (from
	// clientProvider.GetClient) reports its canonical model AND its own
	// endpoint and format.
	primaryClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fbClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("fb ok")}},
	}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{
		"fb-ep:fb-fmt": fbClient,
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary-model" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}
	req := &MessageRequest{Model: "primary-model"}
	resp, err := Send(context.Background(), primaryClient, req, nil, fallbackFn, cp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ServedReport{Model: "fb-model", Endpoint: "fb-ep", Format: "fb-fmt"}
	if resp.Served != want {
		t.Errorf("Served = %+v, want %+v", resp.Served, want)
	}
}

func TestSend_ServedReportGetClientNil(t *testing.T) {
	t.Parallel()
	// Proves a hop that reuses the caller's client (GetClient returned nil
	// for the pair) reports the hop model with EMPTY legs — the caller's
	// endpoint and format served.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
		{resp: &MessageResponse{Content: TextContent("fb ok on caller client")}},
	}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary-model" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}
	req := &MessageRequest{Model: "primary-model"}
	resp, err := Send(context.Background(), mc, req, nil, fallbackFn, cp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ServedReport{Model: "fb-model"}
	if resp.Served != want {
		t.Errorf("Served = %+v, want %+v (hop model, no legs)", resp.Served, want)
	}
}

func TestSend_ServedReportNilClientProvider(t *testing.T) {
	t.Parallel()
	// Proves a nil clientProvider is the same reused-client rule: the hop
	// went through the caller's client, so the report carries the hop model
	// and no legs.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
		{resp: &MessageResponse{Content: TextContent("fb ok on caller client")}},
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary-model" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}
	req := &MessageRequest{Model: "primary-model"}
	resp, err := Send(context.Background(), mc, req, nil, fallbackFn, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ServedReport{Model: "fb-model"}
	if resp.Served != want {
		t.Errorf("Served = %+v, want %+v (hop model, no legs)", resp.Served, want)
	}
}

func TestSend_ServedReportNamesSucceedingHop(t *testing.T) {
	t.Parallel()
	// Proves a chain's report names the hop that SUCCEEDED, not an earlier
	// one: hop 1 (own client) fails with 503, hop 2 (own client) answers.
	primaryClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fb1 := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 503}},
	}}
	fb2 := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("fb2 ok")}},
	}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{
		"ep1:fmt1": fb1,
		"ep2:fmt2": fb2,
	}}
	fallbackFn := func(model string) (string, string, string, bool) {
		switch model {
		case "primary-model":
			return "fb1-model", "ep1", "fmt1", true
		case "fb1-model":
			return "fb2-model", "ep2", "fmt2", true
		}
		return "", "", "", false
	}
	req := &MessageRequest{Model: "primary-model"}
	resp, err := Send(context.Background(), primaryClient, req, nil, fallbackFn, cp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ServedReport{Model: "fb2-model", Endpoint: "ep2", Format: "fmt2"}
	if resp.Served != want {
		t.Errorf("Served = %+v, want %+v (the succeeding hop)", resp.Served, want)
	}
}

func TestSend_ServedReportPrimaryServed(t *testing.T) {
	t.Parallel()
	// Characterisation: a response the primary served keeps the ZERO report
	// (empty means primary), and the report never serialises.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("primary ok")}},
	}}
	req := &MessageRequest{Model: "primary"}
	resp, err := Send(context.Background(), mc, req, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Served != (ServedReport{}) {
		t.Errorf("Served = %+v, want the zero report (primary served)", resp.Served)
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if strings.Contains(string(b), "Served") || strings.Contains(string(b), "served") {
		t.Errorf("serialised response mentions the report: %s", b)
	}
}

func TestSend_ServedReportStripRetryServed(t *testing.T) {
	t.Parallel()
	// Characterisation: the 400 strip-and-retry stays on the primary, so
	// its success keeps the zero report.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: http.StatusBadRequest, Body: `{"error":"thinking is not supported"}`}},
		{resp: &MessageResponse{Content: TextContent("ok after strip")}},
	}}
	req := &MessageRequest{
		Model:    "primary",
		Thinking: &ThinkingConfig{Type: "enabled", BudgetTokens: 1024},
	}
	resp, err := Send(context.Background(), mc, req, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Served != (ServedReport{}) {
		t.Errorf("Served = %+v, want the zero report (strip retry stays primary)", resp.Served)
	}
}

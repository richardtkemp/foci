package provider

import (
	"context"
	"errors"
	"testing"
)

// A failing fallback hop's error arrives wrapped in *FailedHop carrying the
// hop's ServedReport, so a caller can pair its error-path work (rate-limit
// gate closure) with the tuple that FAILED — the hop's — while errors.As /
// errors.Is still reach the hop's own error. These tests pin that contract
// through Send, alongside the no-hop rule (FailedTuple → primary).

// TestSend_FailedHopWrapsHopError proves the wrapper's core contract: a
// primary 529 walked to a hop (own client) that answers 429 returns an error
// that unwraps to the hop's 429 *APIError, carries the hop's ServedReport,
// and resolves through FailedTuple to the hop's full tuple — model AND legs.
func TestSend_FailedHopWrapsHopError(t *testing.T) {
	t.Parallel()
	primaryClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fbClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 429}},
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
	_, err := Send(context.Background(), primaryClient, req, nil, fallbackFn, cp, nil)
	if err == nil {
		t.Fatal("expected the hop's 429, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 {
		t.Errorf("errors.As(*APIError) = %v, want the hop's 429", err)
	}
	var hop *FailedHop
	if !errors.As(err, &hop) {
		t.Fatalf("errors.As(*FailedHop) = false for %v, want the hop labelled", err)
	}
	if want := (ServedReport{Model: "fb-model", Endpoint: "fb-ep", Format: "fb-fmt"}); hop.Report != want {
		t.Errorf("FailedHop.Report = %+v, want %+v", hop.Report, want)
	}
	primary := ModelTuple{Model: "primary-model", Endpoint: "p-ep", Format: "p-fmt"}
	if got := FailedTuple(primary, err); got != (ModelTuple{Model: "fb-model", Endpoint: "fb-ep", Format: "fb-fmt"}) {
		t.Errorf("FailedTuple = %+v, want the hop tuple {fb-model fb-ep fb-fmt}", got)
	}
}

// TestSend_FailedHopReusedCallerClientLegsArePrimary proves the reused-client
// rule on the failure side: a hop that went through the CALLER's client
// (GetClient nil) reports empty legs, so FailedTuple fills them with the
// primary's — that client's endpoint answered, its gate is the one to close.
func TestSend_FailedHopReusedCallerClientLegsArePrimary(t *testing.T) {
	t.Parallel()
	shared := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}}, // primary attempt
		{err: &APIError{StatusCode: 429}}, // hop on the same client
	}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{}}
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary-model" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}
	req := &MessageRequest{Model: "primary-model"}
	_, err := Send(context.Background(), shared, req, nil, fallbackFn, cp, nil)
	if err == nil {
		t.Fatal("expected the hop's 429, got nil")
	}
	var hop *FailedHop
	if !errors.As(err, &hop) {
		t.Fatalf("errors.As(*FailedHop) = false for %v, want the hop labelled", err)
	}
	if want := (ServedReport{Model: "fb-model"}); hop.Report != want {
		t.Errorf("FailedHop.Report = %+v, want %+v (hop model, no legs)", hop.Report, want)
	}
	primary := ModelTuple{Model: "primary-model", Endpoint: "p-ep", Format: "p-fmt"}
	if got := FailedTuple(primary, err); got != (ModelTuple{Model: "fb-model", Endpoint: "p-ep", Format: "p-fmt"}) {
		t.Errorf("FailedTuple = %+v, want the hop model on the caller's legs {fb-model p-ep p-fmt}", got)
	}
}

// TestSend_FailedHopChainNamesFailingHop proves a chain's wrapper names the
// hop that FAILED, not an earlier one: hop 1 answers 529 (still eligible, so
// the walk continues to hop 2), hop 2 answers 429 — the returned error is
// hop 2's, and FailedTuple resolves to hop 2's tuple.
func TestSend_FailedHopChainNamesFailingHop(t *testing.T) {
	t.Parallel()
	primaryClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fb1 := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fb2 := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 429}},
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
	_, err := Send(context.Background(), primaryClient, req, nil, fallbackFn, cp, nil)
	if err == nil {
		t.Fatal("expected hop 2's 429, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 {
		t.Errorf("errors.As(*APIError) = %v, want hop 2's 429", err)
	}
	var hop *FailedHop
	if !errors.As(err, &hop) || hop.Report.Model != "fb2-model" {
		t.Fatalf("FailedHop = %+v, want the hop that failed (fb2-model), not an earlier one", err)
	}
	primary := ModelTuple{Model: "primary-model", Endpoint: "p-ep", Format: "p-fmt"}
	if got := FailedTuple(primary, err); got != (ModelTuple{Model: "fb2-model", Endpoint: "ep2", Format: "fmt2"}) {
		t.Errorf("FailedTuple = %+v, want hop 2's tuple {fb2-model ep2 fmt2}", got)
	}
}

// TestSend_FailedHopPreservesDeadlineExceeded proves errors.Is keeps working
// through the wrapper for non-API errors too: a hop failing with
// context.DeadlineExceeded (the last hop in the chain) still satisfies
// errors.Is(err, context.DeadlineExceeded) AND names its hop.
func TestSend_FailedHopPreservesDeadlineExceeded(t *testing.T) {
	t.Parallel()
	primaryClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	fbClient := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: context.DeadlineExceeded},
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
	_, err := Send(context.Background(), primaryClient, req, nil, fallbackFn, cp, nil)
	if err == nil {
		t.Fatal("expected the hop's DeadlineExceeded, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, DeadlineExceeded) = false for %v, want preserved through the wrapper", err)
	}
	primary := ModelTuple{Model: "primary-model", Endpoint: "p-ep", Format: "p-fmt"}
	if got := FailedTuple(primary, err); got != (ModelTuple{Model: "fb-model", Endpoint: "fb-ep", Format: "fb-fmt"}) {
		t.Errorf("FailedTuple = %+v, want the hop tuple {fb-model fb-ep fb-fmt}", got)
	}
}

// TestFailedTuple_NoHopIsPrimary proves the no-hop rule: the primary's own
// error — no fallback attempted (nil fallbackFn; or none configured for the
// model; or no error at all) — carries no hop, so FailedTuple returns the
// primary.
func TestFailedTuple_NoHopIsPrimary(t *testing.T) {
	t.Parallel()
	primary := ModelTuple{Model: "primary-model", Endpoint: "p-ep", Format: "p-fmt"}

	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 429}},
	}}
	req := &MessageRequest{Model: "primary-model"}
	if _, err := Send(context.Background(), mc, req, nil, nil, nil, nil); err == nil {
		t.Fatal("expected the primary's 429")
	} else if got := FailedTuple(primary, err); got != primary {
		t.Errorf("nil fallbackFn: FailedTuple = %+v, want the primary", got)
	}

	mc = &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
	}}
	noFallback := func(string) (string, string, string, bool) { return "", "", "", false }
	if _, err := Send(context.Background(), mc, req, nil, noFallback, nil, nil); err == nil {
		t.Fatal("expected the primary's 529 (original error path)")
	} else if got := FailedTuple(primary, err); got != primary {
		t.Errorf("no fallback configured: FailedTuple = %+v, want the primary", got)
	}

	if got := FailedTuple(primary, nil); got != primary {
		t.Errorf("nil error: FailedTuple = %+v, want the primary", got)
	}
}

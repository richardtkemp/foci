package provider

import (
	"context"
	"testing"
)

// #3035: Send stamps the endpoint whose registry handle served a response,
// so a caller that knew only the model and format (compaction, summaries)
// learns which [endpoints] entry served — a zero_cost one is priced at $0.

func namedHandle(name string, c Client) *SwappableClient {
	h := NewSwappableClient(c)
	h.SetEndpointName(name)
	return h
}

func TestSend_StampsPrimaryEndpoint(t *testing.T) {
	t.Parallel()
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("ok")}},
	}}
	resp, err := Send(context.Background(), namedHandle("local", mc), &MessageRequest{Model: "qwen"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Endpoint != "local" {
		t.Errorf("resp.Endpoint = %q, want local", resp.Endpoint)
	}
	got := ServedTuple(ModelTuple{Model: "qwen", Format: "openai"}, resp)
	if want := (ModelTuple{Model: "qwen", Endpoint: "local", Format: "openai"}); got != want {
		t.Errorf("ServedTuple = %+v, want %+v (the endpoint leg filled from the response)", got, want)
	}
	// A tuple that names its endpoint keeps it.
	if got := ServedTuple(ModelTuple{Model: "qwen", Endpoint: "named"}, resp); got.Endpoint != "named" {
		t.Errorf("ServedTuple endpoint = %q, want the caller's named one", got.Endpoint)
	}
}

func TestSend_UnnamedClientStampsNothing(t *testing.T) {
	t.Parallel()
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{resp: &MessageResponse{Content: TextContent("ok")}},
	}}
	resp, err := Send(context.Background(), mc, &MessageRequest{Model: "m"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Endpoint != "" {
		t.Errorf("resp.Endpoint = %q from a client that is not a named handle", resp.Endpoint)
	}
}

func TestSend_StampsFallbackHopEndpoint(t *testing.T) {
	t.Parallel()
	fallbackFn := func(model string) (string, string, string, bool) {
		if model == "primary-model" {
			return "fb-model", "fb-ep", "fb-fmt", true
		}
		return "", "", "", false
	}

	// A hop through its own client: the hop's endpoint served.
	primary := &fallbackMockClient{responses: []fallbackMockResponse{{err: &APIError{StatusCode: 529}}}}
	fb := &fallbackMockClient{responses: []fallbackMockResponse{{resp: &MessageResponse{Content: TextContent("fb")}}}}
	cp := &fallbackMockClientProvider{clients: map[string]Client{"fb-ep:fb-fmt": namedHandle("fb-ep", fb)}}
	resp, err := Send(context.Background(), namedHandle("primary-ep", primary), &MessageRequest{Model: "primary-model"}, nil, fallbackFn, cp, nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Endpoint != "fb-ep" {
		t.Errorf("own-client hop: resp.Endpoint = %q, want fb-ep", resp.Endpoint)
	}

	// A hop on the caller's client: the caller's endpoint served.
	mc := &fallbackMockClient{responses: []fallbackMockResponse{
		{err: &APIError{StatusCode: 529}},
		{resp: &MessageResponse{Content: TextContent("fb on caller")}},
	}}
	resp, err = Send(context.Background(), namedHandle("primary-ep", mc), &MessageRequest{Model: "primary-model"}, nil, fallbackFn,
		&fallbackMockClientProvider{clients: map[string]Client{}}, nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Endpoint != "primary-ep" {
		t.Errorf("reused-client hop: resp.Endpoint = %q, want primary-ep", resp.Endpoint)
	}
}

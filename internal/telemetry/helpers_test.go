package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// setupTest arms tracing with an in-memory exporter via the initWith test
// seam and registers a cleanup that tears everything down (resetForTest),
// so tests are independent of each other and of run order under repeated
// runs. Returns the exporter to read spans from after flush(t).
func setupTest(t *testing.T, o Options) *tracetest.InMemoryExporter {
	t.Helper()
	if o.MaxFieldBytes <= 0 {
		o.MaxFieldBytes = 1 << 20
	}
	if o.Environment == "" {
		o.Environment = "test"
	}
	exp := tracetest.NewInMemoryExporter()
	if err := initWith(exp, o); err != nil {
		t.Fatalf("initWith: %v", err)
	}
	t.Cleanup(resetForTest)
	return exp
}

// flush forces every buffered span out to the in-memory exporter.
func flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := flushForTest(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// findSpan returns the first span with the given name, or nil.
func findSpan(spans tracetest.SpanStubs, name string) *tracetest.SpanStub {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

// findSpans returns every span with the given name.
func findSpans(spans tracetest.SpanStubs, name string) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == name {
			out = append(out, spans[i])
		}
	}
	return out
}

// rootForTrace picks the span belonging to the given trace out of a set of
// same-named candidates (e.g. several "turn" roots from different turns in
// one test).
func rootForTrace(spans []tracetest.SpanStub, id trace.TraceID) *tracetest.SpanStub {
	for i := range spans {
		if spans[i].SpanContext.TraceID() == id {
			return &spans[i]
		}
	}
	return nil
}

// attrOf looks up one attribute by key.
func attrOf(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

func attrStr(t *testing.T, attrs []attribute.KeyValue, key string) string {
	t.Helper()
	v, ok := attrOf(attrs, key)
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	return v.AsString()
}

func attrBool(t *testing.T, attrs []attribute.KeyValue, key string) bool {
	t.Helper()
	v, ok := attrOf(attrs, key)
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	return v.AsBool()
}

func attrInt(t *testing.T, attrs []attribute.KeyValue, key string) int64 {
	t.Helper()
	v, ok := attrOf(attrs, key)
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	return v.AsInt64()
}

func attrFloat(t *testing.T, attrs []attribute.KeyValue, key string) float64 {
	t.Helper()
	v, ok := attrOf(attrs, key)
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	return v.AsFloat64()
}

func attrStrSlice(t *testing.T, attrs []attribute.KeyValue, key string) []string {
	t.Helper()
	v, ok := attrOf(attrs, key)
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	return v.AsStringSlice()
}

func hasAttr(attrs []attribute.KeyValue, key string) bool {
	_, ok := attrOf(attrs, key)
	return ok
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

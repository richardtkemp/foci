// Package telemetry exports every agent turn as an OpenTelemetry trace over
// OTLP/HTTP — in practice to a self-hosted Langfuse, whose OTel endpoint maps
// the langfuse.* span attributes set here onto its trace/observation model.
//
// It has two producers, deliberately chosen because every backend already
// converges on them (see docs/WIRING.md "Tracing"):
//
//   - the turnevent.Sink stream — the agent is the sole producer of the
//     ordered per-turn events (tool calls, subagent runs, text, thinking,
//     completion) for the API, claude-code, opencode and codex transports
//     alike. Wrapping the sink (NewTurnSink) yields the trace's SHAPE: one
//     root "turn" span, a "tool" child per tool call, an "agent" child per CC
//     subagent run.
//   - log.API / log.AccumulateSubagentRow — every api.db row passes through
//     log.APIHook, and each becomes exactly one "generation" observation
//     carrying that row's model, usage and calculated cost. Cost therefore
//     lives in exactly one place, and SUM(observation cost) over a day equals
//     SUM(calculated_cost_usd) over the same day by construction — which is
//     what scripts/langfuse-etl/etl.py reconcile checks.
//
// Identity is deterministic: every id is a SHA-256 of the api.db turn_id
// ("<session>@<StartedAt UnixNano>") plus a role. A subagent that reports its
// spend half an hour after its parent turn closed still lands in the SPAWNING
// turn's trace, and a cross-agent link can be computed by whoever holds the
// turn id without any shared in-memory state.
//
// Everything here is nil-safe and a no-op until Init succeeds; a disabled
// install pays one atomic load per turn.
package telemetry

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"foci/internal/log"
)

// Options configures Init. Values come from [tracing] in foci.toml plus the
// langfuse.public_key / langfuse.secret_key secrets.
type Options struct {
	// Endpoint is the OTLP/HTTP base URL; spans POST to Endpoint + "/v1/traces".
	Endpoint string
	// PublicKey and SecretKey form the HTTP basic-auth pair (Langfuse project keys).
	PublicKey, SecretKey string
	// Environment is stamped on every span as langfuse.environment.
	Environment string
	// Content attaches prompt/reply/thinking/tool text; false exports shape,
	// timing, usage and cost only.
	Content bool
	// SystemPrompt additionally exports the full system prompt text once per
	// session per distinct prompt hash (hash + length are always recorded).
	SystemPrompt bool
	// MaxFieldBytes caps any single exported text field.
	MaxFieldBytes int
	// FlushTimeout bounds Shutdown's wait for buffered spans.
	FlushTimeout time.Duration
	// ServiceVersion is the foci build version, for the resource.
	ServiceVersion string
	// SecretValues are redacted from every exported text field — the
	// gateway holds the secrets store, so it can scrub the VALUES directly
	// rather than hashing candidate substrings the way the backfill ETL must.
	SecretValues []string
}

var (
	enabled atomic.Bool

	mu             sync.RWMutex
	tracerProvider *sdktrace.TracerProvider
	tracer         trace.Tracer
	opts           Options
	redactor       *Redactor

	tlog = log.NewComponentLogger("telemetry")
)

const (
	serviceName  = "foci"
	tracerName   = "foci/internal/telemetry"
	exportPath   = "/v1/traces"
	batchTimeout = 2 * time.Second
	exportMax    = 256
	queueSize    = 4096
	httpTimeout  = 15 * time.Second
)

// Init builds the exporter and tracer provider and arms the api.db hooks.
// Returns an error (and stays disabled) on a malformed endpoint or missing
// keys; export failures at runtime are logged, never fatal.
func Init(ctx context.Context, o Options) error {
	if o.Endpoint == "" {
		return fmt.Errorf("tracing endpoint is empty")
	}
	if o.PublicKey == "" || o.SecretKey == "" {
		return fmt.Errorf("tracing needs both langfuse.public_key and langfuse.secret_key")
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("tracing endpoint %q is not an absolute http(s) URL", o.Endpoint)
	}
	if o.MaxFieldBytes <= 0 {
		o.MaxFieldBytes = 2 << 20
	}
	if o.FlushTimeout <= 0 {
		o.FlushTimeout = 5 * time.Second
	}
	if o.Environment == "" {
		o.Environment = "production"
	}

	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(o.PublicKey+":"+o.SecretKey))
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(strings.TrimRight(o.Endpoint, "/")+exportPath),
		otlptracehttp.WithHeaders(map[string]string{"Authorization": auth}),
		otlptracehttp.WithTimeout(httpTimeout),
	)
	if err != nil {
		return fmt.Errorf("otlp exporter: %w", err)
	}

	return initWith(exp, o)
}

// initWith builds the tracer provider around an already-constructed exporter
// and arms the api.db hooks — the part of Init that has nothing to do with
// OTLP/HTTP specifically, split out so tests can drive it with an in-memory
// exporter (go.opentelemetry.io/otel/sdk/trace/tracetest) instead of a real
// endpoint. o must already be validated and defaulted (Init does that before
// calling this; tests fill in Options directly).
func initWith(exp sdktrace.SpanExporter, o Options) error {
	host, _ := os.Hostname()
	res := resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.version", o.ServiceVersion),
		attribute.String("host.name", host),
		attribute.String("deployment.environment", o.Environment),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(idGenerator{}),
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(batchTimeout),
			sdktrace.WithMaxExportBatchSize(exportMax),
			sdktrace.WithMaxQueueSize(queueSize),
		),
	)
	// Exporter failures surface here (the batch processor swallows them
	// otherwise). Debounced: an outage would otherwise log once per batch.
	var lastErr atomic.Int64
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		now := time.Now().Unix()
		if now-lastErr.Load() < 60 {
			return
		}
		lastErr.Store(now)
		tlog.Warnf("export: %v (further export errors suppressed for 60s)", err)
	}))

	mu.Lock()
	tracerProvider = tp
	tracer = tp.Tracer(tracerName)
	opts = o
	redactor = NewRedactor(o.SecretValues)
	mu.Unlock()

	log.APIHook = recordEntry
	log.CorrectionHook = recordCorrection
	enabled.Store(true)
	tlog.Infof("tracing enabled → %s (environment=%s content=%v system_prompt=%v)",
		o.Endpoint, o.Environment, o.Content, o.SystemPrompt)
	return nil
}

// Enabled reports whether Init succeeded. Every entry point checks it first,
// so a disabled install never allocates a span.
func Enabled() bool { return enabled.Load() }

// Shutdown flushes buffered spans (bounded by FlushTimeout) and disables
// tracing. Safe to call when never initialised.
func Shutdown(ctx context.Context) {
	if !enabled.CompareAndSwap(true, false) {
		return
	}
	log.APIHook = nil
	log.CorrectionHook = nil
	mu.Lock()
	tp := tracerProvider
	timeout := opts.FlushTimeout
	tracerProvider = nil
	mu.Unlock()
	if tp == nil {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := tp.Shutdown(fctx); err != nil {
		tlog.Warnf("shutdown: %v", err)
	}
}

// current returns the tracer and options under the read lock, or ok=false
// when tracing is off.
func current() (trace.Tracer, Options, *Redactor, bool) {
	if !enabled.Load() {
		return nil, Options{}, nil, false
	}
	mu.RLock()
	defer mu.RUnlock()
	if tracer == nil {
		return nil, Options{}, nil, false
	}
	return tracer, opts, redactor, true
}

// field prepares a text field for export: redacted, then capped at
// MaxFieldBytes with a marker saying how much was cut. Returns the number of
// redactions applied so callers can record it in metadata.
func field(o Options, r *Redactor, s string) (string, int) {
	if s == "" {
		return "", 0
	}
	s, n := r.Redact(s)
	if len(s) > o.MaxFieldBytes {
		cut := len(s) - o.MaxFieldBytes
		s = s[:o.MaxFieldBytes] + fmt.Sprintf("\n…[truncated %d bytes]", cut)
	}
	return s, n
}

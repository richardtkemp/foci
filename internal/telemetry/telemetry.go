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
//   - the cost ledger's Book (internal/delegator/accounting) — every booked
//     call passes through accounting.BookedHook once its transaction commits,
//     and each becomes exactly one "generation" observation carrying that
//     call's model, usage and priced cost. Cost therefore lives in exactly one
//     place, and SUM(observation cost) over a day equals the ledger's
//     daily_costs by construction — which is what
//     scripts/langfuse-etl/etl.py reconcile checks.
//
// Identity is deterministic: every id is a SHA-256 of the ledger's turn_id
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

	"foci/internal/delegator/accounting"
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
	// SecretValues is called each time a text field is exported, so values
	// added, changed or overridden in an [agents.<id>.*] table after Init
	// are scrubbed too — the gateway holds the secrets store, so it hands
	// over a function reading the store's CURRENT values and scrubs the
	// VALUES directly rather than hashing candidate substrings the way the
	// backfill ETL must. Results are accumulated for the process lifetime
	// (a value removed from the store stays scrubbed — a rotated-out
	// credential can still appear in later output). Nil means only the
	// generic credential patterns apply.
	SecretValues func() []string
}

var (
	enabled atomic.Bool

	mu             sync.RWMutex
	tracerProvider *sdktrace.TracerProvider
	tracer         trace.Tracer
	opts           Options
	// redactSet is the never-shrinking union of secret values seen since
	// Init; redactor is derived from it and replaced whole (never mutated)
	// when the set grows. Both are guarded by mu and reset by initWith.
	redactSet map[string]bool
	redactor  *Redactor

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
	redactSet = nil // fresh union per arm; seeded from o.SecretValues below
	redactor = nil  // rebuilt by redactorFor, never carried across arms
	mu.Unlock()

	// Seed the union with the startup values BEFORE arming, so a secret
	// removed from the store between Init and the first exported field is
	// still scrubbed — the behaviour the one-time slice of old had. The
	// function runs here outside mu: the secrets store's own lock never
	// nests under telemetry's. No field can export yet (enabled is still
	// false), so the seed cannot race the first export.
	redactorFor(o.SecretValues)

	accounting.BookedHook = recordBooking
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
	accounting.BookedHook = nil
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
func current() (trace.Tracer, Options, bool) {
	if !enabled.Load() {
		return nil, Options{}, false
	}
	mu.RLock()
	defer mu.RUnlock()
	if tracer == nil {
		return nil, Options{}, false
	}
	return tracer, opts, true
}

// field prepares a text field for export: the redactor is refreshed from
// the live secret-value function in o, applied, then capped at MaxFieldBytes
// with a marker saying how much was cut. Returns the number of redactions
// applied so callers can record it in metadata.
func field(o Options, s string) (string, int) {
	if s == "" {
		return "", 0
	}
	s, n := redactorFor(o.SecretValues).Redact(s)
	if len(s) > o.MaxFieldBytes {
		cut := len(s) - o.MaxFieldBytes
		s = s[:o.MaxFieldBytes] + fmt.Sprintf("\n…[truncated %d bytes]", cut)
	}
	return s, n
}

// redactorFor returns the Redactor to scrub one exported field with. It
// reads the current secret values from fn OUTSIDE the lock (nil fn = only
// the generic credential patterns apply, as with an empty list), merges any
// never-seen values into redactSet — the never-shrinking union that keeps a
// value removed from the store scrubbed, because a rotated-out credential
// can still appear in later output — and rebuilds the redactor via
// NewRedactor when the union grew, so NewRedactor stays the single owner of
// the value rules (trim, ≥ 8 chars, dedupe, longest first, generic
// patterns). No watcher: fn runs on the exporting goroutine, the same way
// the secrets store's own redaction does for tool output.
func redactorFor(fn func() []string) *Redactor {
	var fresh []string
	if fn != nil {
		fresh = fn()
	}
	mu.Lock()
	defer mu.Unlock()
	if redactSet == nil {
		redactSet = make(map[string]bool, len(fresh))
	}
	grew := false
	for _, v := range fresh {
		if !redactSet[v] {
			redactSet[v] = true
			grew = true
		}
	}
	if grew || redactor == nil {
		values := make([]string, 0, len(redactSet))
		for v := range redactSet {
			values = append(values, v)
		}
		redactor = NewRedactor(values)
	}
	return redactor
}

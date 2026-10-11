package provider

import (
	"errors"
	"fmt"
)

// ModelTuple names one way of reaching a model: the canonical model id
// (developer/model_id) plus the endpoint and wire format a request to it
// uses. The client leg is the caller's concern — this triple is what a
// request's post-processing (gate release, ledger booking) pairs with.
type ModelTuple struct {
	Model    string
	Endpoint string
	Format   string
}

// ServedReport names the fallback hop that served a response. Model is the
// hop's canonical model. Endpoint and Format are the hop's legs when it went
// through its own client (from ClientProvider.GetClient), and empty when it
// reused the caller's client — the caller's legs served then. The zero value
// means the primary request served (first try, retries, or the 400
// strip-and-retry).
type ServedReport struct {
	Model    string
	Endpoint string
	Format   string
}

// ServedTuple combines the caller's primary tuple with a successful
// response's serving report (MessageResponse.Served) into the tuple that
// actually served the request:
//
//   - an empty report keeps the primary tuple (the primary served);
//   - a report with legs takes the hop's endpoint and format;
//   - a report without legs (the hop reused the caller's client) serves the
//     hop's model on the caller's endpoint and format.
//
// The empty-means-primary rule lives here alone; callers never re-derive it.
func ServedTuple(primary ModelTuple, resp *MessageResponse) ModelTuple {
	if resp == nil {
		return primary
	}
	return tupleFor(primary, resp.Served)
}

// FailedHop is the error-side twin of MessageResponse.Served: it labels a
// provider.Send error with the fallback hop that returned it, so a caller
// can pair its error-path work (rate-limit gate closure) with the tuple that
// FAILED — the hop's — instead of the primary's. Read it through
// FailedTuple. Unwrap keeps errors.As/errors.Is reaching the hop's own
// error (*APIError, context.DeadlineExceeded, ...).
type FailedHop struct {
	// Report names the hop whose error this is, exactly as a successful
	// response's Served would: the hop's canonical model, plus its endpoint
	// and format when it went through its own client — empty legs mean the
	// hop reused the caller's client.
	Report ServedReport
	Err    error
}

func (h *FailedHop) Error() string { return fmt.Sprintf("fallback hop %s: %v", h.Report.Model, h.Err) }

func (h *FailedHop) Unwrap() error { return h.Err }

// FailedTuple returns the tuple whose failure a provider.Send error carries:
// the hop's (under ServedTuple's leg rules) when the error wraps a FailedHop,
// else the primary — the primary's own failure, no fallback hop attempted.
func FailedTuple(primary ModelTuple, err error) ModelTuple {
	var hop *FailedHop
	if !errors.As(err, &hop) {
		return primary
	}
	return tupleFor(primary, hop.Report)
}

// tupleFor applies the empty-means-primary leg rule shared by ServedTuple
// (success) and FailedTuple (failure). Unexported; the rule lives here alone.
func tupleFor(primary ModelTuple, report ServedReport) ModelTuple {
	if report == (ServedReport{}) {
		return primary
	}
	if report.Endpoint == "" && report.Format == "" {
		return ModelTuple{Model: report.Model, Endpoint: primary.Endpoint, Format: primary.Format}
	}
	return ModelTuple{Model: report.Model, Endpoint: report.Endpoint, Format: report.Format}
}

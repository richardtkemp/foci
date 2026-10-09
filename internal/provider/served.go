package provider

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
	if resp == nil || resp.Served == (ServedReport{}) {
		return primary
	}
	if resp.Served.Endpoint == "" && resp.Served.Format == "" {
		return ModelTuple{Model: resp.Served.Model, Endpoint: primary.Endpoint, Format: primary.Format}
	}
	return ModelTuple{Model: resp.Served.Model, Endpoint: resp.Served.Endpoint, Format: resp.Served.Format}
}

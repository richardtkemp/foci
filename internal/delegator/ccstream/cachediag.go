package ccstream

// cachediag.go — what CC's api-request records say a request changed (#3221).
//
// CC 2.1.3xx writes an api-request record to the transcript as it sends each
// request: its id, the id of the request it extends (base), how many of the
// base's messages it kept unchanged (keep), the digests of the messages it
// appended (tail), a hash of its system prompt and tools (shapeHash), and its
// other parameters (params). The assistant line of the response names the
// record (requestRef). So a request changed nothing before its new messages
// when it kept every message its base sent (base.keep + len(base.tail)) and
// has the base's shape, model and parameters.
//
// A shape change does not by itself lose the cache: a tool added mid-
// conversation reads in full (observed 21 Nov 2026). The verdict is evidence
// for a bust the cache watch has already found, never a bust detector.

import (
	"bytes"
	"encoding/json"
	"strings"

	"foci/internal/delegator/accounting"
)

// ccRequestDigestCap bounds the requests kept: a request's base is the one CC
// sent just before it, so a short window of recent ones is enough.
const ccRequestDigestCap = 64

// ccRequestDigest is one api-request record, as the comparison needs it.
type ccRequestDigest struct {
	base         string
	keep, sent   int // sent = keep + len(tail): the messages the request carried
	shape, model string
	params       string // the params other than model, key-sorted
}

// ccRequestDigests holds the recent api-request records of one transcript.
// Not safe for concurrent use: one tail goroutine owns it.
type ccRequestDigests struct {
	reqs  map[string]ccRequestDigest
	order []string
}

func newCCRequestDigests() *ccRequestDigests {
	return &ccRequestDigests{reqs: make(map[string]ccRequestDigest)}
}

// observe records raw if it is an api-request record. A cheap substring check
// skips the unmarshal on every other line.
func (d *ccRequestDigests) observe(raw []byte) {
	if !bytes.Contains(raw, []byte(`"api-request"`)) {
		return
	}
	var r struct {
		Type      string                     `json:"type"`
		ID        string                     `json:"id"`
		Base      string                     `json:"base"`
		Keep      int                        `json:"keep"`
		Tail      []json.RawMessage          `json:"tail"`
		ShapeHash string                     `json:"shapeHash"`
		Params    map[string]json.RawMessage `json:"params"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &r) != nil || r.Type != "api-request" || r.ID == "" {
		return
	}
	var model string
	if m, ok := r.Params["model"]; ok {
		_ = json.Unmarshal(m, &model)
		delete(r.Params, "model")
	}
	params, _ := json.Marshal(r.Params) // map keys marshal sorted
	if _, seen := d.reqs[r.ID]; !seen {
		d.order = append(d.order, r.ID)
		if len(d.order) > ccRequestDigestCap {
			delete(d.reqs, d.order[0])
			d.order = d.order[1:]
		}
	}
	d.reqs[r.ID] = ccRequestDigest{base: r.Base, keep: r.Keep, sent: r.Keep + len(r.Tail),
		shape: r.ShapeHash, model: model, params: string(params)}
}

// change is what request ref changed against its base: RequestUnchanged, a
// comma-separated list of accounting.RequestChanged* parts, or "" when ref or
// its base is not known (no record, the first request of this process).
func (d *ccRequestDigests) change(ref string) string {
	req, ok := d.reqs[ref]
	if !ok || req.base == "" {
		return ""
	}
	base, ok := d.reqs[req.base]
	if !ok {
		return ""
	}
	var parts []string
	if req.model != base.model {
		parts = append(parts, accounting.RequestChangedModel)
	}
	if req.shape != base.shape {
		parts = append(parts, accounting.RequestChangedSystemOrTools)
	}
	if req.params != base.params {
		parts = append(parts, accounting.RequestChangedParams)
	}
	if req.keep < base.sent {
		parts = append(parts, accounting.RequestChangedMessages)
	}
	if len(parts) == 0 {
		return accounting.RequestUnchanged
	}
	return strings.Join(parts, ",")
}

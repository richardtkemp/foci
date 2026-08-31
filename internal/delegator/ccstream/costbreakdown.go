package ccstream

import (
	"fmt"
	"time"

	"foci/internal/modelinfo"
)

// costBreakdown renders the per-class token counts and prices behind one turn's
// calculated cost, for the divergence warning (#1695).
//
// It exists because the warning previously reported only two scalars, and the
// question it always provokes — WHICH class disagrees — was then answerable only
// by reconstructing the turn from CC's transcript. api.db cannot answer it: its
// token columns hold the final cycle's context fill, not the deltas that were
// priced, so a single-cycle turn whose row prices at $0.74 can legitimately
// carry a calculated cost of $4.39 and look like a pricing bug.
type costBreakdown struct {
	model      string
	cycles     int
	input      int
	output     int
	cacheRead  int
	cacheWrite int
}

// String prices each class through modelinfo.CostAsOf with the other classes
// zeroed. Deliberately the same function that produced the total rather than a
// local rate lookup: a second copy of the rate logic could disagree with the
// figure under investigation, which would make this line actively misleading at
// exactly the moment it is being trusted.
func (b costBreakdown) String() string {
	now := time.Now()
	price := func(in, out, cr, cw int) float64 {
		return modelinfo.CostAsOf(b.model, now, in, out, cr, cw)
	}
	return fmt.Sprintf(
		"cycles=%d in=%d ($%.6f) out=%d ($%.6f) cache_read=%d ($%.6f) cache_write=%d ($%.6f)",
		b.cycles,
		b.input, price(b.input, 0, 0, 0),
		b.output, price(0, b.output, 0, 0),
		b.cacheRead, price(0, 0, b.cacheRead, 0),
		b.cacheWrite, price(0, 0, 0, b.cacheWrite),
	)
}

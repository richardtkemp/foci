package modelinfo

import (
	"testing"
	"time"
)

// TestCostOnEndpoint (#3035): a call on a zero-cost endpoint is priced at $0
// without consulting the rate table (no unpriced-model warning), the
// zero-cost rate group is a real group with every class at $0, and any other
// endpoint prices as CostAsOf.
func TestCostOnEndpoint(t *testing.T) {
	t.Cleanup(func() { SetZeroCostEndpoints(nil) })
	prev := UnpricedModelHook
	var warned []string
	UnpricedModelHook = func(m string) { warned = append(warned, m) }
	t.Cleanup(func() { UnpricedModelHook = prev })
	unpricedMu.Lock()
	delete(unpricedSeen, "qwen-zc-3035")
	unpricedMu.Unlock()

	SetZeroCostEndpoints([]string{"local"})
	tokens := Tokens{ClassInput: 1000, ClassOutput: 500}
	at := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	if usd, priced := CostOnEndpoint("local", "qwen-zc-3035", at, tokens); usd != 0 || !priced {
		t.Errorf("zero-cost endpoint: $%v priced=%v, want $0 priced", usd, priced)
	}
	if len(warned) != 0 {
		t.Errorf("UnpricedModelHook fired on a zero-cost endpoint: %v", warned)
	}
	if _, priced := CostOnEndpoint("other", "qwen-zc-3035", at, tokens); priced || len(warned) != 1 {
		t.Errorf("other endpoint: priced=%v warned=%v, want unpriced and one warning", priced, warned)
	}

	rates, ok := RatesAsOf(ZeroCostRateModel, at)
	if !ok || len(rates) != len(Classes()) {
		t.Fatalf("RatesAsOf(%q) = %v, %v; want every class", ZeroCostRateModel, rates, ok)
	}
	for c, r := range rates {
		if r != 0 {
			t.Errorf("zero-cost rate for %s = %v", c, r)
		}
	}
	n := 0
	for _, row := range RateTable() {
		if row.RateModel == ZeroCostRateModel {
			n++
			if row.USDPerUnit == nil || *row.USDPerUnit != 0 {
				t.Errorf("token_rates row %+v, want $0", row)
			}
		}
	}
	if n != len(Classes()) {
		t.Errorf("RateTable has %d zero-cost rows, want one per class (%d)", n, len(Classes()))
	}
}

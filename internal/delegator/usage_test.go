package delegator

import "testing"

func TestUsageInfoAvailable(t *testing.T) {
	win := func(pct int) UsageWindow { return UsageWindow{Percent: pct, Reported: true} }
	cases := []struct {
		name string
		info *UsageInfo
		want bool
	}{
		{"both under", &UsageInfo{RateLimitsAvailable: true, FiveHour: win(10), SevenDay: win(97)}, true},
		{"five hour at threshold", &UsageInfo{RateLimitsAvailable: true, FiveHour: win(98), SevenDay: win(10)}, false},
		{"weekly reached", &UsageInfo{RateLimitsAvailable: true, FiveHour: win(1), SevenDay: win(100)}, false},
		{"one reported, under", &UsageInfo{RateLimitsAvailable: true, FiveHour: win(5)}, true},
		{"none reported", &UsageInfo{RateLimitsAvailable: true}, false},
		{"rate limits not available", &UsageInfo{FiveHour: win(1), SevenDay: win(1)}, false},
		{"codex allowance", &UsageInfo{RateLimitsAvailable: true, FiveHour: win(1), Allowance: &Allowance{}}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := c.info.Available(98); got != c.want {
			t.Errorf("%s: Available(98) = %v, want %v", c.name, got, c.want)
		}
	}
}

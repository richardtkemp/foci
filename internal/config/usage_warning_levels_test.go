package config

import (
	"reflect"
	"strings"
	"testing"

	"foci/internal/delegator"
)

// TestUsageWarningLevels_Default (#3145): unset resolves to 50..90 in tens
// plus 95..99, and the backends' fallback (delegator, which cannot import
// config) is the same list.
func TestUsageWarningLevels_Default(t *testing.T) {
	want := []int{50, 60, 70, 80, 90, 95, 96, 97, 98, 99}
	if got := (NotifyConfig{}).UsageWarningLevelList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(delegator.DefaultUsageWarningLevels, want) {
		t.Fatalf("delegator.DefaultUsageWarningLevels = %v, want %v", delegator.DefaultUsageWarningLevels, want)
	}
}

func TestParseUsageWarningLevels(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    []int
		wantErr string
	}{
		{"80, 50,90,80", []int{50, 80, 90}, ""},
		{"none", []int{}, ""},
		{" NONE ", []int{}, ""},
		{"99", []int{99}, ""},
		{"", nil, "empty"},
		{"50,abc", nil, "not a whole number"},
		{"0,50", nil, "between 1 and 99"},
		{"100", nil, "between 1 and 99"},
		{"50,,60", nil, "not a whole number"},
	} {
		got, err := ParseUsageWarningLevels(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%q: err %v, want one containing %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

// TestUsageWarningLevels_AgentOverridesGlobal: the per-agent value wins over
// the global one, and an agent that sets none inherits it.
func TestUsageWarningLevels_AgentOverridesGlobal(t *testing.T) {
	global, agent := "60,90", "none"
	g := NotifyConfig{UsageWarningLevels: &global}
	if got := resolveNotify(Merge(NotifyConfig{}, g)).UsageWarningLevels; !reflect.DeepEqual(got, []int{60, 90}) {
		t.Fatalf("inherited = %v, want [60 90]", got)
	}
	if got := resolveNotify(Merge(NotifyConfig{UsageWarningLevels: &agent}, g)).UsageWarningLevels; len(got) != 0 || got == nil {
		t.Fatalf("agent none = %#v, want an empty, non-nil list", got)
	}
}

func TestValidateUsageWarningLevels(t *testing.T) {
	bad := "50,150"
	if err := validateUsageWarningLevels("agent \"x\" [notify]", &bad); err == nil || !strings.Contains(err.Error(), "usage_warning_levels") {
		t.Fatalf("err = %v, want a usage_warning_levels error", err)
	}
	if err := validateUsageWarningLevels("[notify]", nil); err != nil {
		t.Fatalf("unset: %v", err)
	}
}

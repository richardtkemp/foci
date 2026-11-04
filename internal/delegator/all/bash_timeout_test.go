package all_test

import (
	"testing"
	"time"

	"foci/internal/delegator"
	_ "foci/internal/delegator/all"
)

// #2812 req 4: the limit of a chat "!" command per backend, from the registered
// specs themselves.
func TestBashDefaultTimeout_PerBackend(t *testing.T) {
	t.Parallel()
	ccEnv := map[string]string{"BASH_DEFAULT_TIMEOUT_MS": "2000"}
	ocEnv := map[string]string{"OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS": "3000"}
	for _, tc := range []struct {
		backend string
		env     map[string]string
		want    time.Duration
	}{
		{"claude-code", nil, 120 * time.Second},
		{"claude-code", ccEnv, 2 * time.Second},
		{"claude-code", map[string]string{"BASH_DEFAULT_TIMEOUT_MS": "soon"}, 120 * time.Second},
		{"codex", ccEnv, 10 * time.Second},
		{"opencode", ccEnv, 120 * time.Second},
		{"opencode", ocEnv, 3 * time.Second},
	} {
		spec, ok := delegator.SpecFor(tc.backend)
		if !ok {
			t.Fatalf("no spec for %s", tc.backend)
		}
		if got := spec.BashDefaultTimeout(tc.env); got != tc.want {
			t.Errorf("%s env=%v: %s, want %s", tc.backend, tc.env, got, tc.want)
		}
	}
}

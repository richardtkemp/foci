package config

import (
	"strings"
	"testing"
)

// TestValidateEndpointMaxInFlight (#3019): [endpoints.*] max_in_flight loads
// as written (unset = 0, no limit) and a negative value is refused.
func TestValidateEndpointMaxInFlight(t *testing.T) {
	for _, tc := range []struct {
		line    string
		want    int
		wantErr string
	}{
		{line: "", want: 0},
		{line: "max_in_flight = 1\n", want: 1},
		{line: "max_in_flight = 4\n", want: 4},
		{line: "max_in_flight = -1\n", wantErr: "max_in_flight = -1"},
	} {
		body := "[[agents]]\nid = \"a\"\n\n[endpoints.local]\nformat = \"openai\"\nurl = \"http://127.0.0.1:11434/v1\"\n" + tc.line
		cfg, err := loadTOML(t, body)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "endpoints.local") {
				t.Errorf("%q: err = %v, want it to name endpoints.local and %s", tc.line, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: load: %v", tc.line, err)
		}
		if got := cfg.Endpoints["local"].MaxInFlight; got != tc.want {
			t.Errorf("%q: MaxInFlight = %d, want %d", tc.line, got, tc.want)
		}
	}
}

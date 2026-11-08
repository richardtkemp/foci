package config

import (
	"strings"
	"testing"
)

// TestValidateEndpointInstructionRole (#2987): [endpoints.*] instruction_role
// accepts "developer" and "system" (and unset, meaning developer) and refuses
// anything else.
func TestValidateEndpointInstructionRole(t *testing.T) {
	for _, tc := range []struct {
		value, wantErr, wantEffective string
	}{
		{value: "", wantEffective: "developer"},
		{value: "developer", wantEffective: "developer"},
		{value: "system", wantEffective: "system"},
		{value: "user", wantErr: `instruction_role = "user"`},
		{value: "System", wantErr: `instruction_role = "System"`},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			body := "[[agents]]\nid = \"a\"\n\n[endpoints.local]\nformat = \"openai\"\nurl = \"http://127.0.0.1:11434/v1\"\n"
			if tc.value != "" {
				body += "instruction_role = \"" + tc.value + "\"\n"
			}
			cfg, err := loadTOML(t, body)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "endpoints.local") {
					t.Fatalf("err = %v, want it to name endpoints.local and %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.Endpoints["local"].EffectiveInstructionRole(); got != tc.wantEffective {
				t.Errorf("EffectiveInstructionRole = %q, want %q", got, tc.wantEffective)
			}
		})
	}
}

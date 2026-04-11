package periodic

import (
	"testing"
	"time"
)

func TestTickInterval(t *testing.T) {
	// Guards the tickInterval constant against accidental changes; the 30-second value is relied
	// upon by all periodic scheduling logic.
	if tickInterval != 30*time.Second {
		t.Errorf("tick interval = %v, want 30s", tickInterval)
	}
}


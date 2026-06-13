package browser

import (
	"os"
	"testing"
)

// TestMain stops the lazily-initialized sharedBrowserMgr after the suite (it
// spawns a real headless browser process). Before 2.1's package split this
// cleanup lived in the unified tools TestMain.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedBrowserMgr != nil {
		sharedBrowserMgr.Stop()
	}
	os.Exit(code)
}

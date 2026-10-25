package browser

import (
	"os"
	"testing"

	"github.com/ysmood/leakless"
)

// TestMain stops the lazily-initialized sharedBrowserMgr after the suite (it
// spawns a real headless browser process). Before 2.1's package split this
// cleanup lived in the unified tools TestMain.
//
// It also extracts go-rod's leakless guard binary before any test runs. Every
// launch (real or fake chromium) execs it from $TMPDIR, which the test harness
// recreates per run, and leakless.GetLeaklessBin writes it with a plain
// check-then-WriteFile. The tests run in parallel, so without this two first
// launches could race: one truncating the file while another execs it.
func TestMain(m *testing.M) {
	if leakless.Support() {
		leakless.GetLeaklessBin()
	}
	code := m.Run()
	if sharedBrowserMgr != nil {
		sharedBrowserMgr.Stop()
	}
	os.Exit(code)
}

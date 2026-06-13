package convo

import (
	"bytes"
	"os"
	"testing"

	"foci/internal/log"
)

// initConversation opens a single fallback conversation log for tests.
func initConversation(path string) error {
	cl, err := openLog(path)
	if err != nil {
		return err
	}
	convLogs = map[string]*agentLog{"": cl}
	convFallback = cl
	return nil
}

// resetConvo clears the package globals for test isolation.
func resetConvo() {
	convLogs = nil
	convFallback = nil
	Hook = nil
}

// captureLog redirects log event output to a buffer (convo reports insert
// errors via log.Errorf) and restores stderr on cleanup.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

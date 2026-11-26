package ccstream

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"foci/internal/delegator"
)

// Frames captured from CC 2.1.325 (#3281): a /compact resumed with
// HTTPS_PROXY on a closed port. CC opens the compaction, then closes it with
// compact_result "failed" and its reason, then goes idle with no boundary.
const (
	frame3281Compacting = `{"type":"system","subtype":"status","status":"compacting","session_id":"4c023215-9517-4558-85f1-9bd0f629c834","uuid":"0d62ac1e-85db-4022-a262-58c2f93947c4"}`
	frame3281Failed     = `{"type":"system","subtype":"status","status":null,"compact_result":"failed","compact_error":"Error during compaction: API Error: Connection refused — a firewall or proxy may be blocking it (ECONNREFUSED)","session_id":"4c023215-9517-4558-85f1-9bd0f629c834","uuid":"b092fe70-1a29-4382-a6c0-bedf0f6a392a"}`
	frame3281Idle       = `{"type":"system","subtype":"session_state_changed","state":"idle","uuid":"a7c59674-7f67-45c7-8ad7-7a9549870ef2","session_id":"4c023215-9517-4558-85f1-9bd0f629c834"}`
)

// runCompactFrames arms a compaction wait, feeds the frames through OnSystem
// and returns the wait's outcome.
func runCompactFrames(t *testing.T, b *Backend, frames ...string) error {
	t.Helper()
	for _, f := range frames {
		var probe struct {
			Subtype string `json:"subtype"`
		}
		if err := json.Unmarshal([]byte(f), &probe); err != nil {
			t.Fatalf("bad test frame %q: %v", f, err)
		}
		b.OnSystem(probe.Subtype, []byte(f))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return b.WaitForCompaction(ctx)
}

// statusFailed is a compact_result "failed" status frame with the given
// compact_error (omitted when nil).
func statusFailed(compactError *string) string {
	if compactError == nil {
		return `{"type":"system","subtype":"status","status":null,"compact_result":"failed"}`
	}
	return `{"type":"system","subtype":"status","status":null,"compact_result":"failed","compact_error":` + jsonQuote(*compactError) + `}`
}

func TestCompactionFailed_3281_NetworkFailureIsReportedAsFailure(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.ArmCompactionWait()
	err := runCompactFrames(t, b, frame3281Compacting, frame3281Failed, frame3281Idle)
	if errors.Is(err, delegator.ErrCompactionNoBoundary) {
		t.Fatalf("a failed compaction was reported as a decline: %v", err)
	}
	var failed *delegator.CompactionFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("err = %v (%T), want *delegator.CompactionFailedError", err, err)
	}
	const want = "API Error: Connection refused — a firewall or proxy may be blocking it (ECONNREFUSED)"
	if failed.Reason != want {
		t.Errorf("Reason = %q, want %q", failed.Reason, want)
	}
}

func TestCompactionFailed_3281_Outcomes(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	cases := []struct {
		name       string
		err        *string
		wantNoBnd  bool   // decline: ErrCompactionNoBoundary after idle
		wantStop   bool   // ErrCompactionStopped
		wantReason string // CompactionFailedError.Reason otherwise
	}{
		{name: "not enough messages is a decline", err: str("Not enough messages to compact."), wantNoBnd: true},
		{name: "auto-path abort is a stop", err: str("aborted"), wantStop: true},
		{name: "manual-path abort is a stop", err: str("API Error: Request was aborted."), wantStop: true},
		{name: "auth failure", err: str("Error during compaction: Failed to authenticate: OAuth session expired and could not be refreshed"), wantReason: "Failed to authenticate: OAuth session expired and could not be refreshed"},
		{name: "hook block is a failure", err: str("Compaction blocked by PreCompact hook"), wantReason: "Compaction blocked by PreCompact hook"},
		{name: "no reason given", err: nil, wantReason: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := &Backend{}
			b.ArmCompactionWait()
			err := runCompactFrames(t, b, frame3281Compacting, statusFailed(tc.err), frame3281Idle)
			switch {
			case tc.wantNoBnd:
				if !errors.Is(err, delegator.ErrCompactionNoBoundary) {
					t.Fatalf("err = %v, want ErrCompactionNoBoundary", err)
				}
			case tc.wantStop:
				if !errors.Is(err, delegator.ErrCompactionStopped) {
					t.Fatalf("err = %v, want ErrCompactionStopped", err)
				}
			default:
				var failed *delegator.CompactionFailedError
				if !errors.As(err, &failed) {
					t.Fatalf("err = %v (%T), want *delegator.CompactionFailedError", err, err)
				}
				if failed.Reason != tc.wantReason {
					t.Errorf("Reason = %q, want %q", failed.Reason, tc.wantReason)
				}
			}
		})
	}
}

// A success status frame changes nothing: the boundary decides.
func TestCompactionFailed_3281_SuccessStatusIsNotAFailure(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.ArmCompactionWait()
	ok := `{"type":"system","subtype":"status","status":null,"compact_result":"success"}`
	err := runCompactFrames(t, b, frame3281Compacting, ok, `{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"manual","pre_tokens":1000}}`, frame3281Idle)
	if err != nil {
		t.Fatalf("err = %v, want nil (compacted)", err)
	}
}

// While the /compact is deferred behind a running turn, a failed
// auto-compaction inside that turn must not resolve the /compact's wait
// (#2147).
func TestCompactionFailed_3281_DeferredIgnoresFailedStatus(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	b.ArmCompactionWait()
	b.turnMu.Lock()
	b.compactDeferred = true
	b.turnMu.Unlock()
	b.OnSystem("status", []byte(frame3281Failed))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.WaitForCompaction(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the wait still armed (DeadlineExceeded)", err)
	}
}

package usercrontab

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCrontab installs a fake `crontab` script as Bin. It keeps the crontab
// in a file under t.TempDir; readMode/writeMode select its behaviour:
// "ok" (normal), "fail" (exit 1 with a non-"no crontab" error), "hang"
// (sleep far past Timeout, in a child process that keeps the pipes open).
// It returns the crontab file's path.
func fakeCrontab(t *testing.T, readMode, writeMode string) string {
	t.Helper()
	dir := t.TempDir()
	tab := filepath.Join(dir, "crontab.txt")
	script := fmt.Sprintf(`#!/bin/sh
f=%q
case "$1" in
-l)
  case %q in
  fail) echo "crontab: permission denied" >&2; exit 1 ;;
  hang) sleep 30 ;;
  esac
  if [ -f "$f" ]; then cat "$f"; else echo "crontab: no crontab for test" >&2; exit 1; fi ;;
-)
  case %q in
  fail) echo "crontab: tmp/tmp.1: Operation not permitted" >&2; exit 1 ;;
  hang) sleep 30 ;;
  esac
  cat > "$f" ;;
*) exit 2 ;;
esac
`, tab, readMode, writeMode)
	bin := filepath.Join(dir, "crontab")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: test executable
		t.Fatal(err)
	}
	orig := Bin
	Bin = bin
	t.Cleanup(func() { Bin = orig })
	return tab
}

func TestAppendKeepsExistingJobsAndContentVerbatim(t *testing.T) {
	tab := fakeCrontab(t, "ok", "ok")
	existing := "PATH=/usr/bin:/bin\n0 1 * * * existing-job\n"
	if err := os.WriteFile(tab, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	// $, backslashes, quotes and % pass through untouched: no shell sees them.
	lines := []string{
		`40 2 * * 1 foci branch -a x -m "$(date) \n 'q'" >> $HOME/logs/cron.log`,
		`*/30 * * * * echo 100\% done`,
	}
	if err := Append(context.Background(), lines); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := os.ReadFile(tab)
	want := existing + "\n" + lines[0] + "\n" + lines[1] + "\n"
	if string(got) != want {
		t.Errorf("crontab =\n%q\nwant\n%q", got, want)
	}
}

func TestAppendToAbsentCrontab(t *testing.T) {
	tab := fakeCrontab(t, "ok", "ok")
	if err := Append(context.Background(), []string{"0 4 * * * job"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, _ := os.ReadFile(tab)
	if string(got) != "\n0 4 * * * job\n" {
		t.Errorf("crontab = %q", got)
	}
}

// A read that fails for any reason other than "no crontab" must install
// nothing: treating it as empty would replace the user's jobs with ours.
func TestAppendFailedReadInstallsNothing(t *testing.T) {
	tab := fakeCrontab(t, "fail", "ok")
	if err := os.WriteFile(tab, []byte("0 1 * * * existing-job\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Append(context.Background(), []string{"0 4 * * * job"})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Append err = %v, want the read error", err)
	}
	got, _ := os.ReadFile(tab)
	if string(got) != "0 1 * * * existing-job\n" {
		t.Errorf("crontab changed after a failed read: %q", got)
	}
}

func TestInstallErrorCarriesStderr(t *testing.T) {
	fakeCrontab(t, "ok", "fail")
	err := Install(context.Background(), "x\n")
	if err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("Install err = %v, want stderr in it", err)
	}
}

// #2976: `crontab -` that never returns (macOS privacy prompt) must not hang
// the caller — it is stopped at Timeout and reported.
func TestInstallThatHangsIsStoppedAtTimeout(t *testing.T) {
	fakeCrontab(t, "ok", "hang")
	orig := Timeout
	Timeout = 300 * time.Millisecond
	t.Cleanup(func() { Timeout = orig })

	start := time.Now()
	err := Append(context.Background(), []string{"0 4 * * * job"})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "no answer after") {
		t.Fatalf("Append err = %v, want a timeout error", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Append took %s; the timeout did not stop it", elapsed)
	}
}

func TestReadThatHangsIsStoppedAtTimeout(t *testing.T) {
	fakeCrontab(t, "hang", "ok")
	orig := Timeout
	Timeout = 300 * time.Millisecond
	t.Cleanup(func() { Timeout = orig })

	start := time.Now()
	_, err := Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no answer after") {
		t.Fatalf("Read err = %v, want a timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Read took %s; the timeout did not stop it", elapsed)
	}
}

func TestAppendLines(t *testing.T) {
	for _, tc := range []struct{ cur, want string }{
		{"", "\na\nb\n"},
		{"x\n", "x\n\na\nb\n"},
		{"x", "x\n\na\nb\n"},
	} {
		if got := AppendLines(tc.cur, []string{"a", "b"}); got != tc.want {
			t.Errorf("AppendLines(%q) = %q, want %q", tc.cur, got, tc.want)
		}
	}
}

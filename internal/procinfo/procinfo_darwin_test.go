//go:build darwin

package procinfo

import (
	"encoding/binary"
	"testing"
)

func TestParsePS(t *testing.T) {
	out := `    1     0     0  25648 /sbin/launchd
  100     1   501   9824 /Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Helper
  bad line here
  200     1   501      x /bin/zsh
  300   100   502   4000 /bin/zsh
`
	procs := parsePS(out)
	if len(procs) != 3 {
		t.Fatalf("parsePS: got %d rows, want 3 (malformed skipped): %+v", len(procs), procs)
	}
	if procs[1].Comm != "Google Chrome Helper" {
		t.Errorf("comm with spaces: got %q", procs[1].Comm)
	}
	if p := procs[2]; p.PID != 300 || p.PPID != 100 || p.UID != 502 || p.RSSKB != 4000 || p.Comm != "zsh" {
		t.Errorf("row 2 = %+v", p)
	}
}

func TestParseProcargs2(t *testing.T) {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, 3)
	buf = append(buf, []byte("/usr/local/bin/opencode\x00\x00\x00\x00opencode\x00serve\x00--port=1\x00HOME=/x\x00")...)
	args, err := parseProcargs2(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 || args[0] != "opencode" || args[1] != "serve" || args[2] != "--port=1" {
		t.Errorf("parseProcargs2 = %q, want [opencode serve --port=1] (env excluded)", args)
	}
	if _, err := parseProcargs2([]byte{1}); err == nil {
		t.Error("short buffer: want error")
	}
}

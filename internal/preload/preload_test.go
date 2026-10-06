package preload

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeShim creates a fake nosgid.so in a temp dir and returns its path.
func writeShim(t *testing.T) string {
	t.Helper()
	so := filepath.Join(t.TempDir(), "lib", "nosgid.so")
	if err := os.MkdirAll(filepath.Dir(so), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(so, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return so
}

// trustingView models a correctly-provisioned install: every component is
// owned by a trusted uid and nothing on the path is writable by this process.
// The real filesystem is still used for lstat/symlinks; only the two
// permission questions are answered by the fake (a test cannot create
// root-owned files).
func trustingView() trustView {
	return trustView{
		lstat:        os.Lstat,
		canWrite:     func(string) bool { return false },
		trustedOwner: func(uint32) bool { return true },
	}
}

func TestApply_SetsPreloadWhenShimTrusted(t *testing.T) {
	so := writeShim(t)
	t.Setenv("LD_PRELOAD", "")

	apply(so, trustingView())

	if got := os.Getenv("LD_PRELOAD"); got != so {
		t.Fatalf("LD_PRELOAD = %q, want %q", got, so)
	}
}

func TestApply_PrependsToExisting(t *testing.T) {
	so := writeShim(t)
	t.Setenv("LD_PRELOAD", "/other/lib.so")

	apply(so, trustingView())

	want := so + " /other/lib.so"
	if got := os.Getenv("LD_PRELOAD"); got != want {
		t.Fatalf("LD_PRELOAD = %q, want %q", got, want)
	}
}

func TestApply_IdempotentWhenAlreadyPresent(t *testing.T) {
	so := writeShim(t)
	pre := so + " /other/lib.so"
	t.Setenv("LD_PRELOAD", pre)

	apply(so, trustingView())

	if got := os.Getenv("LD_PRELOAD"); got != pre {
		t.Fatalf("LD_PRELOAD = %q, want unchanged %q", got, pre)
	}
}

func TestApply_NoopWhenShimMissing(t *testing.T) {
	so := filepath.Join(t.TempDir(), "nosgid.so") // never created
	t.Setenv("LD_PRELOAD", "/keep.so")

	apply(so, trustingView())

	if got := os.Getenv("LD_PRELOAD"); got != "/keep.so" {
		t.Fatalf("LD_PRELOAD = %q, want unchanged /keep.so", got)
	}
}

// #1487: each way the agent could substitute the shim's bytes must keep the
// shim OUT of LD_PRELOAD. A writable shim is code execution in every child.
func TestApply_RefusesSubstitutableShim(t *testing.T) {
	cases := map[string]func(so string) trustView{
		"file writable": func(so string) trustView {
			v := trustingView()
			v.canWrite = func(p string) bool { return p == so }
			return v
		},
		"parent dir writable (unlink+replace)": func(so string) trustView {
			v := trustingView()
			v.canWrite = func(p string) bool { return p == filepath.Dir(so) }
			return v
		},
		"ancestor dir writable (rename the dir away)": func(so string) trustView {
			v := trustingView()
			v.canWrite = func(p string) bool { return p == filepath.Dir(filepath.Dir(so)) }
			return v
		},
		"file owned by an untrusted uid (can chmod it back)": func(so string) trustView {
			v := trustingView()
			v.trustedOwner = func(uint32) bool { return false }
			return v
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			so := writeShim(t)
			t.Setenv("LD_PRELOAD", "/keep.so")

			apply(so, mk(so))

			if got := os.Getenv("LD_PRELOAD"); got != "/keep.so" {
				t.Fatalf("LD_PRELOAD = %q, want unchanged /keep.so (shim must be refused)", got)
			}
		})
	}
}

// A symlink in a protected location pointing at bytes in a writable one is
// not protected: the TARGET's directory must be checked too.
func TestApply_RefusesSymlinkIntoWritableDir(t *testing.T) {
	target := writeShim(t)
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "nosgid.so")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	realTargetDir, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LD_PRELOAD", "/keep.so")
	v := trustingView()
	v.canWrite = func(p string) bool { return p == realTargetDir }

	apply(link, v)

	if got := os.Getenv("LD_PRELOAD"); got != "/keep.so" {
		t.Fatalf("LD_PRELOAD = %q, want unchanged /keep.so (symlink target dir is writable)", got)
	}
}

// The live view must reject what the pre-#1487 install produced: a shim owned
// by the service user in a directory it can write. A temp file owned by the
// test user is exactly that shape (unless the suite runs as root).
func TestLiveView_RejectsUserOwnedShim(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a temp file is root-owned, so it is genuinely trusted")
	}
	so := writeShim(t)
	if err := checkTrusted(so, liveView()); err == nil {
		t.Fatalf("checkTrusted(%s) = nil; a user-owned shim in a user-writable dir must be rejected", so)
	}
}

// Path and the Makefile's install destination must agree, or install-lib
// writes a file foci never preloads (and the shim silently stops working).
func TestPathMatchesMakefileInstallDir(t *testing.T) {
	f, err := os.Open("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "NOSGID_LIB_DIR") {
			continue
		}
		_, val, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("unparseable Makefile line %q", line)
		}
		if got, want := strings.TrimSpace(val), filepath.Dir(Path); got != want {
			t.Fatalf("Makefile NOSGID_LIB_DIR = %q, but preload.Path lives in %q", got, want)
		}
		return
	}
	t.Fatal("NOSGID_LIB_DIR not found in Makefile")
}

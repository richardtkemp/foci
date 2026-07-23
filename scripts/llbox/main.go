// Command llbox seals the current process with a Landlock write-whitelist,
// then execs a command in it. Reads are never restricted — only write-class
// filesystem operations (open O_WRONLY/O_RDWR/O_CREAT, mkdir, unlink, rename,
// link, symlink, chmod, chown, truncate) are confined to the paths passed via
// -w; anything else the wrapped command tries to write is denied by the
// kernel (EACCES/EPERM), not by this tool.
//
// Usage:
//
//	llbox -w /a,/b,/c -- cmd args...
//
// Gracefully degrades: on a kernel/environment that doesn't support Landlock
// (too old, the "landlock" LSM disabled at boot, or a non-Linux OS), llbox
// prints a single warning line to stderr and execs the command UNSEALED
// rather than failing — sealing is defense in depth for `make test`, not a
// hard requirement to run it at all. By contrast, once Landlock is confirmed
// available, any LATER failure (a whitelist path that doesn't exist, a
// rejected rule, restrict_self failing) is treated as fatal: silently
// running unsealed in that case would defeat the whole point, so llbox exits
// nonzero instead of falling back.
//
// Originally prototyped as a throwaway POC (foci_todo #1517); promoted here
// as a proper dependency-free repo tool per #1523, alongside the other
// repo-local Go tools under scripts/ (find-disconnected-tests etc).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func main() {
	var writable string
	flag.StringVar(&writable, "w", "", "comma-separated writable paths")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: llbox -w /a,/b,/c -- cmd args...")
		fmt.Fprintln(os.Stderr, "\nSeals the process with a Landlock write-whitelist (degrading")
		fmt.Fprintln(os.Stderr, "gracefully to unsealed when Landlock is unavailable), then execs")
		fmt.Fprintln(os.Stderr, "the given command inside it. Reads are always unrestricted.")
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	var paths []string
	for _, p := range strings.Split(writable, ",") {
		if p != "" {
			paths = append(paths, p)
		}
	}

	supported, err := seal(paths)
	switch {
	case !supported:
		fmt.Fprintf(os.Stderr, "llbox: WARNING: Landlock unavailable (%v) — running UNSEALED\n", err)
	case err != nil:
		fmt.Fprintf(os.Stderr, "llbox: FATAL: Landlock is available but sealing setup failed: %v\n", err)
		os.Exit(1)
	}

	cmd := exec.Command(flag.Arg(0), flag.Args()[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "llbox: exec: %v\n", err)
		os.Exit(1)
	}
}

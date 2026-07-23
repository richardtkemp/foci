//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	landlockRuleTypePathBeneath = 1
	// landlockCreateRulesetVersion, passed as the `flags` argument, makes
	// landlock_create_ruleset() a pure ABI-version probe: attr is ignored,
	// size must be 0, and the return value is the supported ABI version (or
	// -1/ENOSYS on a kernel with no Landlock support, or with it disabled at
	// boot via the "landlock" LSM parameter).
	landlockCreateRulesetVersion = 1 << 0

	prSetNoNewPrivs = 38
	unixOPath       = 0x200000
)

// write-class access bits (ABI1 + REFER from ABI2 + TRUNCATE from ABI3).
//
// REFER (bit 13) is REQUIRED for rename()/link() that moves a path between
// two different directories, even when both are already whitelisted —
// without it the kernel refuses the cross-directory move with EXDEV
// ("invalid cross-device link"), a misleading error that looks like a real
// cross-filesystem problem but is actually this Landlock ABI2 quirk.
// (foci_todo #1517: broke TestMigrateLegacyLayout, a same-tree os.Rename one
// directory up, until this bit was added.)
const writeSet uint64 = (1 << 1) | (1 << 4) | (1 << 5) | (1 << 6) | (1 << 7) |
	(1 << 8) | (1 << 9) | (1 << 10) | (1 << 11) | (1 << 12) | (1 << 13) | (1 << 14)

// fileWriteSet is the subset of writeSet valid against a non-directory
// anchor (a regular file or device node, e.g. /dev/null): WRITE_FILE(1) and
// TRUNCATE(14) only. landlock_add_rule() returns EINVAL if any
// directory-only right (MAKE_*, REMOVE_*, REFER, READ_DIR) is requested
// against a non-directory path_beneath anchor.
const fileWriteSet uint64 = (1 << 1) | (1 << 14)

// seal attempts to confine the current process (and everything it execs
// from here on) to writing only beneath paths. It returns (false, err) when
// Landlock itself is unavailable — the caller should degrade gracefully in
// that case — and (true, err) when Landlock is available but something in
// setup failed (a whitelist path missing, a rejected rule, ...), which the
// caller should treat as fatal rather than silently running unsealed.
func seal(paths []string) (supported bool, err error) {
	// Pure version probe first (attr=NULL, size=0) so an unsupported kernel
	// is diagnosed on its own, before we build a real ruleset.
	if _, _, errno := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion); errno != 0 {
		return false, errno
	}

	attr := make([]byte, 8) // ruleset_attr{ u64 handled_access_fs } — size 8 selects the ABI1 layout.
	binary.LittleEndian.PutUint64(attr, writeSet)
	rs, _, errno := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr[0])), 8, 0)
	if errno != 0 {
		return false, fmt.Errorf("create_ruleset: %w", errno)
	}

	for _, p := range paths {
		fd, err := syscall.Open(p, unixOPath|syscall.O_CLOEXEC, 0)
		if err != nil {
			return true, fmt.Errorf("open %s: %w", p, err)
		}
		access := writeSet
		if fi, statErr := os.Stat(p); statErr == nil && !fi.IsDir() {
			access = fileWriteSet
		}
		// path_beneath_attr{ u64 allowed_access; s32 parent_fd } PACKED = 12 bytes.
		pb := make([]byte, 12)
		binary.LittleEndian.PutUint64(pb[0:], access)
		binary.LittleEndian.PutUint32(pb[8:], uint32(fd))
		_, _, addErrno := syscall.Syscall6(sysLandlockAddRule, rs, landlockRuleTypePathBeneath,
			uintptr(unsafe.Pointer(&pb[0])), 0, 0, 0)
		syscall.Close(fd)
		if addErrno != 0 {
			return true, fmt.Errorf("add_rule %s: %w", p, addErrno)
		}
	}

	// NO_NEW_PRIVS is required by landlock_restrict_self() for an
	// unprivileged (non-CAP_SYS_ADMIN) caller.
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		return true, fmt.Errorf("prctl(NO_NEW_PRIVS): %w", errno)
	}
	if _, _, errno := syscall.Syscall(sysLandlockRestrictSelf, rs, 0, 0); errno != 0 {
		return true, fmt.Errorf("restrict_self: %w", errno)
	}
	return true, nil
}

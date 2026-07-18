/*
 * nosgid.so — an LD_PRELOAD shim that strips the set-user-ID / set-group-ID
 * bits from the chmod family of libc calls before they reach the kernel.
 *
 * Why this exists
 * ---------------
 * foci-gw runs under a systemd unit hardened with RestrictSUIDSGID=yes (plus
 * NoNewPrivileges=yes and no CAP_FSETID). That directive turns any chmod which
 * *sets* S_ISUID or S_ISGID into a hard EPERM. Ordinary unprivileged POSIX
 * behaviour is the opposite: silently drop those bits and return success.
 *
 * Build tools an agent runs (npm / astro / rollup / esbuild, …) routinely
 * normalise permissions across a tree. When that tree contains setgid
 * directories (inherited from a setgid parent), the tool re-applies a mode
 * like 02775 and gets EPERM under the hardened service, aborting the build.
 *
 * This shim restores the gentle "drop the bit, succeed" behaviour for every
 * child of foci-gw (shell-tool subprocesses and delegated backends), without
 * weakening the RestrictSUIDSGID hardening on foci-gw itself. It only ever
 * clears 06000 (S_ISUID|S_ISGID); it never grants a permission a process was
 * not already allowed to set, and has no effect on the ordinary rwx bits.
 *
 * Built to bin/nosgid.so by the Makefile and installed to
 * $(FOCI_HOME)/.lib/nosgid.so; injected into LD_PRELOAD at startup by
 * internal/preload.
 */
#define _GNU_SOURCE
#include <sys/stat.h>
#include <dlfcn.h>
#include <stddef.h>

#define STRIP_MODE (S_ISUID | S_ISGID) /* 06000 */

int chmod(const char *path, mode_t mode)
{
	static int (*real)(const char *, mode_t) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "chmod");
	return real(path, mode & ~STRIP_MODE);
}

int fchmod(int fd, mode_t mode)
{
	static int (*real)(int, mode_t) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "fchmod");
	return real(fd, mode & ~STRIP_MODE);
}

int fchmodat(int dirfd, const char *path, mode_t mode, int flags)
{
	static int (*real)(int, const char *, mode_t, int) = NULL;
	if (!real)
		real = dlsym(RTLD_NEXT, "fchmodat");
	return real(dirfd, path, mode & ~STRIP_MODE, flags);
}

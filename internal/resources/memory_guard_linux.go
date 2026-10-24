//go:build linux

package resources

// Linux reads everything from procfs: /proc/meminfo, /proc/<pid>/status and
// the PSI file /proc/pressure/memory.

func platformMemTotal() (int64, error)       { return readMemTotal(defaultProcDir) }
func platformUserRSS(uid int) (int64, error) { return readUserRSS(defaultProcDir, uid) }
func platformSelfRSS() (int64, error)        { return readSelfRSS(defaultProcDir) }
func platformPressure() (float64, error)     { return readMemoryPressure(defaultProcDir) }
func platformFindLargest(uid, selfPid int) (int, string, int64, error) {
	return findLargestProcess(defaultProcDir, uid, selfPid)
}

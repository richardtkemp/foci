//go:build !linux && !darwin

package resources

import "errors"

// errNoMemorySource is returned on platforms with no implemented source. The
// guard logs it once per tick at Warn (memtotal) and takes no action.
var errNoMemorySource = errors.New("memory guard: no memory source on this platform")

func platformMemTotal() (int64, error)   { return 0, errNoMemorySource }
func platformUserRSS(int) (int64, error) { return 0, errNoMemorySource }
func platformSelfRSS() (int64, error)    { return 0, errNoMemorySource }
func platformPressure() (float64, error) { return 0, errNoMemorySource }
func platformFindLargest(int, int) (int, string, int64, error) {
	return 0, "", 0, errNoMemorySource
}

//go:build linux

package pig

import "golang.org/x/sys/unix"

// birth is the birth time of the file fd names, in nanoseconds, as statx
// gives it; 0 where the file system keeps none.
func birth(fd int) uint64 {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &stx); err != nil || stx.Mask&unix.STATX_BTIME == 0 {
		return 0
	}
	return uint64(stx.Btime.Sec)*1e9 + uint64(stx.Btime.Nsec) //nolint:gosec // a birth time is after 1970
}

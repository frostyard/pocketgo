//go:build unix && !linux

package uploads

import "golang.org/x/sys/unix"

// mountOf is the file system of the open file fd.
func mountOf(fd int) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil //nolint:unconvert // Dev's type differs by system
}

//go:build unix && !linux

package uploads

// mountOf is the file system of the open file fd.
func mountOf(fd int) (uint64, error) { return deviceID(fd) }

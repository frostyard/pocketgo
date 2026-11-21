//go:build linux

package uploads

import "golang.org/x/sys/unix"

// mountOf is the mount of the open file fd: two bind mounts of one file
// system share their device, yet a link from one to the other fails, so
// Linux tells them apart by their mount (Codex review of #49, round 10).
// A kernel that gives no mount's id gives the file system's.
func mountOf(fd int) (uint64, error) {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stx); err == nil && stx.Mask&unix.STATX_MNT_ID != 0 {
		return stx.Mnt_id, nil
	}
	return deviceID(fd)
}

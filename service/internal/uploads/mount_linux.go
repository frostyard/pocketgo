//go:build linux

package uploads

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// mountOf is the mount of the open file fd: two bind mounts of one file
// system share their device, yet a link from one to the other fails, so
// Linux tells them apart by their mount (Codex review of #49, round 10).
// A kernel whose statx gives no mount's id gives it in the descriptor's
// fdinfo; one that gives neither is refused, never taken for one mount
// (Codex review of #49, round 11).
func mountOf(fd int) (uint64, error) {
	if id, ok := statxMount(fd); ok {
		return id, nil
	}
	info, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return 0, fmt.Errorf("the mount of a folder: %w", err)
	}
	for line := range strings.SplitSeq(string(info), "\n") {
		if v, ok := strings.CutPrefix(line, "mnt_id:"); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, errors.New("the system says no folder's mount")
}

// statxMount is the mount id statx gives of fd; tests set it.
var statxMount = func(fd int) (uint64, bool) {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &stx); err != nil || stx.Mask&unix.STATX_MNT_ID == 0 {
		return 0, false
	}
	return stx.Mnt_id, true
}

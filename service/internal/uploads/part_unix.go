//go:build unix

package uploads

import (
	"os"

	"golang.org/x/sys/unix"
)

// createPart makes a part file, ".part-<random>", in folder dir, where no
// entry is: where the host makes no unnamed file, the upload is visible
// there until Link, and a stop of pocketd may leave it.
func createPart(dir int, random func() string) (*os.File, string, error) {
	part := ".part-" + random() + random()
	fd, err := unix.Openat(dir, part, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), part), part, nil
}

// linkPart names part file part, of folder dir, name, where no entry is,
// and drops its part name.
func linkPart(dir int, part, name string) error {
	if err := unix.Linkat(dir, part, dir, name, 0); err != nil {
		return err
	}
	return unix.Unlinkat(dir, part, 0)
}

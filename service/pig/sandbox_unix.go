//go:build unix

package pig

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// writable reports whether this process may change dir's entries: create,
// rename or remove them.
func writable(dir string) bool {
	return syscall.Access(dir, 0o2) == nil // W_OK
}

// rootOwned checks that only root can change path: it and every folder
// above it belong to root and are writable by nobody else.
func rootOwned(path string) error {
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: no owner information", p)
		}
		if st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s can be changed by a user other than root", p)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

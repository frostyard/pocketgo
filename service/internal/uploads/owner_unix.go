//go:build unix

package uploads

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedBySelf says the service user owns the entry info describes.
func ownedBySelf(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

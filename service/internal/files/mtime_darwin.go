//go:build darwin

package files

import (
	"golang.org/x/sys/unix"
	"time"
)

// mtime reads the modification time from a Darwin stat.
func mtime(st *unix.Stat_t) time.Time { return time.Unix(st.Mtim.Unix()) }

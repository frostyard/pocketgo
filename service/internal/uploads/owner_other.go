//go:build !unix

package uploads

import "io/fs"

// ownedBySelf holds where the system names no owner pocketd can check.
func ownedBySelf(fs.FileInfo) bool { return true }

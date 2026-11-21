//go:build !unix

package state

import "os"

// openUploadFile opens a file to upload; FileSource then refuses anything
// but a regular file.
func openUploadFile(path string) (*os.File, error) { return os.Open(path) }

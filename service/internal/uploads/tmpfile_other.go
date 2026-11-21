//go:build unix && !linux

package uploads

import "os"

// createUnnamed makes a part file in folder dir: this system makes no
// unnamed file.
func createUnnamed(dir int, random func() string) (*os.File, string, error) {
	return createPart(dir, random)
}

// linkUnnamed names file's part name, name.
func linkUnnamed(dir int, _ *os.File, part, name string) error { return linkPart(dir, part, name) }

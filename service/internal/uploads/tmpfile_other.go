//go:build unix && !linux

package uploads

import (
	"errors"
	"os"
)

// errNoUnnamed is this system, which makes no unnamed file: the file is
// staged in the staging folder instead.
var errNoUnnamed = errors.New("no unnamed file here")

// createUnnamed makes no file here.
func createUnnamed(int) (*os.File, error) { return nil, errNoUnnamed }

// linkUnnamed names no file here.
func linkUnnamed(int, *os.File, string) error { return errNoUnnamed }

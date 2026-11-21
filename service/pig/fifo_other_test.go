//go:build !unix

package pig

import (
	"errors"
	"os"
)

func mkfifo(string) error { return errors.New("no named pipes") }

// nonblocking cannot tell here.
func nonblocking(*os.File) (bool, error) { return false, errors.New("no descriptor flags here") }

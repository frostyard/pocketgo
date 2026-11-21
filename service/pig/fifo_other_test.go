//go:build !unix

package pig

import "errors"

func mkfifo(string) error { return errors.New("no named pipes") }

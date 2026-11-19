//go:build !unix

package profiles

import "errors"

func mkfifo(string) error { return errors.New("no named pipes") }

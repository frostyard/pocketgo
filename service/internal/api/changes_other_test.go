//go:build !unix

package api

import (
	"context"
	"errors"
)

// DirID and ReadIn read nothing where there is no sandbox: commands, and
// the reads that run as commands, run on Linux only (ADR-0012).
func (f *fakeEngine) DirID(context.Context, string) (string, bool, error) {
	return "", false, errors.New("no sandbox here")
}

func (f *fakeEngine) ReadIn(context.Context, string, string, string, int) (string, []byte, error) {
	return "", nil, errors.New("no sandbox here")
}

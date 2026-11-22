//go:build !unix

package api

import (
	"context"
	"errors"
)

// ReadIn reads nothing where there is no sandbox: commands, and the reads
// that run as commands, run on Linux only (ADR-0012).
func (f *fakeEngine) ReadIn(context.Context, string, string, int) (string, []byte, error) {
	return "", nil, errors.New("no sandbox here")
}

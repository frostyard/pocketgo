//go:build !unix

package api

import (
	"context"
	"errors"
	"github.com/frostyard/pocketgo/service/internal/files"
)

// DirID and ReadIn read nothing where there is no sandbox: commands, and
// the reads that run as commands, run on Linux only (ADR-0012).
func (f *fakeEngine) DirID(context.Context, string) (string, bool, error) {
	return "", false, errors.New("no sandbox here")
}

func (f *fakeEngine) ReadIn(context.Context, string, string, string, int) (string, []byte, error) {
	return "", nil, errors.New("no sandbox here")
}

// ListFiles lists a fixture outside any sandbox, as only a test does.
func (f *fakeEngine) ListFiles(_ context.Context, root, rel string, maxEntries, maxNames int) (files.Folder, error) {
	return files.Folder{}, errors.New("no sandbox here")
}

// ReadFile reads a fixture outside any sandbox, as only a test does.
func (f *fakeEngine) ReadFile(_ context.Context, root, rel string, max int64) (files.File, error) {
	return files.File{}, errors.New("no sandbox here")
}

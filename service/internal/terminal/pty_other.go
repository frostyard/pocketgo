//go:build !linux

package terminal

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
)

// pocketd runs shells on Linux only: it runs on the team VM.
const (
	hangUp = syscall.Signal(1)
	kill   = syscall.Signal(9)
)

type shell struct{}

func startShell(path, dir, home string, env []string, rows, cols int) (*shell, error) {
	return nil, fmt.Errorf("terminals on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

func (*shell) read([]byte) (int, error)  { return 0, errors.ErrUnsupported }
func (*shell) write([]byte) (int, error) { return 0, errors.ErrUnsupported }
func (*shell) resize(int, int) error     { return errors.ErrUnsupported }
func (*shell) signal(syscall.Signal)     {}
func (*shell) wait() *int                { return nil }
func (*shell) close()                    {}

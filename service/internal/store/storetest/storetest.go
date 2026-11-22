// Package storetest gives each test its own database file, byte for byte what
// store.Open makes on a fresh file. Migrating once per test process rather than
// once per test speeds up fixtures. Only tests import this package.
package storetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/service/internal/store"
)

var template = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "storetest-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "w.sqlite")
	s, err := store.Open(context.Background(), path, nil)
	if err != nil {
		return nil, err
	}
	if err := s.Close(); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path + "-wal"); err == nil {
		if info.Size() > 0 {
			return nil, fmt.Errorf("template has uncheckpointed WAL: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return os.ReadFile(path)
})

// Template returns the migrated database bytes, built once per process.
func Template() ([]byte, error) { return template() }

func writeTemplate(path string) error {
	data, err := Template()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Open copies the template into a new file and opens it with the given clock.
// The caller owns closing the returned store.
func Open(t testing.TB, path string, now func() time.Time) *store.Store {
	t.Helper()
	if err := writeTemplate(path); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), path, now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

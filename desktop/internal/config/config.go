// Package config keeps what the desktop client remembers between runs: the
// pocketd it signs in to and the window's layout, in a settings file, and
// the device token, in the desktop's keyring (Credentials). Ported from
// frostyard/pocket's internal/config (ADR-0010).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AppID names the app for the desktop, its windows and the keyring. It is
// not frostyard/pocket's, so the two clients run side by side.
const AppID = "org.frostyard.PocketGo"

// Settings is the file the client keeps in the user's config folder.
type Settings struct {
	// Server is the service URL of the pocketd signed in to, as
	// state.ParseServer returns it: the app signs in to it at start with
	// the kept token. Signing out clears it before it forgets the token, so
	// a token the keyring failed to delete is never used again.
	Server string `json:"server,omitempty"`
	// LastServer is the service URL the sign-in form offers: the last one
	// signed in to, also after signing out.
	LastServer string `json:"lastServer,omitempty"`
	// Widths and Collapsed are the saved layout of the window's panes.
	Widths    map[string]float32 `json:"widths,omitempty"`
	Collapsed map[string]bool    `json:"collapsed,omitempty"`
	// LastSession is the session shown when the window last closed.
	LastSession int64 `json:"lastSession,omitempty"`
	// Seen are the owner's seen marks (state.Seen), by the service URL
	// they were made on, since a session's id is its service's: for each
	// session, the updatedAt it had when the owner last had it open. The
	// sidebar shows NEW on a session updated since.
	Seen map[string]map[int64]time.Time `json:"seen,omitempty"`
	// Pinned are the sessions the owner pinned (state.Pins), by the
	// service URL they were pinned on, each list in the order pinned. The
	// sidebar lists them first, after the places.
	Pinned map[string][]int64 `json:"pinned,omitempty"`
	// Zoom scales everything in the window, as a browser zooms a page:
	// 1.25 for 125%. Zero is 100% (state.Zoom).
	Zoom float32 `json:"zoom,omitempty"`
	// Notify are the owner's notification switches (state.NotifyPrefs),
	// by the service URL they were set for: a service without an entry
	// has every notification on (ADR-0049).
	Notify map[string]Notify `json:"notify,omitempty"`
	// Notified are the keys of what waited for the owner that this
	// desktop has seen (state.Options.KeepNotices), by service URL, oldest
	// first: each thing notifies once, also across runs (ADR-0049).
	Notified map[string][]string `json:"notified,omitempty"`
}

// Notify are the notification switches for one service, as
// state.NotifyPrefs has them: each one off when set, so a service the owner
// set nothing for has them all on.
type Notify struct {
	Off          bool `json:"off,omitempty"`
	AsksOff      bool `json:"asksOff,omitempty"`
	ApprovalsOff bool `json:"approvalsOff,omitempty"`
	ReviewsOff   bool `json:"reviewsOff,omitempty"`
	RunsOff      bool `json:"runsOff,omitempty"`
}

// Store reads and writes Settings in a folder.
type Store struct {
	mu  sync.Mutex
	dir string
}

// Open returns the store in the user's config folder ("pocketgo" in
// os.UserConfigDir), or in dir when it is not empty (POCKETGO_CONFIG_DIR,
// for development and tests).
func Open(dir string) (*Store, error) {
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("finding the config folder: %w", err)
		}
		dir = filepath.Join(base, "pocketgo")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir is the store's folder.
func (s *Store) Dir() string { return s.dir }

func (s *Store) file() string { return filepath.Join(s.dir, "settings.json") }

// Load reads the settings; a missing file is empty settings.
func (s *Store) Load() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st Settings
	b, err := os.ReadFile(s.file())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return Settings{}, fmt.Errorf("reading %s: %w", s.file(), err)
	}
	return st, nil
}

// Save writes the settings, atomically, readable by the user only.
func (s *Store) Save(st Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file())
}

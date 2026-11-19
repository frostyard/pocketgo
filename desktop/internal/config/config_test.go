package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

var errMissing = errors.New("no such secret")

// ring is a Keyring in memory; err makes every call fail.
type ring struct {
	secrets map[string][]byte
	err     error
}

func (r *ring) Set(key string, value []byte) error {
	if r.err != nil {
		return r.err
	}
	r.secrets[key] = value
	return nil
}

func (r *ring) Get(key string) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	v, ok := r.secrets[key]
	if !ok {
		return nil, errMissing
	}
	return v, nil
}

func (r *ring) Delete(key string) error {
	if r.err != nil {
		return r.err
	}
	delete(r.secrets, key)
	return nil
}

const server = "https://pocketd.example.ts.net:8443"

func tokenFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "token-*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestTheTokenGoesToTheKeyringAndNoFile(t *testing.T) {
	dir := t.TempDir()
	r := &ring{secrets: map[string][]byte{}}
	c := Credentials{Ring: r, NotFound: errMissing, Dir: dir}
	if tok, err := c.Token(server); err != nil || tok != "" {
		t.Fatalf("no token yet: %q, %v", tok, err)
	}
	// A file from a time without a keyring goes once the keyring works.
	if err := os.WriteFile(c.file(server), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.SetToken(server, "pgd_secret"); err != nil {
		t.Fatal(err)
	}
	if got := string(r.secrets["token:"+server]); got != "pgd_secret" {
		t.Errorf("keyring holds %q", got)
	}
	if files := tokenFiles(t, dir); len(files) != 0 {
		t.Errorf("token files %v with a working keyring", files)
	}
	if tok, err := c.Token(server + "/"); err != nil || tok != "pgd_secret" {
		t.Errorf("Token = %q, %v", tok, err)
	}
	if err := c.Forget(server); err != nil {
		t.Fatal(err)
	}
	if tok, _ := c.Token(server); tok != "" {
		t.Errorf("a forgotten token is still %q", tok)
	}
}

func TestWithoutAKeyringTheTokenGoesToAPrivateFile(t *testing.T) {
	dir := t.TempDir()
	c := Credentials{Ring: &ring{err: errors.ErrUnsupported}, NotFound: errMissing, Dir: dir}
	if err := c.SetToken(server, "pgd_secret"); err != nil {
		t.Fatal(err)
	}
	files := tokenFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("token files %v", files)
	}
	if info, err := os.Stat(files[0]); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("the token file is %v, %v; want mode 0600", info.Mode(), err)
	}
	if tok, err := c.Token(server); err != nil || tok != "pgd_secret" {
		t.Errorf("Token = %q, %v", tok, err)
	}
	if err := c.Forget(server); err != nil || len(tokenFiles(t, dir)) != 0 {
		t.Errorf("Forget left %v (%v)", tokenFiles(t, dir), err)
	}
}

// TestAWorkingKeyringWithoutTheTokenMeansNone leaves a token file from a time
// without a keyring: a keyring that works and lacks the token wins.
func TestAWorkingKeyringWithoutTheTokenMeansNone(t *testing.T) {
	dir := t.TempDir()
	c := Credentials{Ring: &ring{secrets: map[string][]byte{}}, NotFound: errMissing, Dir: dir}
	if err := os.WriteFile(c.file(server), []byte("pgd_stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := c.Token(server); err != nil || tok != "" {
		t.Errorf("Token = %q, %v; want none: the keyring has no token", tok, err)
	}
}

// TestForgetSaysWhenTheKeyringKeepsTheToken has a keyring refuse to delete:
// signing out must not look done.
func TestForgetSaysWhenTheKeyringKeepsTheToken(t *testing.T) {
	locked := errors.New("the keyring is locked")
	r := &ring{secrets: map[string][]byte{"token:" + server: []byte("pgd_secret")}}
	c := Credentials{Ring: r, NotFound: errMissing, Dir: t.TempDir()}
	r.err = locked
	if err := c.Forget(server); !errors.Is(err, locked) {
		t.Errorf("Forget = %v, want the keyring's error", err)
	}
	r.err = nil
	if err := c.Forget(server); err != nil {
		t.Errorf("Forget = %v after the keyring unlocked", err)
	}
	// Nothing to forget is not an error, with a keyring or without one.
	r.err = errMissing
	if err := c.Forget(server); err != nil {
		t.Errorf("Forget of a missing token = %v", err)
	}
	if err := (Credentials{Ring: &ring{err: errors.ErrUnsupported}, Dir: t.TempDir()}).Forget(server); err != nil {
		t.Errorf("Forget without a keyring = %v", err)
	}
}

func TestAFailingKeyringNeverFallsBackToAFile(t *testing.T) {
	dir := t.TempDir()
	locked := errors.New("the keyring is locked")
	c := Credentials{Ring: &ring{err: locked}, NotFound: errMissing, Dir: dir}
	if err := c.SetToken(server, "pgd_secret"); !errors.Is(err, locked) {
		t.Errorf("SetToken = %v, want the keyring's error", err)
	}
	if files := tokenFiles(t, dir); len(files) != 0 {
		t.Errorf("a locked keyring wrote the token to %v", files)
	}
	if _, err := c.Token(server); !errors.Is(err, locked) {
		t.Errorf("Token = %v, want the keyring's error", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if st, err := s.Load(); err != nil || st.Server != "" {
		t.Fatalf("empty settings: %+v, %v", st, err)
	}
	want := Settings{Server: server, LastServer: server, Widths: map[string]float32{"sidebar": 300}, Collapsed: map[string]bool{"sidebar": true}, LastSession: 7}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.Server != want.Server || got.LastServer != server || got.Widths["sidebar"] != 300 || !got.Collapsed["sidebar"] || got.LastSession != 7 {
		t.Errorf("Load = %+v, %v", got, err)
	}
	if err := os.WriteFile(s.file(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Error("a broken file loaded")
	}
}

package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/github"
)

var errNoKey = errors.New("no such secret")

// fakeRing is a keyring in memory; fail makes every call fail with it, and
// setFail, getFail and deleteFail fail 1 kind of call.
type fakeRing struct {
	m          map[string][]byte
	fail       error
	setFail    error
	getFail    error
	deleteFail error
}

func (f *fakeRing) Set(key string, value []byte) error {
	if err := firstErr(f.fail, f.setFail); err != nil {
		return err
	}
	f.m[key] = value
	return nil
}

func (f *fakeRing) Get(key string) ([]byte, error) {
	if err := firstErr(f.fail, f.getFail); err != nil {
		return nil, err
	}
	v, ok := f.m[key]
	if !ok {
		return nil, errNoKey
	}
	return v, nil
}

func (f *fakeRing) Delete(key string) error {
	if err := firstErr(f.fail, f.deleteFail); err != nil {
		return err
	}
	if _, ok := f.m[key]; !ok {
		return errNoKey
	}
	delete(f.m, key)
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// noFiles runs the test in empty folders and fails if any file appears.
func noFiles(t *testing.T) {
	t.Helper()
	dir, home := t.TempDir(), t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home)
	t.Cleanup(func() {
		for _, d := range []string{dir, home} {
			if names, _ := os.ReadDir(d); len(names) > 0 {
				t.Errorf("a file was written in %s: %v", d, names[0].Name())
			}
		}
	})
}

// token is a sign-in, as the sign-in of the app has it.
func token() github.AppToken {
	return github.AppToken{
		ClientID:       github.AppClientID,
		Login:          github.Login,
		Access:         "ghu_ACCESS0000",
		Refresh:        "ghr_REFRESH0000",
		AccessExpires:  time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC),
		RefreshExpires: time.Date(2027, 5, 13, 12, 0, 0, 0, time.UTC),
	}
}

func TestGitHubApp(t *testing.T) {
	noFiles(t)
	ring := &fakeRing{m: map[string][]byte{}}
	g := GitHubApp{Ring: ring, NotFound: errNoKey}

	if _, err := g.Load(); !errors.Is(err, ErrNoSignIn) {
		t.Fatalf("load with no entry: %v", err)
	}
	want := token()
	if err := g.Save(want); err != nil {
		t.Fatal(err)
	}
	// 1 entry, keyed github:app, JSON in the documented shape.
	b, ok := ring.m["github:app"]
	if !ok || len(ring.m) != 1 {
		t.Fatalf("keyring %v", ring.m)
	}
	var stored map[string]any
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "client_id", "login", "access", "access_expires", "refresh", "refresh_expires"} {
		if _, ok := stored[k]; !ok {
			t.Errorf("the entry has no %q: %s", k, b)
		}
	}
	if stored["access_expires"] != "2026-11-14T20:00:00Z" || stored["refresh_expires"] != "2027-05-13T12:00:00Z" {
		t.Errorf("the times are %v and %v", stored["access_expires"], stored["refresh_expires"])
	}
	got, err := g.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != want.ClientID || got.Login != want.Login || got.Access != want.Access || got.Refresh != want.Refresh ||
		!got.AccessExpires.Equal(want.AccessExpires) || !got.RefreshExpires.Equal(want.RefreshExpires) {
		t.Errorf("load gives %+v, want %+v", got, want)
	}
	if err := g.Forget(); err != nil {
		t.Fatal(err)
	}
	if err := g.Forget(); err != nil {
		t.Fatalf("forget a missing entry: %v", err)
	}
	if _, err := g.Load(); !errors.Is(err, ErrNoSignIn) {
		t.Fatalf("load after forget: %v", err)
	}
}

// fixedExpiry is a nonzero expiry: no test here reads the clock (Codex
// review of #24, round 10).
var fixedExpiry = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

func TestGitHubAppSaveRefusesAnIncompleteToken(t *testing.T) {
	noFiles(t)
	ring := &fakeRing{m: map[string][]byte{}}
	g := GitHubApp{Ring: ring, NotFound: errNoKey}
	for name, tok := range map[string]github.AppToken{
		"no client_id":      {Login: github.Login, Access: "a", Refresh: "b", AccessExpires: fixedExpiry, RefreshExpires: fixedExpiry},
		"no login":          {ClientID: github.AppClientID, Access: "a", Refresh: "b", AccessExpires: fixedExpiry, RefreshExpires: fixedExpiry},
		"no access":         {ClientID: github.AppClientID, Login: github.Login, Refresh: "b", AccessExpires: fixedExpiry, RefreshExpires: fixedExpiry},
		"no refresh":        {ClientID: github.AppClientID, Login: github.Login, Access: "a", AccessExpires: fixedExpiry, RefreshExpires: fixedExpiry},
		"no access expiry":  {ClientID: github.AppClientID, Login: github.Login, Access: "a", Refresh: "b", RefreshExpires: fixedExpiry},
		"no refresh expiry": {ClientID: github.AppClientID, Login: github.Login, Access: "a", Refresh: "b", AccessExpires: fixedExpiry},
	} {
		t.Run(name, func(t *testing.T) {
			if err := g.Save(tok); err == nil {
				t.Fatal("an incomplete sign-in was saved")
			}
			if len(ring.m) != 0 {
				t.Fatalf("the keyring holds %v", ring.m)
			}
		})
	}
}

// TestGitHubAppBadSignIn: a value that is not a sign-in gives its reason.
func TestGitHubAppBadSignIn(t *testing.T) {
	entry := func(change func(map[string]any)) []byte {
		b, _ := json.Marshal(storedSignIn{
			V: 1, ClientID: github.AppClientID, Login: github.Login, Access: "ghu_A", Refresh: "ghr_R",
			AccessExpires: "2026-11-14T20:00:00Z", RefreshExpires: "2027-05-13T12:00:00Z",
		})
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if change != nil {
			change(m)
		}
		out, _ := json.Marshal(m)
		return out
	}
	for name, tc := range map[string]struct {
		value  []byte
		reason string
	}{
		"bad JSON":      {[]byte(`not json`), "the value does not parse"},
		"v 2":           {entry(func(m map[string]any) { m["v"] = 2 }), "the version is 2, not 1"},
		"empty field":   {entry(func(m map[string]any) { m["access"] = "" }), "the access is empty"},
		"bad time":      {entry(func(m map[string]any) { m["access_expires"] = "next week" }), "the access_expires time does not parse"},
		"zero time":     {entry(func(m map[string]any) { m["refresh_expires"] = "0001-01-01T00:00:00Z" }), "the refresh_expires time is zero"},
		"other build":   {entry(func(m map[string]any) { m["client_id"] = "Iv23liOTHER" }), "this sign-in is for another build of PocketGo"},
		"another login": {entry(func(m map[string]any) { m["login"] = "frostyard-machines" }), "this sign-in is for frostyard-machines, not bketelsen"},
	} {
		t.Run(name, func(t *testing.T) {
			noFiles(t)
			ring := &fakeRing{m: map[string][]byte{"github:app": tc.value}}
			_, err := GitHubApp{Ring: ring, NotFound: errNoKey}.Load()
			var bad *BadSignIn
			if !errors.As(err, &bad) {
				t.Fatalf("err %v", err)
			}
			if bad.Reason != tc.reason {
				t.Errorf("reason %q, want %q", bad.Reason, tc.reason)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("the text %q names no reason", err.Error())
			}
		})
	}
}

// TestGitHubAppNeedsAKeyring: no keyring, or 1 that fails, is an error and
// writes no file.
func TestGitHubAppNeedsAKeyring(t *testing.T) {
	noFiles(t)
	unsupported := errors.New("no keyring here")
	for name, g := range map[string]GitHubApp{
		"no keyring":  {NotFound: errNoKey},
		"unsupported": {Ring: &fakeRing{m: map[string][]byte{}, fail: unsupported}, NotFound: errNoKey},
		"locked":      {Ring: &fakeRing{m: map[string][]byte{}, fail: errors.New("the keyring is locked")}, NotFound: errNoKey},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := g.Load(); !errors.Is(err, errNoKeyring) && !strings.Contains(err.Error(), "keyring") {
				t.Errorf("load: %v", err)
			}
			if err := g.Save(token()); err == nil {
				t.Error("save: no error")
			}
			if err := g.Forget(); err == nil {
				t.Error("forget: no error")
			}
		})
	}
	// errors.ErrUnsupported says Approve + merge needs the desktop keyring.
	g := GitHubApp{Ring: &fakeRing{m: map[string][]byte{}, fail: errors.ErrUnsupported}, NotFound: errNoKey}
	if _, err := g.Load(); !errors.Is(err, errNoKeyring) {
		t.Errorf("load: %v", err)
	}
}

// TestGitHubAppFailingKeyringWritesNoFile: a keyring that refuses a write
// leaves no file behind.
func TestGitHubAppFailingKeyringWritesNoFile(t *testing.T) {
	noFiles(t)
	g := GitHubApp{Ring: &fakeRing{m: map[string][]byte{}, setFail: errors.New("the keyring is locked")}, NotFound: errNoKey}
	if err := g.Save(token()); err == nil {
		t.Fatal("save: no error")
	}
	if names, _ := os.ReadDir("."); len(names) != 0 {
		t.Errorf("a file was written: %v", names)
	}
}

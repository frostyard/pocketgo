//go:build unix && !darwin

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The lock's home comes from the user database alone: a user it does not
// name gets none, whatever $HOME says (Codex review of #24, round 3).
func TestPasswdHomeHasNoFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(path, []byte("root:x:0:0:root:/root:/bin/sh\nbk:x:1000:1000:BK:/var/home/bk:/bin/bash\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if home, err := passwdHome(path, 1000); err != nil || home != "/var/home/bk" {
		t.Fatalf("uid 1000: %q, %v", home, err)
	}
	t.Setenv("HOME", t.TempDir())
	if home, err := passwdHome(path, 4242); err == nil || !strings.Contains(err.Error(), "does not name user 4242") {
		t.Fatalf("an unknown uid: %q, %v", home, err)
	}
}

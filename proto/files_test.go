package proto

import (
	"strings"
	"testing"
)

// TestCheckFilePath covers cleaned paths and every refusal class.
func TestCheckFilePath(t *testing.T) {
	for _, tt := range []struct {
		name, in, want string
		bad            bool
	}{
		{"home", "", "", false}, {"dot", ".", "", false}, {"dot part", "a/./b", "a/b", false}, {"slashes", "a//b", "a/b", false}, {"parent inside", "a/../b", "b", false}, {"trailing", "a/", "a", false}, {"report", "reports/x/README.md", "reports/x/README.md", false},
		{"absolute", "/etc/passwd", "", true}, {"parent", "..", "", true}, {"outside", "../x", "", true}, {"escape", "a/../../x", "", true}, {"nul", "a\x00b", "", true}, {"long", strings.Repeat("a", 4097), "", true}, {"utf8", "\xff", "", true}, {"git", ".git/config", "", true}, {"env", "a/.env", "", true}, {"hidden", "notes/.hidden/x", "", true}, {"key", "keys/server.key", "", true}, {"token", "my-token.txt", "", true}, {"case", "Credentials.json", "", true}, {"identity", "id_ed25519.pub", "", true}, {"auth", "auth.json", "", true},
		{"maximum", strings.Repeat("a/", 2047) + "aa", strings.Repeat("a/", 2047) + "aa", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckFilePath(tt.in)
			if (err != nil) != tt.bad || got != tt.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

// TestHiddenName covers dot entries without hiding ordinary extensions.
func TestHiddenName(t *testing.T) {
	for _, name := range []string{".", ".git", ".env.local", "a.b", "README.md"} {
		t.Run(name, func(t *testing.T) {
			if HiddenName(name) != strings.HasPrefix(name, ".") {
				t.Fatal(name)
			}
		})
	}
}

// TestWithheldName exercises every configured secret pattern.
func TestWithheldName(t *testing.T) {
	yes := []string{"API_TOKEN", "server.KEY", "id_rsa", "netrc"}
	for _, s := range withheldParts {
		yes = append(yes, "my-"+s+".txt")
	}
	for _, s := range withheldSuffixes {
		yes = append(yes, "server"+s)
	}
	for _, s := range withheldPrefixes {
		yes = append(yes, s+".pub")
	}
	yes = append(yes, withheldNames...)
	for _, name := range yes {
		t.Run(name, func(t *testing.T) {
			if !WithheldName(name) {
				t.Fatal(name)
			}
		})
	}
	for _, name := range []string{"README.md", "notes.txt", "key.md", "tokyo.md"} {
		t.Run(name, func(t *testing.T) {
			if WithheldName(name) {
				t.Fatal(name)
			}
		})
	}
}
